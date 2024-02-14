package lsp

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	gsync "github.com/kralicky/gpkg/sync"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protols/pkg/format"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var analyzers = map[protocol.CodeActionKind][]Analyzer{
	protocol.RefactorRewrite: {
		simplifyRepeatedOptions,
		simplifyRepeatedFieldLiterals,
	},
	protocol.RefactorExtract: {
		extractFields,
	},
}

type pendingCodeAction struct {
	id           string
	cancel       context.CancelFunc
	cancelDelete func() bool
	resolve      func(*protocol.CodeAction) error
}

type pendingCodeActionManager struct {
	queue gsync.Map[string, *pendingCodeAction]
}

var actionQueue pendingCodeActionManager

func (q *pendingCodeActionManager) enqueue(title string, kind protocol.CodeActionKind, resolve func(*protocol.CodeAction) error) protocol.CodeAction {
	newId := uuid.NewString()
	ctx, ca := context.WithTimeout(context.Background(), 30*time.Second)
	cancelDelete := context.AfterFunc(ctx, func() {
		q.queue.Delete(newId)
	})
	q.queue.Store(newId, &pendingCodeAction{
		id:           newId,
		cancel:       ca,
		cancelDelete: cancelDelete,
		resolve:      resolve,
	})

	return protocol.CodeAction{
		Title: title,
		Kind:  kind,
		Data:  newId,
	}
}

func (q *pendingCodeActionManager) resolve(ca *protocol.CodeAction) error {
	id, ok := ca.Data.(string)
	if !ok {
		return fmt.Errorf("invalid code action data type: %T", ca.Data)
	}
	pca, ok := q.queue.LoadAndDelete(id)
	if !ok {
		return fmt.Errorf("no pending code action with id %q", ca.Data)
	}
	pca.cancelDelete()
	pca.cancel()
	return pca.resolve(ca)
}

func resolveCodeAction(ca *protocol.CodeAction) error {
	return actionQueue.resolve(ca)
}

func FindRefactorActions(ctx context.Context, request *protocol.CodeActionParams, linkRes linker.Result, mapper *protocol.Mapper, want map[protocol.CodeActionKind]bool) []protocol.CodeAction {
	var wg sync.WaitGroup
	resultsC := make(chan protocol.CodeAction)
	for kind, list := range analyzers {
		if !want[kind] {
			continue
		}
		for _, analyzer := range list {
			wg.Add(1)
			go func() {
				defer wg.Done()
				analyzer(ctx, request, linkRes, mapper, resultsC)
			}()
		}
	}
	go func() {
		wg.Wait()
		close(resultsC)
	}()
	results := make([]protocol.CodeAction, 0)
	for result := range resultsC {
		results = append(results, result)
	}
	return results
}

func RefactorUndeclaredName(ctx context.Context, cache *Cache, uri protocol.DocumentURI, name string, kind protocol.CodeActionKind) []protocol.CodeAction {
	linkRes, err := cache.FindResultOrPartialResultByURI(uri)
	if err != nil {
		return nil
	}
	filter := func(d protoreflect.Descriptor) bool {
		switch d := d.(type) {
		case protoreflect.MessageDescriptor:
			return !d.IsMapEntry() && d.ParentFile().Path() != linkRes.Path()
		case protoreflect.EnumDescriptor:
			return d.ParentFile().Path() != linkRes.Path()
		case protoreflect.ExtensionDescriptor:
			return d.ParentFile().Path() != linkRes.Path()
		}
		return false
	}
	var candidates []protoreflect.Descriptor
	if strings.Contains(name, ".") {
		candidates = cache.FindAllDescriptorsByQualifiedPrefix(ctx, name, filter).All()
	} else {
		candidates = cache.FindAllDescriptorsByQualifiedPrefix(ctx, string(linkRes.Package().Append(protoreflect.Name(name))), filter).All()
	}

	var matches []protoreflect.Descriptor
	for _, candidate := range candidates {
		baseName := name[strings.LastIndexByte(name, '.')+1:]
		if string(candidate.Name()) != baseName {
			continue
		}
		if strings.Contains(name, ".") {
			if strings.HasSuffix(string(candidate.FullName().Parent()), string(protoreflect.FullName(name).Parent())) {
				matches = append(matches, candidate)
			}
		} else {
			if candidate.Name() == protoreflect.Name(name) {
				matches = append(matches, candidate)
			}
		}
	}
	var actions []protocol.CodeAction
	for _, match := range matches {
		item := protocol.CodeAction{
			Title:       fmt.Sprintf("Import %q from %q", match.FullName(), match.ParentFile().Path()),
			Kind:        kind,
			IsPreferred: true,
			Edit: &protocol.WorkspaceEdit{
				Changes: map[protocol.DocumentURI][]protocol.TextEdit{
					uri: {
						editAddImport(linkRes, match.ParentFile().Path()),
					},
				},
			},
		}
		actions = append(actions, item)
	}
	return actions
}

type Analyzer func(ctx context.Context, request *protocol.CodeActionParams, linkRes linker.Result, mapper *protocol.Mapper, results chan<- protocol.CodeAction)

// simplifyRepeatedOptions transforms repeated options declared separately
// into a single option with a repeated value. For example:
//
// given an option 'repeated string repeated_strings', declarations of the form:
//
//	option (foo).string_list = "foo";
//	option (foo).string_list = "bar";
//
// become consolidated into a single declaration:
//
//	option (foo) = {
//	  string_list: ["foo", "bar"]
//	};
//
// or, if there already exists a declaration 'option (foo) = {...}' in the same
// scope, the new field is is added to the existing declaration:
//
//	option (foo) = {
//	  ...
//	  string_list: ["foo", "bar"];
//	};
func simplifyRepeatedOptions(ctx context.Context, request *protocol.CodeActionParams, linkRes linker.Result, mapper *protocol.Mapper, results chan<- protocol.CodeAction) {
	var tracker ast.AncestorTracker
	parentsToSearch := map[ast.Node][]ast.Node{}
	targetOptionNames := map[string]struct{}{}
	optionNodePathsByParent := make(map[ast.Node]map[string][][]ast.Node)

	walkOpts := tracker.AsWalkOptions()
	if request.Range != (protocol.Range{}) {
		a := linkRes.AST()
		startOff, endOff, _ := mapper.RangeOffsets(request.Range)
		startToken := a.TokenAtOffset(startOff)
		endToken := a.TokenAtOffset(endOff)
		walkOpts = append(walkOpts, ast.WithRange(startToken, endToken))
	}

	ast.Walk(linkRes.AST(), &ast.SimpleVisitor{
		DoVisitOptionNode: func(node *ast.OptionNode) error {
			if node.IsIncomplete() {
				return nil
			}
			name := format.StringForOptionName(node.Name)
			if _, ok := targetOptionNames[name]; ok {
				return nil
			}
			targetOptionNames[name] = struct{}{}
			parent := tracker.Parent()
			parentPath := slices.Clone(tracker.Path())
			parentsToSearch[parent] = parentPath[:len(parentPath)-2]
			optionNodePathsByParent[parent] = make(map[string][][]ast.Node)
			return nil
		},
	}, walkOpts...)

	for parent, path := range parentsToSearch {
		tracker := ast.AncestorTrackerFromPath(path)
		ast.Walk(parent, &ast.SimpleVisitor{
			DoVisitOptionNode: func(node *ast.OptionNode) error {
				if node.IsIncomplete() {
					return nil
				}
				parent := tracker.Parent()
				if _, ok := optionNodePathsByParent[parent]; !ok {
					return nil
				}
				name := format.StringForOptionName(node.Name)

				optionNodePathsByParent[parent][name] = append(optionNodePathsByParent[parent][name], slices.Clone(tracker.Path()))
				return nil
			},
		}, tracker.AsWalkOptions()...)
	}

	for parent, optionNodePathsByName := range optionNodePathsByParent {
		for targetOptionName := range targetOptionNames {
			optionNodePaths, ok := optionNodePathsByName[targetOptionName]
			if !ok || len(optionNodePaths) < 2 {
				continue
			}
			fileNode := linkRes.AST()

			var insert *ast.OptionNode
			var insertPos protocol.Position
			var remove []ast.Node

			switch parent := parent.(type) {
			case *ast.MessageNode:
				var insertIdx int
				insert, insertIdx, remove = buildSimplifyOptionsASTChanges(optionNodePathsByName, targetOptionName, parent.Decls)
				if insertIdx == -1 {
					continue
				}
				insertPos, _ = mapper.OffsetPosition(fileNode.NodeInfo(parent.Decls[insertIdx]).Start().Offset)
			case *ast.FileNode:
				var insertIdx int
				insert, insertIdx, remove = buildSimplifyOptionsASTChanges(optionNodePathsByName, targetOptionName, parent.Decls)
				if insertIdx == -1 {
					continue
				}
				insertPos, _ = mapper.OffsetPosition(fileNode.NodeInfo(parent.Decls[insertIdx]).Start().Offset)
			default:
				continue
			}

			insertEdit, err := formatNodeInsertEdit(fileNode, insert, insertPos)
			if err != nil {
				continue
			}
			removalEdits := formatNodeRemovalEdits(fileNode, remove, insertPos)

			action := protocol.CodeAction{
				Title: "Simplify repeated options",
				Kind:  protocol.RefactorRewrite,
				Edit: &protocol.WorkspaceEdit{
					Changes: map[protocol.DocumentURI][]protocol.TextEdit{
						mapper.URI: append(removalEdits, insertEdit),
					},
				},
			}
			results <- action
		}
	}
}

func newFieldVisitor(tracker *ast.AncestorTracker, paths *[][]ast.Node) ast.Visitor {
	return &ast.SimpleVisitor{
		DoVisitMessageDeclNode: visitEnclosingRange[ast.MessageDeclNode](tracker, paths),
		DoVisitFieldDeclNode:   visitEnclosingRange[ast.FieldDeclNode](tracker, paths),
	}
}

func extractFields(ctx context.Context, request *protocol.CodeActionParams, linkRes linker.Result, mapper *protocol.Mapper, results chan<- protocol.CodeAction) {
	if request.Range.Start == request.Range.End {
		return
	}
	fileNode := linkRes.AST()
	startOff, endOff, _ := mapper.RangeOffsets(request.Range)

	// if necessary, shrink the range until there is a valid token at the offset
	var startToken, endToken ast.Token
	for startOff < endOff {
		startToken = fileNode.TokenAtOffset(startOff)
		if startToken != ast.TokenError {
			break
		}
		startOff++
	}
	for endOff > startOff {
		endToken = fileNode.TokenAtOffset(endOff)
		if endToken != ast.TokenError {
			break
		}
		endOff--
	}
	if endOff-startOff < 1 {
		return
	}

	paths, ok := findPathsEnclosingRange(linkRes, startToken, endToken, newFieldVisitor)
	if !ok {
		return
	}

	var enclosedFields []*ast.FieldNode
	var parentNodePath []ast.Node

	for _, path := range paths {
		if fld, ok := path[len(path)-1].(*ast.FieldNode); ok {
			if fld.Start() < startToken || fld.End() > endToken {
				continue
			}
			if parentNodePath == nil {
				parentNodePath = path[:len(path)-1]
			} else if !slices.Equal(parentNodePath, path[:len(path)-1]) {
				return // fields in the range must share the same parent
			}
			enclosedFields = append(enclosedFields, fld)
		} else {
			return // the range must exclusively enclose message fields
		}
	}

	if len(enclosedFields) == 0 {
		return
	}

	desc, _, err := deepPathSearch(parentNodePath, linkRes, linkRes)
	if err != nil {
		return
	}
	switch desc := desc.(type) {
	case protoreflect.MessageDescriptor:
		msgNode, ok := parentNodePath[len(parentNodePath)-1].(*ast.MessageNode)
		if !ok {
			return
		}
		{
			openInfo := fileNode.NodeInfo(msgNode.OpenBrace)
			closeInfo := fileNode.NodeInfo(msgNode.CloseBrace)
			if startOff < openInfo.Start().Offset || endOff > closeInfo.End().Offset {
				return
			}
		}
		fieldDescs := make([]protoreflect.FieldDescriptor, len(enclosedFields))
		for i, fld := range enclosedFields {
			number, ok := ast.AsInt32(fld.Tag, 0, int32(protowire.MaxValidNumber))
			if !ok {
				return
			}
			fieldDescs[i] = desc.Fields().ByNumber(protowire.Number(number))
		}
		results <- actionQueue.enqueue("Extract fields into new message", protocol.RefactorExtract, func(ca *protocol.CodeAction) error {
			parentInfo := fileNode.NodeInfo(msgNode)
			parentRange := positionsToRange(parentInfo.Start(), fileNode.NodeInfo(msgNode.CloseBrace).End())
			endPos := parentInfo.End()
			if parentInfo.TrailingComments().Len() > 0 {
				endPos = parentInfo.TrailingComments().Index(parentInfo.TrailingComments().Len() - 1).End()
				endPos.Col++ // see Comment.End() doc
			}
			indentation := parentInfo.Start().Col - 1
			newMsgInsertPos := toPosition(endPos)

			updatedParentFields := make([]ast.MessageElement, 0, len(msgNode.Decls))
			insertedPlaceholder := false
			for _, decl := range msgNode.Decls {
				if fld, ok := decl.(*ast.FieldNode); ok && slices.Contains(enclosedFields, fld) {
					if !insertedPlaceholder {
						insertedPlaceholder = true
						updatedParentFields = append(updatedParentFields, nil)
					}
					continue
				}
				updatedParentFields = append(updatedParentFields, decl)
			}
			var newMsgFields []ast.MessageElement
			for i, fld := range enclosedFields {
				// the new field type may need to be updated
				fldDesc := fieldDescs[i]
				newFldType := fld.FldType
				if fldDesc.Kind() == protoreflect.MessageKind {
					relName := relativeFullName(fldDesc.Message().FullName(), desc.ParentFile().Package())
					if relName != string(fld.FldType.AsIdentifier()) {
						if strings.Contains(relName, ".") {
							compoundIdent := &ast.CompoundIdentNode{}
							parts := strings.Split(relName, ".")
							for i, part := range parts {
								compoundIdent.Components = append(compoundIdent.Components, &ast.IdentNode{Val: part})
								if i < len(parts)-1 {
									compoundIdent.Dots = append(compoundIdent.Dots, &ast.RuneNode{Rune: '.'})
								}
							}
							newFldType = compoundIdent
						} else {
							newFldType = &ast.IdentNode{Val: relName}
						}
					}
				}
				newFld := ast.NewFieldNode(fld.Label.KeywordNode, newFldType, fld.Name, fld.Equals, &ast.UintLiteralNode{Val: uint64(i + 1)}, fld.Options)
				newFld.AddSemicolon(fld.Semicolon)
				newMsgFields = append(newMsgFields, newFld)
			}
			newMsgName := findNewUnusedMessageName(desc)
			newFieldName := findNewUnusedFieldName(desc)
			newMessage := ast.NewMessageNode(&ast.KeywordNode{Val: "message"}, &ast.IdentNode{Val: newMsgName}, &ast.RuneNode{Rune: '{'}, newMsgFields, &ast.RuneNode{Rune: '}'})

			var label *ast.KeywordNode
			if isProto2(fileNode) {
				label = &ast.KeywordNode{Val: "optional"}
			}
			newParentMessageField := ast.NewFieldNode(label, &ast.IdentNode{Val: newMsgName}, &ast.IdentNode{Val: newFieldName}, &ast.RuneNode{Rune: '='}, &ast.UintLiteralNode{Val: enclosedFields[0].Tag.Val}, nil)
			newParentMessageField.AddSemicolon(&ast.RuneNode{Rune: ';'})
			for i, v := range updatedParentFields {
				if v == nil {
					updatedParentFields[i] = newParentMessageField
					break
				}
			}
			updatedParent := ast.NewMessageNode(msgNode.Keyword, msgNode.Name, msgNode.OpenBrace, updatedParentFields, msgNode.CloseBrace)

			updatedParentText, err := format.PrintNode(format.NodeInfoOverlay(fileNode, map[ast.Node]ast.NodeInfo{
				newMessage:            {},
				msgNode.Keyword:       {},
				newParentMessageField: {},
			}), updatedParent)
			if err != nil {
				return fmt.Errorf("error formatting updated parent message: %v", err)
			}

			newMessageText, err := format.PrintNode(format.NodeInfoOverlay(fileNode, map[ast.Node]ast.NodeInfo{
				newMessage: {},
			}), newMessage)
			if err != nil {
				return fmt.Errorf("error formatting new message: %v", err)
			}

			ca.Edit = &protocol.WorkspaceEdit{
				Changes: map[protocol.DocumentURI][]protocol.TextEdit{
					request.TextDocument.URI: {
						{
							Range:   parentRange,
							NewText: indentTextHanging(updatedParentText, int(parentRange.Start.Character)),
						},
						{
							Range:   protocol.Range{Start: newMsgInsertPos, End: newMsgInsertPos},
							NewText: fmt.Sprintf("\n\n%s", indentText(newMessageText, indentation)),
						},
					},
				},
			}
			return nil
		})
	}
}

func findNewUnusedMessageName(desc protoreflect.MessageDescriptor) string {
	parent := desc.Parent()
	prefix := "NewMessage"
	name := prefix
	if msgContainer, ok := parent.(interface {
		Messages() protoreflect.MessageDescriptors
	}); ok {
		for i := 1; i < 100; i++ {
			if msgContainer.Messages().ByName(protoreflect.Name(name)) == nil {
				return name
			}
			name = fmt.Sprintf("%s%d", prefix, i)
		}
	}
	return name
}

func findNewUnusedFieldName(desc protoreflect.MessageDescriptor) string {
	prefix := "newField"
	name := prefix
	for i := 1; i < 100; i++ {
		if desc.Fields().ByName(protoreflect.Name(name)) == nil {
			return name
		}
		name = fmt.Sprintf("%s%d", prefix, i)
	}
	return name
}

func indentTextHanging(text string, indentation int) string {
	return strings.ReplaceAll(text, "\n", "\n"+strings.Repeat(" ", indentation))
}

func indentText(text string, indentation int) string {
	return strings.Repeat(" ", indentation) + indentTextHanging(text, indentation)
}

func formatNodeInsertEdit(fileNode *ast.FileNode, node ast.Node, insertPos protocol.Position) (protocol.TextEdit, error) {
	newText, err := format.PrintNode(fileNode, node)
	if err != nil {
		return protocol.TextEdit{}, err
	}

	return protocol.TextEdit{
		Range:   protocol.Range{Start: insertPos, End: insertPos},
		NewText: indentTextHanging(newText, int(insertPos.Character)),
	}, nil
}

func formatNodeRemovalEdits(fileNode *ast.FileNode, nodes []ast.Node, insertPos protocol.Position) []protocol.TextEdit {
	removalEdits := make([]protocol.TextEdit, 0, len(nodes))
	for _, node := range nodes {
		info := fileNode.NodeInfo(node)
		rng := toRange(info)
		if rng.Start != insertPos {
			prevItem, ok := fileNode.Items().Previous(node.Start().AsItem())
			if ok {
				end := fileNode.ItemInfo(prevItem).End()
				rng.Start = toPosition(end)
			}
		}
		removalEdits = append(removalEdits, protocol.TextEdit{
			Range:   rng,
			NewText: "",
		})
	}
	return removalEdits
}

func buildSimplifyOptionsASTChanges[E ast.Node, S ~[]E](
	optionNodePathsByName map[string][][]ast.Node,
	targetOptionName string,
	currentElements S,
) (insert *ast.OptionNode, insertIdx int, remove []ast.Node) {
	optionNodePaths := optionNodePathsByName[targetOptionName]

	nodeMap := make(map[*ast.OptionNode]struct{})
	for _, optionNodePath := range optionNodePaths {
		nodeMap[optionNodePath[len(optionNodePath)-1].(*ast.OptionNode)] = struct{}{}
	}
	toCompact := make([]*ast.OptionNode, 0, len(optionNodePaths))
	insertIdx = -1
	for i, elem := range currentElements {
		node := ast.Node(elem)
		if optNode, ok := node.(*ast.OptionNode); ok {
			if _, ok := nodeMap[optNode]; ok {
				if insertIdx == -1 {
					insertIdx = i
				}
				toCompact = append(toCompact, optNode)
				remove = append(remove, elem)
				continue
			}
		}
	}
	if insertIdx == -1 || len(toCompact) == 0 {
		return nil, -1, nil
	}
	if len(toCompact[0].Name.Parts) == 1 {
		// can't compact this syntax
		return nil, -1, nil
	}
	vals := make([]ast.ValueNode, 0, len(toCompact))
	commas := make([]*ast.RuneNode, 0, len(toCompact)-1)
	for i, optNode := range toCompact {
		vals = append(vals, optNode.Val)
		if i < len(toCompact)-1 {
			commas = append(commas, &ast.RuneNode{Rune: ','})
		}
	}
	newArray := ast.NewArrayLiteralNode(&ast.RuneNode{Rune: '['}, vals, commas, &ast.RuneNode{Rune: ']'})
	newOptName := ast.NewOptionNameNode(toCompact[0].Name.Parts[:len(toCompact[0].Name.Parts)-1], toCompact[0].Name.Dots[:len(toCompact[0].Name.Dots)-1])
	newMsgField := ast.NewMessageFieldNode(toCompact[0].Name.Parts[len(toCompact[0].Name.Parts)-1], &ast.RuneNode{Rune: ':'}, newArray)

	if existing, ok := optionNodePathsByName[format.StringForOptionName(newOptName)]; ok && len(existing) > 0 {
		prev := existing[0][len(existing[0])-1].(*ast.OptionNode)
		remove = append(remove, prev)
		for i, elem := range currentElements {
			if reflect.ValueOf(elem).Equal(reflect.ValueOf(prev)) {
				insertIdx = i
				break
			}
		}

		prevVal := prev.Val.(*ast.MessageLiteralNode)
		var dupRuneNode *ast.RuneNode
		if len(prevVal.Seps) > 0 && prevVal.Seps[len(prevVal.Seps)-1] != nil {
			dupRuneNode = &ast.RuneNode{Rune: prevVal.Seps[len(prevVal.Seps)-1].Rune}
		}

		msgLit := ast.NewMessageLiteralNode(prevVal.Open, append(prevVal.Elements, newMsgField), append(prevVal.Seps, dupRuneNode), prevVal.Close)
		insert = ast.NewOptionNode(prev.Keyword, prev.Name, prev.Equals, msgLit)
		insert.AddSemicolon(prev.Semicolon)
	} else {
		newMsgLit := ast.NewMessageLiteralNode(&ast.RuneNode{Rune: '{'}, []*ast.MessageFieldNode{newMsgField}, []*ast.RuneNode{nil}, &ast.RuneNode{Rune: '}'})
		insert = ast.NewOptionNode(toCompact[0].Keyword, newOptName, toCompact[0].Equals, newMsgLit)
		insert.AddSemicolon(toCompact[0].Semicolon)
	}

	return
}

// simplifyRepeatedFieldLiterals transforms multiple copies of the same repeated
// fields in message literals into a single field with a repeated value.
// For example:
//
// given an option 'repeated string repeated_strings', declarations of the form:
//
//	option (foo) = {
//	  string_list: "foo";
//	  string_list: "bar";
//	};
//
// become consolidated into a single declaration:
//
//	option (foo) = {
//	  string_list: ["foo", "bar"];
//	};
func simplifyRepeatedFieldLiterals(ctx context.Context, request *protocol.CodeActionParams, linkRes linker.Result, mapper *protocol.Mapper, results chan<- protocol.CodeAction) {
	var startToken, endToken ast.Token
	var walkOpts []ast.WalkOption
	fileNode := linkRes.AST()
	if request.Range != (protocol.Range{}) {
		startOff, endOff, _ := mapper.RangeOffsets(request.Range)
		startToken = fileNode.TokenAtOffset(startOff)
		endToken = fileNode.TokenAtOffset(endOff)
		walkOpts = append(walkOpts, ast.WithRange(startToken, endToken))
	} else {
		startToken, _ = fileNode.Tokens().First()
		endToken = fileNode.EOF.Token()
	}
	ast.Walk(fileNode, &ast.SimpleVisitor{
		DoVisitMessageLiteralNode: func(node *ast.MessageLiteralNode) error {
			var targetFieldName string
			fieldsByName := map[string][]*ast.MessageFieldNode{}
			for _, field := range node.Elements {
				if field.IsIncomplete() {
					continue
				}
				name := format.StringForFieldReference(field.Name)
				if startToken >= field.Start() && endToken <= field.End() {
					targetFieldName = name
				}
				fieldsByName[name] = append(fieldsByName[name], field)
			}
			targetFields := fieldsByName[targetFieldName]
			if len(targetFields) < 2 {
				return nil
			}
			insertPos, _ := mapper.OffsetPosition(fileNode.NodeInfo(targetFields[0]).Start().Offset)

			// found a duplicate field
			insert, remove := buildSimplifyFieldLiteralsASTChanges(targetFields)

			insertEdit, err := formatNodeInsertEdit(fileNode, insert, insertPos)
			if err != nil {
				return nil
			}
			removalEdits := formatNodeRemovalEdits(fileNode, remove, insertPos)

			action := protocol.CodeAction{
				Title: "Simplify repeated fields",
				Kind:  protocol.RefactorRewrite,
				Edit: &protocol.WorkspaceEdit{
					Changes: map[protocol.DocumentURI][]protocol.TextEdit{
						mapper.URI: append(removalEdits, insertEdit),
					},
				},
			}
			results <- action
			return nil
		},
	}, walkOpts...)
}

func buildSimplifyFieldLiteralsASTChanges(fields []*ast.MessageFieldNode) (insert *ast.MessageFieldNode, remove []ast.Node) {
	vals := make([]ast.ValueNode, 0, len(fields))
	for _, field := range fields {
		vals = append(vals, field.Val)
		remove = append(remove, field)
	}
	commas := make([]*ast.RuneNode, 0, len(fields)-1)
	for i := 0; i < len(fields)-1; i++ {
		commas = append(commas, &ast.RuneNode{Rune: ','})
	}
	newArray := ast.NewArrayLiteralNode(&ast.RuneNode{Rune: '['}, vals, commas, &ast.RuneNode{Rune: ']'})
	insert = ast.NewMessageFieldNode(fields[0].Name, fields[0].Sep, newArray)
	return
}

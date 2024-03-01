package lsp

import (
	"context"
	"encoding/json"
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
	"github.com/kralicky/tools-lite/gopls/pkg/protocol"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var analyzers = map[protocol.CodeActionKind][]Analyzer{
	protocol.RefactorRewrite: {
		simplifyRepeatedOptions,
		simplifyRepeatedFieldLiterals,
		renumberFields,
	},
	protocol.RefactorExtract: {
		extractFields,
	},
	protocol.RefactorInline: {
		inlineMessageFields,
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

type codeActionData struct {
	Id string `json:"id"`
}

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

	data, _ := json.Marshal(codeActionData{Id: newId})
	return protocol.CodeAction{
		Title: title,
		Kind:  kind,
		Data:  (*json.RawMessage)(&data),
	}
}

func (q *pendingCodeActionManager) resolve(ca *protocol.CodeAction) error {
	if ca.Data == nil {
		return fmt.Errorf("missing data in code action: %v", ca)
	}
	var data codeActionData

	if err := json.Unmarshal(*ca.Data, &data); err != nil {
		return fmt.Errorf("malformed code action data: %v", err)
	}
	pca, ok := q.queue.LoadAndDelete(data.Id)
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
	var existingContainer *ast.OptionNode
	var parentToSearch ast.Node

	if request.Range == (protocol.Range{}) || request.Range.Start != request.Range.End {
		return
	}

	offset, err := mapper.PositionOffset(request.Range.Start)
	if err != nil {
		return
	}
	fileNode := linkRes.AST()
	token, comment := fileNode.ItemAtOffset(offset)
	if token == ast.TokenError || comment.IsValid() {
		return
	}

	path, ok := findPathIntersectingToken(linkRes, token, request.Range.Start)
	if !ok {
		return
	}
	fieldRefNode, ok := path[len(path)-1].(*ast.FieldReferenceNode)
	if !ok {
		return
	}
	optNameNode, ok := path[len(path)-2].(*ast.OptionNameNode)
	if !ok {
		return
	}

	indexIntoName := unwrapIndex(optNameNode.Parts, fieldRefNode)
	var targetExtension protoreflect.ExtensionDescriptor
	targetField := linkRes.FindFieldDescriptorByFieldReferenceNode(fieldRefNode)
	if targetField == nil {
		return
	}
	var targetExtensionNameNode *ast.FieldReferenceNode
	for i := indexIntoName; i >= 0; i-- {
		namePart := optNameNode.Parts[i]
		if !namePart.IsExtension() {
			continue
		}
		namePartDesc := linkRes.OptionNamePartDescriptor(namePart)
		extDesc := linkRes.FindOptionNameFieldDescriptor(namePartDesc)
		if extDesc == nil || !extDesc.IsExtension() {
			return
		}
		targetExtension = extDesc
		targetExtensionNameNode = namePart
		break
	}

	if targetField.Cardinality() != protoreflect.Repeated {
		return
	}
	for i := len(path) - 2; i >= 0; i-- {
		if opt, ok := path[i].(*ast.OptionNode); ok {
			if opt.Name.Parts[len(opt.Name.Parts)-1] == path[len(path)-1] {
				// 'option (parent).frn = ...'
			} else if len(opt.Name.Parts) == 1 {
				// 'option (parent) = ...'
				existingContainer = opt
			}
			parentToSearch = path[i-1]
			break
		}
	}
	if parentToSearch == nil {
		return
	}

	if existingContainer == nil {
		ast.Inspect(parentToSearch, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.OptionNode:
				if node.IsIncomplete() {
					return false
				}
				if existingContainer != nil {
					return false
				}
				for i, part := range node.Name.Parts {
					if part.IsExtension() {
						namePartDesc := linkRes.OptionNamePartDescriptor(part)
						extDesc := linkRes.FindOptionNameFieldDescriptor(namePartDesc)
						if extDesc == targetExtension {
							if i == len(node.Name.Parts)-1 {
								existingContainer = node
								return false
							}
						}
					}
				}
				return false
			}
			return true
		})
	}
	paths := [][3]ast.Node{} // [parent, option, field]
	var tracker ast.AncestorTracker
	ast.Inspect(parentToSearch, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.OptionNode:
			if node.IsIncomplete() {
				return false
			}
			for i, part := range node.Name.Parts {
				if part.IsExtension() {
					namePartDesc := linkRes.OptionNamePartDescriptor(part)
					extDesc := linkRes.FindOptionNameFieldDescriptor(namePartDesc)
					if extDesc == targetExtension {
						if i == len(node.Name.Parts)-2 {
							last := node.Name.Parts[len(node.Name.Parts)-1]
							namePartDesc := linkRes.OptionNamePartDescriptor(last)
							fldDesc := linkRes.FindOptionNameFieldDescriptor(namePartDesc)
							if fldDesc == targetField {
								// 'option (parent).frn = ...'
								paths = append(paths, [3]ast.Node{parentToSearch, node, last})
								return false
							}
						} else if i == len(node.Name.Parts)-1 {
							// 'option (parent) = { ... }'
							return true // allow the visitor to continue to message field nodes
						}
					}
				}
			}
			return false
		case *ast.MessageFieldNode:
			if node.IsIncomplete() {
				return false
			}

			fd := linkRes.FindFieldDescriptorByMessageFieldNode(node)
			if fd != targetField {
				return true
			}

			path := slices.Clone(tracker.Path())
			var containingOption *ast.OptionNode
			for i := len(path) - 1; i >= 0; i-- {
				if opt, ok := path[i].(*ast.OptionNode); ok {
					containingOption = opt
					break
				}
			}

			paths = append(paths, [3]ast.Node{parentToSearch, containingOption, node})
		}
		return true
	}, tracker.AsWalkOptions()...)

	results <- actionQueue.enqueue("Simplify repeated options", protocol.RefactorRewrite, func(ca *protocol.CodeAction) error {
		fileNode := linkRes.AST()

		edits := calcSimplifyRepeatedOptionsEdits(fileNode, existingContainer, targetExtensionNameNode, paths)
		ca.Edit = &protocol.WorkspaceEdit{
			Changes: map[protocol.DocumentURI][]protocol.TextEdit{
				mapper.URI: edits,
			},
		}
		return nil
	})
}

func newFieldVisitor(tracker *ast.AncestorTracker, paths *[][]ast.Node) func(ast.Node) bool {
	return func(node ast.Node) bool {
		switch node.(type) {
		case *ast.MessageNode, *ast.GroupNode, *ast.FieldNode, *ast.MapFieldNode:
			return visitEnclosingRange(tracker, paths)
		}
		return false
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

			updatedParentFields := make([]*ast.MessageElement, 0, len(msgNode.Decls))
			insertedPlaceholder := false
			for _, decl := range msgNode.Decls {
				if fld := decl.GetField(); fld != nil && slices.Contains(enclosedFields, fld) {
					if !insertedPlaceholder {
						insertedPlaceholder = true
						updatedParentFields = append(updatedParentFields, nil)
					}
					continue
				}
				updatedParentFields = append(updatedParentFields, decl)
			}
			var newMsgFields []*ast.MessageElement
			for i, fld := range enclosedFields {
				// the new field type may need to be updated
				fldDesc := fieldDescs[i]
				newFldType := fld.FieldType
				if fldDesc.Kind() == protoreflect.MessageKind {
					relName := relativeFullName(fldDesc.Message().FullName(), desc.ParentFile().Package())
					if relName != string(fld.FieldType.AsIdentifier()) {
						if strings.Contains(relName, ".") {
							compoundIdent := &ast.CompoundIdentNode{}
							parts := strings.Split(relName, ".")
							for i, part := range parts {
								compoundIdent.Components = append(compoundIdent.Components, &ast.IdentNode{Val: part})
								if i < len(parts)-1 {
									compoundIdent.Dots = append(compoundIdent.Dots, &ast.RuneNode{Rune: '.'})
								}
							}
							newFldType = compoundIdent.AsIdentValueNode()
						} else {
							newFldType = (&ast.IdentNode{Val: relName}).AsIdentValue()
						}
					}
				}
				newFld := &ast.FieldNode{
					Label:     fld.Label,
					FieldType: newFldType,
					Name:      fld.Name,
					Equals:    fld.Equals,
					Tag:       &ast.UintLiteralNode{Val: uint64(i + 1)},
					Options:   fld.Options,
					Semicolon: fld.Semicolon,
				}
				newMsgFields = append(newMsgFields, newFld.AsMessageElement())
			}
			newMsgName := findNewUnusedMessageName(desc)
			newFieldName := findNewUnusedFieldName(desc, "newField")
			newMessage := &ast.MessageNode{
				Keyword:    &ast.IdentNode{Val: "message", IsKeyword: true},
				Name:       &ast.IdentNode{Val: newMsgName},
				OpenBrace:  &ast.RuneNode{Rune: '{'},
				Decls:      newMsgFields,
				CloseBrace: &ast.RuneNode{Rune: '}'},
				Semicolon:  &ast.RuneNode{Rune: ';'},
			}

			var label *ast.IdentNode
			if isProto2(fileNode) {
				label = &ast.IdentNode{Val: "optional", IsKeyword: true}
			}
			newParentMessageField := &ast.FieldNode{
				Label:     label,
				FieldType: (&ast.IdentNode{Val: newMsgName}).AsIdentValue(),
				Name:      &ast.IdentNode{Val: newFieldName},
				Equals:    &ast.RuneNode{Rune: '='},
				Tag:       &ast.UintLiteralNode{Val: enclosedFields[0].Tag.Val},
				Semicolon: &ast.RuneNode{Rune: ';'},
			}
			for i, v := range updatedParentFields {
				if v == nil {
					updatedParentFields[i] = newParentMessageField.AsMessageElement()
					break
				}
			}
			updatedParent := &ast.MessageNode{
				Keyword:    msgNode.Keyword,
				Name:       msgNode.Name,
				OpenBrace:  msgNode.OpenBrace,
				Decls:      updatedParentFields,
				CloseBrace: msgNode.CloseBrace,
				Semicolon:  msgNode.Semicolon,
			}

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

func inlineMessageFields(ctx context.Context, request *protocol.CodeActionParams, linkRes linker.Result, mapper *protocol.Mapper, results chan<- protocol.CodeAction) {
	if request.Range == (protocol.Range{}) || request.Range.Start != request.Range.End {
		return
	}
	fileNode := linkRes.AST()
	offset, err := mapper.PositionOffset(request.Range.Start)
	if err != nil {
		return
	}
	token, comment := linkRes.AST().ItemAtOffset(offset)
	if token == ast.TokenError || comment.IsValid() {
		return
	}
	path, ok := findPathIntersectingToken(linkRes, token, request.Range.Start)
	if !ok {
		return
	}

	if len(path) < 3 {
		// the path must be at least 3 nodes long (file, message, field)
		return
	}

	desc, _, err := deepPathSearch(path, linkRes, linkRes)
	if err != nil {
		return
	}
	fieldDesc, ok := desc.(protoreflect.FieldDescriptor)
	if !ok {
		return
	}
	if fieldDesc.Kind() != protoreflect.MessageKind || fieldDesc.IsMap() || fieldDesc.IsList() {
		return
	}

	fieldToInline, ok := path[len(path)-1].(*ast.FieldNode)
	if !ok {
		return
	}
	containingMessage, ok := path[len(path)-2].(*ast.MessageNode)
	if !ok {
		return
	}

	results <- actionQueue.enqueue("Inline nested message", protocol.RefactorInline, func(ca *protocol.CodeAction) error {
		containingMessageDesc := fieldDesc.ContainingMessage()
		msgDesc := fieldDesc.Message()
		if containingMessageDesc == msgDesc {
			// technically nothing stopping us from doing this, but it's almost
			// certainly not what the user intended and would just mess up their code
			return fmt.Errorf("cannot inline a recursive message into itself")
		}
		existingFieldDescs := fieldDesc.ContainingMessage().Fields()
		newFieldDescs := fieldDesc.Message().Fields()
		updatedMsgFields := make([]*ast.MessageElement, len(containingMessage.Decls))
		var largestFieldNumber uint64
		for i, decl := range containingMessage.Decls {
			if fld := decl.GetField(); fld != nil {
				if fld.Tag.Val > largestFieldNumber {
					largestFieldNumber = fld.Tag.Val
				}
				if fld == fieldToInline {
					continue
				}
			}
			updatedMsgFields[i] = decl
		}
		messageInfo := fileNode.NodeInfo(containingMessage)
		messageRange := positionsToRange(messageInfo.Start(), fileNode.NodeInfo(containingMessage.CloseBrace).End())

		startNumber := largestFieldNumber + 1
		mask := map[ast.Node]ast.NodeInfo{
			containingMessage.Keyword: {},
		}
		for i, desc := range updatedMsgFields {
			if desc != nil {
				continue
			}
			var label *ast.IdentNode
			if isProto2(fileNode) {
				label = &ast.IdentNode{Val: "optional", IsKeyword: true}
			}
			toInsert := make([]*ast.MessageElement, 0, newFieldDescs.Len())
			for j := range newFieldDescs.Len() {
				fld := newFieldDescs.Get(j)
				var fieldType string
				switch fld.Kind() {
				case protoreflect.MessageKind:
					fieldType = relativeFullName(fld.Message().FullName(), linkRes.Package())
				case protoreflect.EnumKind:
					fieldType = relativeFullName(fld.Enum().FullName(), linkRes.Package())
				default:
					fieldType = fld.Kind().String()
				}
				fldName := fld.Name()
				if existingFieldDescs.ByName(fldName) != nil {
					fldName = protoreflect.Name(findNewUnusedFieldName(containingMessageDesc, string(fldName)))
				}
				fldNumber := startNumber + uint64(j)
				newField := &ast.FieldNode{
					Label:     label,
					FieldType: (&ast.IdentNode{Val: fieldType}).AsIdentValue(),
					Name:      &ast.IdentNode{Val: string(fldName)},
					Equals:    &ast.RuneNode{Rune: '='},
					Tag:       &ast.UintLiteralNode{Val: fldNumber},
					Semicolon: &ast.RuneNode{Rune: ';'},
				}
				toInsert = append(toInsert, newField.AsMessageElement())
				mask[newField] = ast.NodeInfo{}
			}

			updatedMsgFields[i] = toInsert[0]
			if len(toInsert) > 1 {
				updatedMsgFields = slices.Insert(updatedMsgFields, i+1, toInsert[1:]...)
			}
			break
		}

		updatedMessage := &ast.MessageNode{
			Keyword:    containingMessage.Keyword,
			Name:       containingMessage.Name,
			OpenBrace:  containingMessage.OpenBrace,
			Decls:      updatedMsgFields,
			CloseBrace: containingMessage.CloseBrace,
			Semicolon:  containingMessage.Semicolon,
		}

		updatedMessageText, err := format.PrintNode(format.NodeInfoOverlay(fileNode, mask), updatedMessage)
		if err != nil {
			return fmt.Errorf("error formatting updated message: %v", err)
		}

		ca.Edit = &protocol.WorkspaceEdit{
			Changes: map[protocol.DocumentURI][]protocol.TextEdit{
				request.TextDocument.URI: {
					{
						Range:   messageRange,
						NewText: indentTextHanging(updatedMessageText, int(messageRange.Start.Character)),
					},
				},
			},
		}
		return nil
	})
}

func renumberFields(ctx context.Context, request *protocol.CodeActionParams, linkRes linker.Result, mapper *protocol.Mapper, results chan<- protocol.CodeAction) {
	if request.Range == (protocol.Range{}) || request.Range.Start != request.Range.End {
		return
	}
	fileNode := linkRes.AST()
	offset, err := mapper.PositionOffset(request.Range.Start)
	if err != nil {
		return
	}
	token, comment := linkRes.AST().ItemAtOffset(offset)
	if token == ast.TokenError || comment.IsValid() {
		return
	}
	path, ok := findPathIntersectingToken(linkRes, token, request.Range.Start)
	if !ok {
		return
	}

	desc, _, err := deepPathSearch(path, linkRes, linkRes)
	if err != nil {
		return
	}
	msgNode, ok := path[len(path)-1].(*ast.MessageNode)
	if !ok {
		return
	}
	if token < msgNode.Name.Start() || token > msgNode.Name.End() {
		return
	}

	var canRenumber bool
	msgDesc, ok := desc.(protoreflect.MessageDescriptor)
	if ok {
		fields := msgDesc.Fields()
		for i := range fields.Len() {
			if fields.Get(i).Number() != protowire.Number(i+1) {
				canRenumber = true
				break
			}
		}
	}

	if !canRenumber {
		return
	}

	results <- actionQueue.enqueue("Renumber fields", protocol.RefactorRewrite, func(ca *protocol.CodeAction) error {
		parentInfo := fileNode.NodeInfo(msgNode)
		parentRange := positionsToRange(parentInfo.Start(), fileNode.NodeInfo(msgNode.CloseBrace).End())
		updatedMsgFields := make([]*ast.MessageElement, len(msgNode.Decls))
		number := 1
		mask := map[ast.Node]ast.NodeInfo{
			msgNode.Keyword: {},
		}
		for i, decl := range msgNode.Decls {
			if fld := decl.GetField(); fld != nil {
				newFld := &ast.FieldNode{
					Label:     fld.Label,
					FieldType: fld.FieldType,
					Name:      fld.Name,
					Equals:    fld.Equals,
					Tag:       &ast.UintLiteralNode{Val: uint64(number)},
					Options:   fld.Options,
					Semicolon: fld.Semicolon,
				}
				mask[fld.Tag] = fileNode.NodeInfo(fld.Tag)
				updatedMsgFields[i] = newFld.AsMessageElement()
				number++
			} else {
				updatedMsgFields[i] = decl
			}
		}
		updatedMessage := &ast.MessageNode{
			Keyword:    msgNode.Keyword,
			Name:       msgNode.Name,
			OpenBrace:  msgNode.OpenBrace,
			Decls:      updatedMsgFields,
			CloseBrace: msgNode.CloseBrace,
			Semicolon:  msgNode.Semicolon,
		}

		updatedMessageText, err := format.PrintNode(format.NodeInfoOverlay(fileNode, mask), updatedMessage)
		if err != nil {
			return fmt.Errorf("error formatting updated message: %v", err)
		}

		ca.Edit = &protocol.WorkspaceEdit{
			Changes: map[protocol.DocumentURI][]protocol.TextEdit{
				request.TextDocument.URI: {
					{
						Range:   parentRange,
						NewText: indentTextHanging(updatedMessageText, int(parentRange.Start.Character)),
					},
				},
			},
		}
		return nil
	})
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

func findNewUnusedFieldName(desc protoreflect.MessageDescriptor, prefix string) string {
	name := prefix
	if last := prefix[len(prefix)-1]; last >= '0' && last <= '9' {
		prefix += "_"
	}
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

func calcSimplifyRepeatedOptionsEdits(
	fileNode *ast.FileNode,
	existingContainer *ast.OptionNode,
	extName *ast.FieldReferenceNode,
	paths [][3]ast.Node,
) (edits []protocol.TextEdit) {
	if len(paths) == 0 {
		return nil
	}
	const (
		parentIdx     = 0
		optionNodeIdx = 1
		fieldIndex    = 2
	)
	firstOptNode := paths[0][optionNodeIdx].(*ast.OptionNode)
	if firstOptNode.IsIncomplete() {
		return nil
	}
	parentNode := paths[0][parentIdx]
	replaceWithinParent := func(existingNode *ast.OptionNode, newNodes ...*ast.OptionNode) {
		switch parent := parentNode.(type) {
		case *ast.MessageNode:
			idx := unwrapIndex(parent.Decls, existingNode)
			newElems := make([]*ast.MessageElement, len(newNodes))
			for i, node := range newNodes {
				newElems[i] = node.AsMessageElement()
			}
			parent.Decls = slices.Replace(parent.Decls, idx, idx+1, newElems...)
		case *ast.FileNode:
			newElems := make([]*ast.FileElement, len(newNodes))
			for i, node := range newNodes {
				newElems[i] = node.AsFileElement()
			}
			idx := unwrapIndex(parent.Decls, existingNode)
			parent.Decls = slices.Replace(parent.Decls, idx, idx+1, newElems...)
		}
	}

	var removeWithinExisting func(*ast.MessageFieldNode)
	firstFieldNode := paths[0][fieldIndex].(*ast.MessageFieldNode)

	var containerArrayNode *ast.ArrayLiteralNode

	var pathsWithinContainer [][3]ast.Node
	var pathsOutsideContainer [][3]ast.Node
	if existingContainer == nil {
		pathsOutsideContainer = paths
	} else {
		removeWithinExisting = func(node *ast.MessageFieldNode) {
			existingMsgLit := existingContainer.Val.GetMessageLiteral()
			idx := unwrapIndex(existingMsgLit.Elements, node)
			if idx != -1 {
				existingMsgLit.Elements = slices.Delete(existingMsgLit.Elements, idx, idx+1)
			}
		}
		for _, path := range paths {
			if path[optionNodeIdx] == existingContainer {
				pathsWithinContainer = append(pathsWithinContainer, path)
			} else {
				pathsOutsideContainer = append(pathsOutsideContainer, path)
			}
		}
	}

SEARCH:
	for _, path := range pathsWithinContainer {
		msgField := path[fieldIndex].(*ast.MessageFieldNode)
		switch val := msgField.Val.Unwrap().(type) {
		case *ast.ArrayLiteralNode:
			containerArrayNode = val
			break SEARCH
		}
	}

	var newOptionNode *ast.OptionNode
	if containerArrayNode == nil {
		containerArrayNode = &ast.ArrayLiteralNode{
			OpenBracket:  &ast.RuneNode{Rune: '['},
			CloseBracket: &ast.RuneNode{Rune: ']'},
			Semicolon:    &ast.RuneNode{Rune: ';'},
		}
		msgField := &ast.MessageFieldNode{
			Name:      firstFieldNode.Name,
			Sep:       &ast.RuneNode{Rune: ':'},
			Val:       containerArrayNode.AsValueNode(),
			Semicolon: &ast.RuneNode{Rune: ';'},
		}
		msgLit := &ast.MessageLiteralNode{
			Open:      &ast.RuneNode{Rune: '{'},
			Elements:  []*ast.MessageFieldNode{msgField},
			Seps:      []*ast.RuneNode{nil},
			Close:     &ast.RuneNode{Rune: '}'},
			Semicolon: &ast.RuneNode{Rune: ';'},
		}
		newOptionNode = &ast.OptionNode{
			Keyword:   firstOptNode.Keyword,
			Name:      &ast.OptionNameNode{Parts: []*ast.FieldReferenceNode{extName}},
			Equals:    firstOptNode.Equals,
			Val:       msgLit.AsValueNode(),
			Semicolon: firstOptNode.Semicolon,
		}
	}

	for _, path := range pathsWithinContainer {
		msgField := path[fieldIndex].(*ast.MessageFieldNode)
		switch val := msgField.Val.Unwrap().(type) {
		case *ast.ArrayLiteralNode:
			if val == containerArrayNode {
				continue
			}
			containerArrayNode.Elements = append(containerArrayNode.Elements, val.Elements...)
		default:
			containerArrayNode.Elements = append(containerArrayNode.Elements, msgField.Val)
		}
		removeWithinExisting(msgField)
	}
	for _, path := range pathsOutsideContainer {
		optionNode := path[optionNodeIdx].(*ast.OptionNode)
		switch val := optionNode.Val.Unwrap().(type) {
		case *ast.ArrayLiteralNode:
			containerArrayNode.Elements = append(containerArrayNode.Elements, val.Elements...)
		default:
			containerArrayNode.Elements = append(containerArrayNode.Elements, optionNode.Val)
		}
		replaceWithinParent(optionNode)
	}
	commas := slices.Clone(containerArrayNode.Commas)
	for i := len(commas); i < len(containerArrayNode.Elements)-1; i++ {
		commas = append(commas, &ast.RuneNode{Rune: ','})
	}
	containerArrayNode.Commas = commas

	// replace firstOptionNode with newOptionNode
	replaceWithinParent(firstOptNode, newOptionNode)

	newParentText, err := format.PrintNode(fileNode, parentNode)
	if err != nil {
		panic(err)
	}
	edits = append(edits, protocol.TextEdit{
		Range:   toRange(fileNode.NodeInfo(parentNode)),
		NewText: indentTextHanging(newParentText, int(fileNode.NodeInfo(parentNode).Start().Col-1)),
	})
	return
}

func calcInsertIndex[E interface {
	comparable
	ast.Node
}, S ~[]E](existing ast.Node, elems S, remove S) int {
	for i, elem := range elems {
		if reflect.ValueOf(elem).Equal(reflect.ValueOf(existing)) {
			return i
		} else {
			for i, elem := range elems {
				if slices.Contains(remove, elem) {
					return i
				}
			}
		}
	}
	return len(elems)
}

func formatNodeInsertEdit[E interface {
	comparable
	ast.Node
}](fileNode *ast.FileNode, node E, insertPos protocol.Position) protocol.TextEdit {
	newText, err := format.PrintNode(fileNode, node)
	if err != nil {
		panic(err)
	}
	return protocol.TextEdit{
		Range:   protocol.Range{Start: insertPos, End: insertPos},
		NewText: indentTextHanging(newText, int(insertPos.Character)),
	}
}

func formatNodeRemovalEdits[E interface {
	comparable
	ast.Node
}, S ~[]E](fileNode *ast.FileNode, nodes S, insertPos protocol.Position) []protocol.TextEdit {
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

func buildSimplifyOptionsASTChanges[E interface {
	comparable
	ast.Node
}](
	existingContainer *ast.OptionNode,
	extName *ast.FieldReferenceNode,
	paths [][3]ast.Node,
) (insert *ast.OptionNode, removeWithinParent []E, removeWithinExisting []ast.Node) {
	// nodeMap := make(map[*ast.OptionNode]struct{})
	// for _, optionNodePath := range paths {
	// 	nodeMap[optionNodePath[optionNodeIdx].(*ast.OptionNode)] = struct{}{}
	// }
	// toCompact := make([]*ast.OptionNode, 0, len(paths))
	// insertIdx = -1
	// for i, elem := range currentElements {
	// 	node := ast.Node(elem)
	// 	if optNode, ok := node.(*ast.OptionNode); ok {
	// 		if _, ok := nodeMap[optNode]; ok {
	// 			if insertIdx == -1 {
	// 				insertIdx = i
	// 			}
	// 			toCompact = append(toCompact, optNode)
	// 			remove = append(remove, elem)
	// 			continue
	// 		}
	// 	}
	// }
	// if insertIdx == -1 || len(toCompact) == 0 {
	// 	return nil, -1, nil
	// }
	// if len(toCompact[0].Name.Parts) == 1 {
	// 	// can't compact this syntax
	// 	return nil, -1, nil
	// }
	// vals := make([]ast.ValueNode, 0, len(toCompact))
	// commas := make([]*ast.RuneNode, 0, len(toCompact)-1)
	// for i, optNode := range toCompact {
	// 	vals = append(vals, optNode.Val)
	// 	if i < len(toCompact)-1 {
	// 		commas = append(commas, &ast.RuneNode{Rune: ','})
	// 	}
	// }
	// newArray := ast.NewArrayLiteralNode(&ast.RuneNode{Rune: '['}, vals, commas, &ast.RuneNode{Rune: ']'})
	// newOptName := ast.NewOptionNameNode(toCompact[0].Name.Parts[:len(toCompact[0].Name.Parts)-1], toCompact[0].Name.Dots[:len(toCompact[0].Name.Dots)-1])
	// newMsgField := ast.NewMessageFieldNode(toCompact[0].Name.Parts[len(toCompact[0].Name.Parts)-1], &ast.RuneNode{Rune: ':'}, newArray)

	// if existing, ok := optionNodePathsByName[format.StringForOptionName(newOptName)]; ok && len(existing) > 0 {
	// 	prev := existing[0][len(existing[0])-1].(*ast.OptionNode)
	// 	remove = append(remove, prev)
	// 	for i, elem := range currentElements {
	// 		if reflect.ValueOf(elem).Equal(reflect.ValueOf(prev)) {
	// 			insertIdx = i
	// 			break
	// 		}
	// 	}

	// 	prevVal := prev.Val.(*ast.MessageLiteralNode)
	// 	var dupRuneNode *ast.RuneNode
	// 	if len(prevVal.Seps) > 0 && prevVal.Seps[len(prevVal.Seps)-1] != nil {
	// 		dupRuneNode = &ast.RuneNode{Rune: prevVal.Seps[len(prevVal.Seps)-1].Rune}
	// 	}

	// 	msgLit := ast.NewMessageLiteralNode(prevVal.Open, append(prevVal.Elements, newMsgField), append(prevVal.Seps, dupRuneNode), prevVal.Close)
	// 	insert = ast.NewOptionNode(prev.Keyword, prev.Name, prev.Equals, msgLit)
	// 	insert.AddSemicolon(prev.Semicolon)
	// } else {
	// 	newMsgLit := ast.NewMessageLiteralNode(&ast.RuneNode{Rune: '{'}, []*ast.MessageFieldNode{newMsgField}, []*ast.RuneNode{nil}, &ast.RuneNode{Rune: '}'})
	// 	insert = ast.NewOptionNode(toCompact[0].Keyword, newOptName, toCompact[0].Equals, newMsgLit)
	// 	insert.AddSemicolon(toCompact[0].Semicolon)
	// }

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
		endToken = fileNode.EOF.GetToken()
	}
	ast.Inspect(fileNode, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.MessageLiteralNode:
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
				return true
			}
			insertPos, _ := mapper.OffsetPosition(fileNode.NodeInfo(targetFields[0]).Start().Offset)

			// found a duplicate field
			insert, remove := buildSimplifyFieldLiteralsASTChanges(targetFields)

			insertEdit := formatNodeInsertEdit(fileNode, insert, insertPos)
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
			return true
		}
		return true
	}, walkOpts...)
}

func buildSimplifyFieldLiteralsASTChanges(fields []*ast.MessageFieldNode) (insert *ast.MessageFieldNode, remove []ast.Node) {
	vals := make([]*ast.ValueNode, 0, len(fields))
	for _, field := range fields {
		vals = append(vals, field.Val)
		remove = append(remove, field)
	}
	commas := make([]*ast.RuneNode, 0, len(fields)-1)
	for i := 0; i < len(fields)-1; i++ {
		commas = append(commas, &ast.RuneNode{Rune: ','})
	}
	newArray := &ast.ArrayLiteralNode{
		OpenBracket:  &ast.RuneNode{Rune: '['},
		Elements:     vals,
		Commas:       commas,
		CloseBracket: &ast.RuneNode{Rune: ']'},
		Semicolon:    &ast.RuneNode{Rune: ';'},
	}
	insert = &ast.MessageFieldNode{
		Name:      fields[0].Name,
		Sep:       fields[0].Sep,
		Val:       newArray.AsValueNode(),
		Semicolon: &ast.RuneNode{Rune: ';'},
	}
	return
}

func unwrapIndex[T ast.Node](within []T, elem ast.Node) int {
	for i, e := range within {
		if ast.Unwrap(e) == elem {
			return i
		}
	}
	return -1
}

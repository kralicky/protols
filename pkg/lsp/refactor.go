package lsp

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protols/pkg/format"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
)

var analyzers = []Analyzer{
	simplifyRepeatedOptions,
	simplifyRepeatedFieldLiterals,
}

func FindRefactorActions(ctx context.Context, linkRes linker.Result, mapper *protocol.Mapper, rng protocol.Range) []protocol.CodeAction {
	var wg sync.WaitGroup
	resultsC := make(chan protocol.CodeAction)
	for _, analyzer := range analyzers {
		wg.Add(1)
		go func(analyzer Analyzer) {
			defer wg.Done()
			analyzer(ctx, linkRes, mapper, rng, resultsC)
		}(analyzer)
	}
	go func() {
		wg.Wait()
		close(resultsC)
	}()
	var results []protocol.CodeAction
	for result := range resultsC {
		results = append(results, result)
	}
	return results
}

type Analyzer func(ctx context.Context, linkRes linker.Result, mapper *protocol.Mapper, rng protocol.Range, results chan<- protocol.CodeAction)

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
func simplifyRepeatedOptions(ctx context.Context, linkRes linker.Result, mapper *protocol.Mapper, rng protocol.Range, results chan<- protocol.CodeAction) {
	var tracker ast.AncestorTracker
	parentsToSearch := map[ast.Node][]ast.Node{}
	targetOptionNames := map[string]struct{}{}
	optionNodePathsByParent := make(map[ast.Node]map[string][][]ast.Node)

	walkOpts := tracker.AsWalkOptions()
	if rng != (protocol.Range{}) {
		a := linkRes.AST()
		startOff, endOff, _ := mapper.RangeOffsets(rng)
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

func formatNodeInsertEdit(fileNode *ast.FileNode, node ast.Node, insertPos protocol.Position) (protocol.TextEdit, error) {
	newText, err := format.PrintNode(fileNode, node)
	if err != nil {
		return protocol.TextEdit{}, err
	}

	// indent newText to match the starting indentation at insertPos
	if indentation := insertPos.Character; indentation > 0 {
		newText = strings.ReplaceAll(newText, "\n", "\n"+strings.Repeat(" ", int(indentation)))
	}

	return protocol.TextEdit{
		Range:   protocol.Range{Start: insertPos, End: insertPos},
		NewText: newText,
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
func simplifyRepeatedFieldLiterals(ctx context.Context, linkRes linker.Result, mapper *protocol.Mapper, rng protocol.Range, results chan<- protocol.CodeAction) {
	var startToken, endToken ast.Token
	var walkOpts []ast.WalkOption
	fileNode := linkRes.AST()
	if rng != (protocol.Range{}) {
		startOff, endOff, _ := mapper.RangeOffsets(rng)
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

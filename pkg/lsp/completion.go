package lsp

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/parser"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (c *Cache) GetCompletions(params *protocol.CompletionParams) (result *protocol.CompletionList, err error) {
	doc := params.TextDocument
	parseRes, err := c.FindParseResultByURI(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	mapper, err := c.GetMapper(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	posOffset, err := mapper.PositionOffset(params.Position)
	if err != nil {
		return nil, err
	}

	fileNode := parseRes.AST()

	tokenAtOffset := fileNode.TokenAtOffset(posOffset)

	completions := []protocol.CompletionItem{}
	thisPackage := parseRes.FileDescriptorProto().GetPackage()

	enc := semanticItems{
		options: semanticItemsOptions{
			skipComments: true,
		},
		parseRes: parseRes,
	}
	computeSemanticTokens(c, &enc, ast.WithIntersection(tokenAtOffset))

	var textPrecedingCursor string
	var textEditRange *protocol.Range
	item, found := findNarrowestSemanticToken(parseRes, enc.items, params.Position)
	if found {
		switch item.typ {
		case semanticTypeType, semanticTypeProperty:
		default:
			return nil, nil
		}

		textPrecedingCursor, textEditRange, err = completeWithinToken(posOffset, mapper, item)
		if err != nil {
			return nil, err
		}
	} else {
		// not in a token. check to see if we are in a message scope

		// ast.Walk(fileNode, &ast.SimpleVisitor{
		// 	DoVisitMessageNode: func(node *ast.MessageNode) error {
		// 		scopeBegin := fileNode.NodeInfo(node.OpenBrace).Start().Offset
		// 		scopeEnd := fileNode.NodeInfo(node.CloseBrace).End().Offset

		// 		if posOffset < scopeBegin || posOffset > scopeEnd {
		// 			return nil
		// 		}

		// 		// find the text from the start of the line to the cursor
		// 		startOffset, err := mapper.PositionOffset(protocol.Position{
		// 			Line:      params.Position.Line,
		// 			Character: 0,
		// 		})
		// 		if err != nil {
		// 			return err
		// 		}
		// 		textPrecedingCursor = string(mapper.Content[startOffset:posOffset])
		// 		return nil
		// 	},
		// })
	}

	ctx, ca := context.WithTimeout(context.Background(), 1*time.Second)
	defer ca()
	text := strings.TrimLeftFunc(textPrecedingCursor, unicode.IsSpace)
	fmt.Println("finding completions for", text)
	matches := c.FindAllDescriptorsByPrefix(ctx, text, protoreflect.FullName(thisPackage))
	for _, match := range matches {
		var compl protocol.CompletionItem
		parent := match.ParentFile()
		pkg := parent.Package()
		if pkg != protoreflect.FullName(thisPackage) {
			compl.Label = string(pkg) + "." + string(match.Name())
			compl.AdditionalTextEdits = []protocol.TextEdit{editAddImport(parseRes, parent.Path())}
		} else {
			compl.Label = string(match.Name())
		}
		switch match.(type) {
		case protoreflect.MessageDescriptor:
			compl.Kind = protocol.ClassCompletion
		case protoreflect.EnumDescriptor:
			compl.Kind = protocol.EnumCompletion
		case protoreflect.FieldDescriptor:
			compl.Kind = protocol.FieldCompletion
		default:
			continue
		}
		if textEditRange != nil {
			compl.TextEdit = &protocol.TextEdit{
				Range:   *textEditRange,
				NewText: compl.Label,
			}
		}
		completions = append(completions, compl)
	}

	return &protocol.CompletionList{
		Items: completions,
	}, nil
}

func editAddImport(parseRes parser.Result, path string) protocol.TextEdit {
	insertionPoint := parseRes.ImportInsertionPoint()
	text := fmt.Sprintf("import \"%s\";\n", path)
	return protocol.TextEdit{
		Range: protocol.Range{
			Start: protocol.Position{
				Line:      uint32(insertionPoint.Line - 1),
				Character: uint32(insertionPoint.Col - 1),
			},
			End: protocol.Position{
				Line:      uint32(insertionPoint.Line - 1),
				Character: uint32(insertionPoint.Col - 1),
			},
		},
		NewText: text,
	}
}

func completeWithinToken(posOffset int, mapper *protocol.Mapper, item semanticItem) (string, *protocol.Range, error) {
	startOffset, err := mapper.PositionOffset(protocol.Position{
		Line:      item.line,
		Character: item.start,
	})
	if err != nil {
		return "", nil, err
	}
	return string(mapper.Content[startOffset:posOffset]), &protocol.Range{
		Start: protocol.Position{
			Line:      item.line,
			Character: item.start,
		},
		End: protocol.Position{
			Line:      item.line,
			Character: item.start + item.len,
		},
	}, nil
}

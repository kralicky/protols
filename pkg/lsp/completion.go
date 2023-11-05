package lsp

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/parser"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func (c *Cache) GetCompletions(params *protocol.CompletionParams) (result *protocol.CompletionList, err error) {
	doc := params.TextDocument
	parseRes, err := c.FindParseResultByURI(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	maybeLinkRes, err := c.FindResultByURI(doc.URI.SpanURI())
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
	latestAstValid, err := c.LatestDocumentContentsWellFormed(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	_ = latestAstValid

	tokenAtOffset := fileNode.TokenAtOffset(posOffset)

	completions := []protocol.CompletionItem{}
	thisPackage := parseRes.FileDescriptorProto().GetPackage()

	enc := semanticItems{
		options: semanticItemsOptions{
			skipComments: false,
		},
		parseRes: parseRes,
	}
	computeSemanticTokens(c, &enc, ast.WithIntersection(tokenAtOffset))

	var textPrecedingCursor string
	var textEditRange *protocol.Range
	item, found := findNarrowestSemanticToken(parseRes, enc.items, params.Position)
	if found {
		if item.typ == semanticTypeComment {
			return nil, nil
		}

		textPrecedingCursor, textEditRange, err = completeWithinToken(posOffset, mapper, item)
		if err != nil {
			return nil, err
		}
	} else {
		path, found := findNarrowestEnclosingScope(parseRes, tokenAtOffset, params.Position)
		if found && maybeLinkRes != nil {
			desc, _, err := deepPathSearch(path, maybeLinkRes)
			if err != nil {
				return nil, err
			}
			switch desc := desc.(type) {
			case protoreflect.MessageDescriptor:
				// complete field names
				// filter out fields that are already present
				existingFieldNames := []string{}
				switch node := path[len(path)-1].(type) {
				case *ast.MessageLiteralNode:
					for _, elem := range node.Elements {
						name := string(elem.Name.Name.AsIdentifier())
						if fd := desc.Fields().ByName(protoreflect.Name(name)); fd != nil {
							existingFieldNames = append(existingFieldNames, name)
						}
					}
					for i, l := 0, desc.Fields().Len(); i < l; i++ {
						fld := desc.Fields().Get(i)
						if slices.Contains(existingFieldNames, string(fld.Name())) {
							continue
						}
						insertPos := protocol.Range{
							Start: params.Position,
							End:   params.Position,
						}
						completions = append(completions, fieldCompletion(fld, insertPos))
					}
				}
			}
		}
	}

	return &protocol.CompletionList{
		Items: completions,
	}, nil

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

func fieldCompletion(fld protoreflect.FieldDescriptor, rng protocol.Range) protocol.CompletionItem {
	name := string(fld.Name())
	var docs string
	if src := fld.ParentFile().SourceLocations().ByDescriptor(fld); len(src.Path) > 0 {
		docs = src.LeadingComments
	}

	compl := protocol.CompletionItem{
		Label:  name,
		Kind:   protocol.FieldCompletion,
		Detail: fieldTypeDetail(fld),
		Documentation: &protocol.Or_CompletionItem_documentation{
			Value: protocol.MarkupContent{
				Kind:  protocol.Markdown,
				Value: docs,
			},
		},
		Deprecated: fld.Options().(*descriptorpb.FieldOptions).GetDeprecated(),
	}
	switch fld.Cardinality() {
	case protoreflect.Repeated:
		compl.Detail = fmt.Sprintf("repeated %s", compl.Detail)
		compl.TextEdit = &protocol.TextEdit{
			Range:   rng,
			NewText: fmt.Sprintf("%s: [\n  ${0}\n]", name),
		}
		textFmt := protocol.SnippetTextFormat
		compl.InsertTextFormat = &textFmt
		insMode := protocol.AdjustIndentation
		compl.InsertTextMode = &insMode
	default:
		switch fld.Kind() {
		case protoreflect.MessageKind:
			msg := fld.Message()
			if !msg.IsMapEntry() {
				compl.TextEdit = &protocol.TextEdit{
					Range:   rng,
					NewText: fmt.Sprintf("%s: {\n  ${0}\n}", name),
				}
				textFmt := protocol.SnippetTextFormat
				compl.InsertTextFormat = &textFmt
				insMode := protocol.AdjustIndentation
				compl.InsertTextMode = &insMode
			}
		default:
			compl.TextEdit = &protocol.TextEdit{
				Range:   rng,
				NewText: fmt.Sprintf("%s: ", name),
			}
		}
	}
	return compl
}

func fieldTypeDetail(fld protoreflect.FieldDescriptor) string {
	switch fld.Kind() {
	case protoreflect.MessageKind:
		return string(fld.Message().FullName())
	default:
		return fld.Kind().String()
	}
}

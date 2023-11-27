package lsp

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/parser"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func (c *Cache) GetCompletions(params *protocol.CompletionParams) (result *protocol.CompletionList, err error) {
	doc := params.TextDocument
	currentParseRes, err := c.FindParseResultByURI(doc.URI)
	if err != nil {
		return nil, err
	}
	maybeCurrentLinkRes, err := c.FindResultByURI(doc.URI)
	if err != nil {
		return nil, err
	}
	mapper, err := c.GetMapper(doc.URI)
	if err != nil {
		return nil, err
	}
	posOffset, err := mapper.PositionOffset(params.Position)
	if err != nil {
		return nil, err
	}
	start, end, err := mapper.RangeOffsets(protocol.Range{
		Start: protocol.Position{
			Line:      params.Position.Line,
			Character: 0,
		},
		End: params.Position,
	})
	if err != nil {
		return nil, err
	}
	textPrecedingCursor := string(mapper.Content[start:end])

	latestAstValid, err := c.LatestDocumentContentsWellFormed(doc.URI)
	if err != nil {
		return nil, err
	}

	var searchTarget parser.Result
	if !latestAstValid {
		// The user is in the middle of editing the file and it's not valid yet,
		// so `currentParseRes` has a separate AST than `maybeCurrentLinkRes`.
		// Can't search for descriptors matching the new AST, only the old one.
		searchTarget = maybeCurrentLinkRes
	} else {
		searchTarget = currentParseRes
	}
	columnAdjust := 0
	if len(textPrecedingCursor) > 0 && textPrecedingCursor[len(textPrecedingCursor)-1] == '.' {
		// position the cursor to be on top of the token before the dot
		columnAdjust -= 2
	}
	tokenAtOffset := searchTarget.AST().TokenAtOffset(posOffset + columnAdjust)

	// complete within options
	indexOfOptionKeyword := strings.LastIndex(textPrecedingCursor, "option ") // note the space; don't match 'optional'
	if indexOfOptionKeyword != -1 {
		// if the cursor is before the next =, then we're in the middle of an option name
		if !strings.Contains(textPrecedingCursor[indexOfOptionKeyword:], "=") {
			completions := []protocol.CompletionItem{}
			// check if we have a partial option name on the line
			partialName := strings.TrimSpace(textPrecedingCursor[indexOfOptionKeyword+len("option "):])
			// find the current scope, then complete option names that would be valid here
			if maybeCurrentLinkRes != nil {
				// if we have a previous link result, use its option index
				path, found := findNarrowestEnclosingScope(currentParseRes, tokenAtOffset, adjustColumn(params.Position, columnAdjust))
				if found {
					c, err := completeOptionNames(path, partialName, maybeCurrentLinkRes, params.Position)
					if err != nil {
						return nil, err
					}
					completions = append(completions, c...)
				}
			}
			return &protocol.CompletionList{
				Items: completions,
			}, nil
		}
	}

	path, found := findNarrowestEnclosingScope(searchTarget, tokenAtOffset, adjustColumn(params.Position, columnAdjust))
	if found && maybeCurrentLinkRes != nil {
		completions := []protocol.CompletionItem{}
		desc, _, err := deepPathSearch(path, searchTarget, maybeCurrentLinkRes)
		if err == nil {
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
						completions = append(completions, fieldCompletion(fld, insertPos, messageLiteralStyle))
					}
				}
			case protoreflect.ExtensionTypeDescriptor:
				switch desc.Kind() {
				case protoreflect.MessageKind:
					msg := desc.Message()
					// complete field names
					for i, l := 0, msg.Fields().Len(); i < l; i++ {
						fld := msg.Fields().Get(i)
						insertPos := protocol.Range{
							Start: params.Position,
							End:   params.Position,
						}
						completions = append(completions, fieldCompletion(fld, insertPos, compactOptionsStyle))
					}
				}
			}
		}
		switch node := path[len(path)-1].(type) {
		case *ast.MessageNode:
			completions = append(completions, messageKeywordCompletions(searchTarget)...)
		case *ast.CompactOptionsNode:
			// ex: 'int32 foo = 1 [(<cursor>'
			var partialName string
			for i := len(textPrecedingCursor) - 1; i >= 0; i-- {
				rn := rune(textPrecedingCursor[i])
				if unicode.IsSpace(rn) || rn == '[' || rn == ',' || rn == ';' {
					partialName = textPrecedingCursor[i+1:]
					break
				}
			}

			items, err := completeOptionNamesByScope(fieldScope, path, partialName, maybeCurrentLinkRes, params.Position)
			if err != nil {
				return nil, err
			}
			completions = append(completions, items...)
		case *ast.FieldReferenceNode:
			// ex: 'int32 foo = 1 [(<cursor>)]'
			var items []protocol.CompletionItem
			var err error
			if node.IsIncomplete() {
				// either the name is incomplete, or the close paren is missing
				inc := node.Name.(*ast.IncompleteIdentNode)
				// complete the partial name
				if inc.IncompleteVal != nil {
					items, err = completeOptionNamesByScope(fieldScope, path, string(inc.IncompleteVal.AsIdentifier()), maybeCurrentLinkRes, params.Position)
				} else {
					// the field is empty
					if node.IsExtension() { // note: this only checks for the open paren
						items, err = completeExtensionNamesByScope(fieldScope, string(node.Open.Rune), maybeCurrentLinkRes, params.Position)
					} else {
						items, err = completeOptionNamesByScope(fieldScope, path, "", maybeCurrentLinkRes, params.Position)
					}
				}
			} else {
				// there is a non-empty name and both parens (if it is an extension)
				if node.IsExtension() {
					items, err = completeExtensionNamesByScope(fieldScope, string(node.Name.AsIdentifier()), maybeCurrentLinkRes, params.Position)
				} else {
					items, err = completeOptionNamesByScope(fieldScope, path, string(node.Name.AsIdentifier()), maybeCurrentLinkRes, params.Position)
				}
			}
			if err != nil {
				return nil, err
			}
			completions = append(completions, items...)
		}

		return &protocol.CompletionList{
			Items: completions,
		}, nil
	}

	return nil, nil
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

type fieldCompletionStyle int

const (
	messageLiteralStyle fieldCompletionStyle = iota
	compactOptionsStyle
)

func fieldCompletion(fld protoreflect.FieldDescriptor, rng protocol.Range, style fieldCompletionStyle) protocol.CompletionItem {
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

	var operator string
	switch style {
	case messageLiteralStyle:
		operator = ": "
	case compactOptionsStyle:
		operator = " = "
	}

	switch fld.Cardinality() {
	case protoreflect.Repeated:
		compl.Detail = fmt.Sprintf("repeated %s", compl.Detail)
		compl.TextEdit = &protocol.TextEdit{
			Range:   rng,
			NewText: fmt.Sprintf("%s%s[${0}]", name, operator),
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
					NewText: fmt.Sprintf("%s%s{\n  ${0}\n}", name, operator),
				}
				textFmt := protocol.SnippetTextFormat
				compl.InsertTextFormat = &textFmt
				insMode := protocol.AdjustIndentation
				compl.InsertTextMode = &insMode
			}
		default:
			compl.TextEdit = &protocol.TextEdit{
				Range:   rng,
				NewText: name + operator,
			}
		}
	}
	return compl
}

func fieldTypeDetail(fld protoreflect.FieldDescriptor) string {
	if fld.IsExtension() {
		fqn := fld.FullName()
		xn := fqn.Name()
		return string(fqn.Parent().Append(protoreflect.Name(fmt.Sprintf("(%s): %s", xn, fld.Kind()))))
	}
	switch fld.Kind() {
	case protoreflect.MessageKind:
		if fld.Message().IsMapEntry() {
			return fmt.Sprintf("map<%s, %s>", fld.MapKey().FullName(), fld.MapValue().FullName())
		}
		return string(fld.Message().FullName())
	default:
		return fld.Kind().String()
	}
}

var fieldDescType = reflect.TypeOf((*protoreflect.FieldDescriptor)(nil)).Elem()
var adjustIndentationMode = protocol.AdjustIndentation
var snippetMode = protocol.SnippetTextFormat

func completeOptionNames(path []ast.Node, maybePartialName string, linkRes linker.Result, pos protocol.Position) ([]protocol.CompletionItem, error) {
	var scope completionScope
	switch path[len(path)-1].(type) {
	case *ast.MessageNode:
		scope = messageScope
	case *ast.FieldNode:
		scope = fieldScope
	default:
		return nil, nil
	}
	return completeOptionNamesByScope(scope, path, maybePartialName, linkRes, pos)
}

var msgDescriptorFields = (*descriptorpb.MessageOptions)(nil).ProtoReflect().Descriptor().Fields()
var fieldDescriptorFields = (*descriptorpb.FieldOptions)(nil).ProtoReflect().Descriptor().Fields()

type completionScope int

const (
	messageScope completionScope = iota
	fieldScope
)

var defaultMessageCompletions = []protocol.CompletionItem{
	newBuiltinScalarOptionCompletionItem(msgDescriptorFields.ByName("deprecated")),
}

var defaultFieldCompletions = []protocol.CompletionItem{
	newDefaultPseudoOptionCompletionItem(),
	newJsonNamePseudoOptionCompletionItem(),
	newBuiltinScalarOptionCompletionItem(fieldDescriptorFields.ByName("deprecated")),
	newBuiltinScalarOptionCompletionItem(fieldDescriptorFields.ByName("retention")),
	newBuiltinScalarOptionCompletionItem(fieldDescriptorFields.ByName("targets")),
	newBuiltinScalarOptionCompletionItem(fieldDescriptorFields.ByName("jstype")),
	newBuiltinScalarOptionCompletionItem(fieldDescriptorFields.ByName("packed")),
	newBuiltinScalarOptionCompletionItem(fieldDescriptorFields.ByName("lazy")),
	newBuiltinScalarOptionCompletionItem(fieldDescriptorFields.ByName("unverified_lazy")),
}

func completeExtensionNamesByScope(scope completionScope, maybePartialName string, linkRes linker.Result, pos protocol.Position) ([]protocol.CompletionItem, error) {
	wantExtension := strings.HasPrefix(maybePartialName, "(")
	if wantExtension {
		maybePartialName = strings.TrimPrefix(maybePartialName, "(")
	} else if maybePartialName == "" {
		// 'option <cursor>' should complete builtin message options
		switch scope {
		case messageScope:
			items := slices.Clone(defaultMessageCompletions)
			if linkRes.AST().Edition != nil {
				items = append(items, newBuiltinScalarOptionCompletionItem(msgDescriptorFields.ByName("features")))
			}
			return items, nil
		case fieldScope:
			items := slices.Clone(defaultFieldCompletions)
			if linkRes.AST().Edition != nil {
				items = append(items,
					newBuiltinScalarOptionCompletionItem(fieldDescriptorFields.ByName("features")),
					newBuiltinScalarOptionCompletionItem(fieldDescriptorFields.ByName("edition_defaults")),
				)
			}
			return items, nil
		}
	}
	var extName string
	switch scope {
	case messageScope:
		extName = "google.protobuf.MessageOptions"
	case fieldScope:
		extName = "google.protobuf.FieldOptions"
	}
	candidates, err := linkRes.FindDescriptorsByPrefix(context.TODO(), maybePartialName, func(d protoreflect.Descriptor) bool {
		if fd, ok := d.(protoreflect.ExtensionDescriptor); ok {
			isExt := fd.IsExtension()
			if wantExtension {
				return isExt && fd.ContainingMessage().FullName() == protoreflect.FullName(extName)
			} else {
				return !isExt && fd.ContainingMessage().FullName() == protoreflect.FullName(extName)
			}
		}
		return false
	})
	if err != nil {
		return nil, err
	}

	items := []protocol.CompletionItem{}
	for _, candidate := range candidates {
		fd := candidate.(protoreflect.FieldDescriptor)
		switch fd.Kind() {
		case protoreflect.MessageKind:
			if fd.IsExtension() && wantExtension {
				items = append(items, newExtensionFieldCompletionItem(fd, false))
			} else if !fd.IsExtension() && !wantExtension {
				items = append(items, newMessageFieldCompletionItem(fd))
			}
		default:
			if fd.Cardinality() == protoreflect.Repeated {
				items = append(items, newNonMessageRepeatedOptionCompletionItem(fd, maybePartialName, pos))
			} else if fd.IsExtension() && wantExtension {
				items = append(items, newExtensionNonMessageFieldCompletionItem(fd, false))
			} else if !fd.IsExtension() && !wantExtension {
				items = append(items, newNonMessageFieldCompletionItem(fd))
			}
		}
	}
	return items, nil
}

func completeOptionNamesByScope(scope completionScope, path []ast.Node, maybePartialName string, linkRes linker.Result, pos protocol.Position) ([]protocol.CompletionItem, error) {
	parts := strings.Split(maybePartialName, ".")
	if len(parts) == 1 && !strings.HasSuffix(maybePartialName, ")") {
		return completeExtensionNamesByScope(scope, maybePartialName, linkRes, pos)
	} else if len(parts) > 1 {
		// walk the options path
		var currentContext protoreflect.MessageDescriptor
		for _, part := range parts[:len(parts)-1] {
			isExtension := strings.HasPrefix(part, "(") && strings.HasSuffix(part, ")")
			if isExtension {
				if currentContext == nil {
					targetName := strings.Trim(part, "()")
					fqn := linkRes.Package()
					if !strings.Contains(targetName, ".") {
						fqn = fqn.Append(protoreflect.Name(targetName))
					} else {
						fqn = protoreflect.FullName(targetName)
					}
					xt, err := linker.ResolverFromFile(linkRes).FindExtensionByName(fqn)
					if err != nil {
						return nil, err
					}
					currentContext = xt.TypeDescriptor().Message()
				} else {
					xt := currentContext.Extensions().ByName(protoreflect.Name(strings.Trim(part, "()")))
					if xt == nil {
						return nil, fmt.Errorf("no such extension %s", part)
					}
					currentContext = xt.Message()
				}
			} else if currentContext != nil {
				fd := currentContext.Fields().ByName(protoreflect.Name(part))
				if fd == nil {
					return nil, fmt.Errorf("no such field %s", part)
				}
				if fd.IsExtension() {
					currentContext = fd.ContainingMessage()
				} else {
					currentContext = fd.Message()
				}
			}
			if currentContext == nil {
				return nil, fmt.Errorf("no such extension %s", part)
			}
		}
		// now we have the context, filter by the last part
		lastPart := parts[len(parts)-1]
		isExtension := strings.HasPrefix(lastPart, "(")
		items := []protocol.CompletionItem{}
		if isExtension {
			exts := currentContext.Extensions()
			for i, l := 0, exts.Len(); i < l; i++ {
				ext := exts.Get(i)
				if !strings.Contains(string(ext.Name()), lastPart) {
					continue
				}
				switch ext.Kind() {
				case protoreflect.MessageKind:
					if ext.Message().IsMapEntry() {
						// if the field is actually a map, the completion should insert map syntax
						items = append(items, newMapFieldCompletionItem(ext))
					} else {
						items = append(items, newExtensionFieldCompletionItem(ext, true))
					}
				default:
					if strings.Contains(string(ext.Name()), lastPart) {
						items = append(items, newNonMessageFieldCompletionItem(ext))
					}
				}
			}
		} else {
			// match field names
			fields := currentContext.Fields()
			for i, l := 0, fields.Len(); i < l; i++ {
				fld := fields.Get(i)
				if !strings.Contains(string(fld.Name()), lastPart) {
					continue
				}
				switch fld.Kind() {
				case protoreflect.MessageKind:
					if fld.Message().IsMapEntry() {
						// if the field is actually a map, the completion should insert map syntax
						items = append(items, newMapFieldCompletionItem(fld))
					} else if fld.IsExtension() {
						items = append(items, newExtensionFieldCompletionItem(fld, false))
					} else {
						items = append(items, newMessageFieldCompletionItem(fld))
					}
				default:
					if strings.Contains(string(fld.Name()), lastPart) {
						if fld.Cardinality() == protoreflect.Repeated {
							items = append(items, newNonMessageRepeatedOptionCompletionItem(fld, maybePartialName, pos))
						} else {
							items = append(items, newNonMessageFieldCompletionItem(fld))
						}
					}
				}
			}
		}
		return items, nil
	}
	return nil, nil
}

func newMapFieldCompletionItem(fld protoreflect.FieldDescriptor) protocol.CompletionItem {
	return protocol.CompletionItem{
		Label:            string(fld.Name()),
		Kind:             protocol.StructCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertTextFormat: &snippetMode,
		InsertText:       fmt.Sprintf("%s = {key: ${1:%s}, value: ${2:%s}};", fld.Name(), fieldTypeDetail(fld.MapKey()), fieldTypeDetail(fld.MapValue())),
	}
}

func newMessageFieldCompletionItem(fld protoreflect.FieldDescriptor) protocol.CompletionItem {
	return protocol.CompletionItem{
		Label:            string(fld.Name()),
		Kind:             protocol.StructCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertText:       string(fld.Name()),
		CommitCharacters: []string{"."},
	}
}

func newExtensionFieldCompletionItem(fld protoreflect.FieldDescriptor, needsLeadingOpenParen bool) protocol.CompletionItem {
	var fmtStr string
	if needsLeadingOpenParen {
		fmtStr = "(%s)"
	} else {
		fmtStr = "%s)"
	}
	return protocol.CompletionItem{
		Label:            string(fld.Name()),
		Kind:             protocol.InterfaceCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertText:       fmt.Sprintf(fmtStr, fld.Name()),
		CommitCharacters: []string{"."},
	}
}

func newDefaultPseudoOptionCompletionItem() protocol.CompletionItem {
	return protocol.CompletionItem{
		Label:            "default",
		Kind:             protocol.ValueCompletion,
		InsertTextFormat: &snippetMode,
		InsertText:       "default = ${0}",
	}
}

func newJsonNamePseudoOptionCompletionItem() protocol.CompletionItem {
	return protocol.CompletionItem{
		Label:            "json_name",
		Kind:             protocol.ValueCompletion,
		InsertTextFormat: &snippetMode,
		InsertText:       `json_name = "${0}"`,
	}
}

func newBuiltinScalarOptionCompletionItem(fld protoreflect.FieldDescriptor) protocol.CompletionItem {
	return protocol.CompletionItem{
		Label:            string(fld.Name()),
		Kind:             protocol.ValueCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertTextFormat: &snippetMode,
		InsertText:       fmt.Sprintf("%s = ${1:%s};", fld.Name(), fld.Default().String()),
	}
}

func newNonMessageFieldCompletionItem(fld protoreflect.FieldDescriptor) protocol.CompletionItem {
	return protocol.CompletionItem{
		Label:            string(fld.Name()),
		Kind:             protocol.ValueCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertTextFormat: &snippetMode,
		InsertText:       fmt.Sprintf("%s = ${0};", fld.Name()),
	}
}

func newNonMessageRepeatedOptionCompletionItem(fld protoreflect.FieldDescriptor, partialName string, pos protocol.Position) protocol.CompletionItem {
	// If we're completing from a top-level option, e.g. 'option (foo).bar'
	// where bar is a repeated field, we need to rewrite the expression to
	// 'option (foo) = {bar: []}'. The syntax 'option (foo).bar = []' is
	// not valid.

	cursorPos := pos
	lastIndexDotInPartialName := strings.LastIndexByte(partialName, '.')
	offset := len(partialName) - lastIndexDotInPartialName
	startInsertPos := protocol.Position{
		Line:      cursorPos.Line,
		Character: cursorPos.Character - uint32(offset),
	}
	return protocol.CompletionItem{
		Label:  string(fld.Name()),
		Kind:   protocol.ValueCompletion,
		Detail: fieldTypeDetail(fld),
		AdditionalTextEdits: []protocol.TextEdit{
			{
				Range: protocol.Range{
					Start: startInsertPos,
					End: protocol.Position{
						Line:      cursorPos.Line,
						Character: cursorPos.Character,
					},
				},
				NewText: " = {",
			},
		},
		InsertText:       fmt.Sprintf("%s: [${0}]};", fld.Name()),
		InsertTextFormat: &snippetMode,
	}
}

func newExtensionNonMessageFieldCompletionItem(fld protoreflect.FieldDescriptor, needsLeadingOpenParen bool) protocol.CompletionItem {
	var fmtStr string
	if needsLeadingOpenParen {
		fmtStr = "(%s) = ${0};"
	} else {
		fmtStr = "%s) = ${0};"
	}
	return protocol.CompletionItem{
		Label:            string(fld.Name()),
		Kind:             protocol.InterfaceCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertTextFormat: &snippetMode,
		InsertText:       fmt.Sprintf(fmtStr, fld.Name()),
	}
}

func messageKeywordCompletions(searchTarget parser.Result) []protocol.CompletionItem {
	// add keyword completions for messages
	keywords := completeKeywords("option", "optional", "repeated", "enum", "message", "reserved")
	if searchTarget.AST().Syntax.Syntax.AsString() == "proto2" {
		keywords = append(keywords, completeKeywords("required", "extend", "group")...)
	}
	// score in order
	for i := range keywords {
		keywords[i].SortText = fmt.Sprint(-(i + 1))
	}
	return keywords
}

func completeKeywords(keywords ...string) []protocol.CompletionItem {
	items := []protocol.CompletionItem{}
	for _, keyword := range keywords {
		items = append(items, protocol.CompletionItem{
			Label:      keyword,
			Kind:       protocol.KeywordCompletion,
			InsertText: fmt.Sprintf("%s ", keyword),
		})
	}
	return items
}

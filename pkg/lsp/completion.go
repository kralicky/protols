package lsp

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protocompile/parser"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func (c *Cache) GetCompletions(params *protocol.CompletionParams) (result *protocol.CompletionList, err error) {
	defer func() {
		if result != nil {
			for i := range result.Items {
				result.Items[i].SortText = fmt.Sprintf("%05d", i)
			}
		}
	}()
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
	start, end, err = mapper.RangeOffsets(protocol.Range{
		Start: params.Position,
		End: protocol.Position{
			Line:      params.Position.Line + 1,
			Character: 0,
		},
	})
	if err != nil {
		return nil, err
	}
	textFollowingCursor := string(mapper.Content[start:end])

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
	if following := len(strings.TrimSpace(textFollowingCursor)); following == 0 {
		// adjust by the number of spaces at the end of the text to the left of the cursor
		for i := len(textPrecedingCursor) - 1; i >= 0; i-- {
			if textPrecedingCursor[i] == ' ' {
				columnAdjust--
			} else {
				break
			}
		}
	}
	tokenAtOffset := searchTarget.AST().TokenAtOffset(posOffset + columnAdjust)
	// tokenInfo := searchTarget.AST().TokenInfo(tokenAtOffset).RawText()
	// _ = tokenInfo
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
					comps, err := c.completeOptionNames(path, partialName, maybeCurrentLinkRes, params.Position)
					if err != nil {
						return nil, err
					}
					completions = append(completions, comps...)
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
			partialName := strings.TrimSpace(textPrecedingCursor)
			if !isProto2(searchTarget.AST()) {
				// complete message types
				completions = append(completions, completeTypeNames(c, partialName, maybeCurrentLinkRes, desc.FullName())...)
			}
			completions = append(completions, messageKeywordCompletions(searchTarget, partialName)...)
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

			items, err := c.completeOptionNamesByScope(fieldScope, path, partialName, maybeCurrentLinkRes, params.Position)
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
					items, err = c.completeOptionNamesByScope(fieldScope, path, string(inc.IncompleteVal.AsIdentifier()), maybeCurrentLinkRes, params.Position)
				} else {
					// the field is empty
					if node.IsExtension() { // note: this only checks for the open paren
						items, err = c.completeExtensionNamesByScope(fieldScope, string(node.Open.Rune), maybeCurrentLinkRes, params.Position)
					} else {
						items, err = c.completeOptionNamesByScope(fieldScope, path, "", maybeCurrentLinkRes, params.Position)
					}
				}
			} else {
				// there is a non-empty name and both parens (if it is an extension)
				if node.IsExtension() {
					items, err = c.completeExtensionNamesByScope(fieldScope, string(node.Name.AsIdentifier()), maybeCurrentLinkRes, params.Position)
				} else {
					items, err = c.completeOptionNamesByScope(fieldScope, path, string(node.Name.AsIdentifier()), maybeCurrentLinkRes, params.Position)
				}
			}
			if err != nil {
				return nil, err
			}
			completions = append(completions, items...)
		case *ast.FieldNode:
			// check if we are completing a type name
			var shouldCompleteType bool
			var shouldCompleteKeywords bool
			var completeType string

			switch {
			case tokenAtOffset == node.End():
				// figure out what the previous token is
				switch tokenAtOffset - 1 {
				case node.FldType.End():
					// complete the field name
					switch fldType := node.FldType.(type) {
					case *ast.IncompleteIdentNode:
						if fldType.IncompleteVal != nil {
							completeType = string(fldType.IncompleteVal.AsIdentifier())
						} else {
							completeType = ""
						}
					case *ast.CompoundIdentNode:
						completeType = string(fldType.AsIdentifier())
					case *ast.IdentNode:
						completeType = string(fldType.AsIdentifier())
					}
					shouldCompleteType = true
				case node.Name.Token():
				case node.Equals.Token():
				case node.Tag.Token():
				case node.Options.End():
				}
			case tokenAtOffset >= node.FldType.Start() && tokenAtOffset <= node.FldType.End():
				// complete within the field type
				pos := searchTarget.AST().NodeInfo(node.FldType).Start()
				startOffset, err := mapper.PositionOffset(protocol.Position{
					Line:      uint32(pos.Line - 1),
					Character: uint32(pos.Col - 1),
				})
				if err != nil {
					return nil, err
				}
				cursorIndexIntoType := posOffset - startOffset
				completeType = string(node.FldType.AsIdentifier())
				if len(completeType) >= cursorIndexIntoType {
					completeType = completeType[:cursorIndexIntoType]
					shouldCompleteType = true
				}
				if node.Label.IsPresent() && node.Label.Start() == node.FldType.Start() {
					// handle empty *ast.IncompleteIdentNodes, such as in 'optional <cursor>'
					completeType = ""
					shouldCompleteType = true
				} else {
					// complete keywords
					shouldCompleteKeywords = true
				}

			}
			if shouldCompleteType {
				fmt.Println("completing type", completeType)
				var scope protoreflect.FullName
				if len(path) > 1 {
					if desc, _, err := deepPathSearch(path[:len(path)-1], searchTarget, maybeCurrentLinkRes); err == nil {
						scope = desc.FullName()
					}
				}
				completions = append(completions, completeTypeNames(c, completeType, maybeCurrentLinkRes, scope)...)
			}
			if shouldCompleteKeywords {
				completions = append(completions, messageKeywordCompletions(searchTarget, completeType)...)
			}
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

var (
	fieldDescType         = reflect.TypeOf((*protoreflect.FieldDescriptor)(nil)).Elem()
	adjustIndentationMode = protocol.AdjustIndentation
	snippetMode           = protocol.SnippetTextFormat
)

func (c *Cache) completeOptionNames(path []ast.Node, maybePartialName string, linkRes linker.Result, pos protocol.Position) ([]protocol.CompletionItem, error) {
	var scope completionScope
LOOP:
	for i := len(path) - 1; i >= 0; i-- {
		switch path[i].(type) {
		case *ast.MessageNode:
			scope = messageScope
			break LOOP
		case *ast.FieldNode:
			scope = fieldScope
			break LOOP
		}
	}
	if scope == 0 {
		return nil, nil
	}
	return c.completeOptionNamesByScope(scope, path, maybePartialName, linkRes, pos)
}

var (
	msgDescriptorFields   = (*descriptorpb.MessageOptions)(nil).ProtoReflect().Descriptor().Fields()
	fieldDescriptorFields = (*descriptorpb.FieldOptions)(nil).ProtoReflect().Descriptor().Fields()
)

type completionScope int

const (
	messageScope completionScope = iota + 1
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

func (c *Cache) completeExtensionNamesByScope(scope completionScope, maybePartialName string, linkRes linker.Result, pos protocol.Position) ([]protocol.CompletionItem, error) {
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
	candidates := c.FindAllDescriptorsByPrefix(context.TODO(), maybePartialName, linkRes.Package(), func(d protoreflect.Descriptor) bool {
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

	items := []protocol.CompletionItem{}
	for _, candidate := range candidates {
		fd := candidate.(protoreflect.FieldDescriptor)
		switch fd.Kind() {
		case protoreflect.MessageKind:
			if fd.IsExtension() && wantExtension {
				items = append(items, newExtensionFieldCompletionItem(fd, linkRes.Package(), false))
			} else if !fd.IsExtension() && !wantExtension {
				items = append(items, newMessageFieldCompletionItem(fd, linkRes.Package()))
			}
		default:
			if fd.Cardinality() == protoreflect.Repeated {
				items = append(items, newNonMessageRepeatedOptionCompletionItem(fd, linkRes.Package(), maybePartialName, pos))
			} else if fd.IsExtension() && wantExtension {
				items = append(items, newExtensionNonMessageFieldCompletionItem(fd, linkRes.Package(), false))
			} else if !fd.IsExtension() && !wantExtension {
				items = append(items, newNonMessageFieldCompletionItem(fd, linkRes.Package()))
			}
		}
	}
	return items, nil
}

// splits an option name into parts, respecting extensions grouped by parens
// ex: "(foo.bar).baz" -> ["(foo.bar)", "baz"]
func splitOptionName(name string) []string {
	var parts []string
	var currentPart []rune
	var inParens bool
	for _, rn := range name {
		switch rn {
		case '(':
			inParens = true
		case ')':
			inParens = false
		case '.':
			if !inParens {
				parts = append(parts, string(currentPart))
				currentPart = nil
				continue
			}
		}
		currentPart = append(currentPart, rn)
	}
	parts = append(parts, string(currentPart))
	return parts
}

func (c *Cache) completeOptionNamesByScope(scope completionScope, path []ast.Node, maybePartialName string, linkRes linker.Result, pos protocol.Position) ([]protocol.CompletionItem, error) {
	parts := splitOptionName(maybePartialName)
	if len(parts) == 1 && !strings.HasSuffix(maybePartialName, ")") {
		return c.completeExtensionNamesByScope(scope, maybePartialName, linkRes, pos)
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
			localPkg := currentContext.ParentFile().Package()
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
						items = append(items, newExtensionFieldCompletionItem(ext, localPkg, true))
					}
				default:
					if strings.Contains(string(ext.Name()), lastPart) {
						items = append(items, newNonMessageFieldCompletionItem(ext, localPkg))
					}
				}
			}
		} else {
			// match field names
			fields := currentContext.Fields()
			localPkg := currentContext.ParentFile().Package()
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
						items = append(items, newExtensionFieldCompletionItem(fld, localPkg, false))
					} else {
						items = append(items, newMessageFieldCompletionItem(fld, localPkg))
					}
				default:
					if strings.Contains(string(fld.Name()), lastPart) {
						if fld.Cardinality() == protoreflect.Repeated {
							items = append(items, newNonMessageRepeatedOptionCompletionItem(fld, localPkg, maybePartialName, pos))
						} else {
							items = append(items, newNonMessageFieldCompletionItem(fld, localPkg))
						}
					}
				}
			}
		}
		return items, nil
	}
	return nil, nil
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

func completeTypeNames(cache *Cache, partialName string, linkRes linker.Result, scope protoreflect.FullName) []protocol.CompletionItem {
	var candidates []protoreflect.Descriptor
	filter := func(d protoreflect.Descriptor) bool {
		switch d := d.(type) {
		case protoreflect.MessageDescriptor:
			return !d.IsMapEntry()
		case protoreflect.EnumDescriptor:
			return true
		}
		return false
	}
	var trimPrefix string
	if strings.Contains(partialName, ".") {
		candidates = cache.FindAllDescriptorsByQualifiedPrefix(context.TODO(), partialName, filter)
	} else {
		localPackage := linkRes.Package()
		trimPrefix = string(localPackage + ".")
		candidates = cache.FindAllDescriptorsByPrefix(context.TODO(), partialName, localPackage, filter)
	}
	items := []protocol.CompletionItem{}
	labelPrefix := ""
	if strings.HasPrefix(partialName, ".") {
		labelPrefix = "."
	}
	distanceSort(candidates, linkRes.Package(), scope)
	for _, candidate := range candidates {
		switch candidate.(type) {
		case protoreflect.MessageDescriptor:
			item := protocol.CompletionItem{
				Label:  strings.TrimPrefix(labelPrefix+string(candidate.FullName()), trimPrefix),
				Kind:   protocol.ClassCompletion,
				Detail: string(candidate.FullName()),
			}
			if _, err := linker.ResolverFromFile(linkRes).FindDescriptorByName(candidate.FullName()); err != nil {
				// add an import for this type
				importPath := candidate.ParentFile().Path()
				item.AdditionalTextEdits = append(item.AdditionalTextEdits, editAddImport(linkRes, importPath))
				if item.Label == item.Detail {
					item.Detail = fmt.Sprintf("from %q", importPath)
				} else {
					item.Detail = fmt.Sprintf("%s (from %q)", item.Detail, importPath)
				}
			}
			items = append(items, item)
		case protoreflect.EnumDescriptor:
			item := protocol.CompletionItem{
				Label:  strings.TrimPrefix(labelPrefix+string(candidate.FullName()), trimPrefix),
				Kind:   protocol.EnumCompletion,
				Detail: string(candidate.FullName()),
			}
			if _, err := linker.ResolverFromFile(linkRes).FindDescriptorByName(candidate.FullName()); err != nil {
				importPath := candidate.ParentFile().Path()
				item.AdditionalTextEdits = append(item.AdditionalTextEdits, editAddImport(linkRes, importPath))
				if item.Label == item.Detail {
					item.Detail = fmt.Sprintf("from %q", importPath)
				} else {
					item.Detail = fmt.Sprintf("%s (from %q)", item.Detail, importPath)
				}
			}
			items = append(items, item)
		}
	}
	return items
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

func newMessageFieldCompletionItem(fld protoreflect.FieldDescriptor, localPkg protoreflect.FullName) protocol.CompletionItem {
	name := relativeFullName(fld.FullName(), localPkg)
	return protocol.CompletionItem{
		Label:            string(fld.FullName()),
		Kind:             protocol.StructCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertText:       string(name),
		CommitCharacters: []string{"."},
	}
}

func newExtensionFieldCompletionItem(fld protoreflect.FieldDescriptor, localPkg protoreflect.FullName, needsLeadingOpenParen bool) protocol.CompletionItem {
	var fmtStr string
	if needsLeadingOpenParen {
		fmtStr = "(%s"
	} else {
		fmtStr = "%s"
	}
	name := relativeFullName(fld.FullName(), localPkg)
	return protocol.CompletionItem{
		Label:            string(name),
		Kind:             protocol.InterfaceCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertText:       fmt.Sprintf(fmtStr, name),
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

func newNonMessageFieldCompletionItem(fld protoreflect.FieldDescriptor, localPkg protoreflect.FullName) protocol.CompletionItem {
	return protocol.CompletionItem{
		Label:            string(fld.Name()),
		Kind:             protocol.ValueCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertTextFormat: &snippetMode,
		InsertText:       fmt.Sprintf("%s = ${0};", fld.Name()),
	}
}

func newNonMessageRepeatedOptionCompletionItem(fld protoreflect.FieldDescriptor, localPkg protoreflect.FullName, partialName string, pos protocol.Position) protocol.CompletionItem {
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

func newExtensionNonMessageFieldCompletionItem(fld protoreflect.FieldDescriptor, localPkg protoreflect.FullName, needsLeadingOpenParen bool) protocol.CompletionItem {
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

func isProto2(f *ast.FileNode) bool {
	if f.Syntax == nil {
		return true
	}
	return f.Syntax.Syntax.AsString() == "proto2"
}

func messageKeywordCompletions(searchTarget parser.Result, partialName string) []protocol.CompletionItem {
	// add keyword completions for messages
	possibleKeywords := []string{"option", "optional", "repeated", "enum", "message", "reserved"}
	if isProto2(searchTarget.AST()) {
		possibleKeywords = append(possibleKeywords, "required", "extend", "group")
	}
	if len(partialName) > 0 {
		possibleKeywords = slices.DeleteFunc(possibleKeywords, func(s string) bool {
			return !strings.HasPrefix(s, partialName)
		})
	}
	return completeKeywords(possibleKeywords...)
}

// sort by distance from local package
type entry struct {
	candidate     protoreflect.Descriptor
	pkgDistance   int
	scopeDistance int
}

// Sorts descriptors by distance from the given local package.
// An optional scope may be set to the fully qualified name of a message or enum
// type within the package named by pkgScope. If so, descriptors closer to the
// scope will be sorted earlier in the list.
func distanceSort(candidates []protoreflect.Descriptor, pkgScope, scope protoreflect.FullName) {
	entries := make([]entry, len(candidates))
	for i, candidate := range candidates {
		pkg := candidate.ParentFile().Package()
		pkgDistance := nameDistance(pkgScope, pkg)
		scopeDistance := 0
		if pkgDistance == 0 && scope != "" {
			scopeDistance = nameDistance(scope, candidate.FullName())
		}
		entries[i] = entry{
			candidate:     candidate,
			pkgDistance:   pkgDistance,
			scopeDistance: scopeDistance,
		}
	}
	affineEntrySort(entries)
	for i, entry := range entries {
		candidates[i] = entry.candidate
	}
}

// Sorts entries by pkgDistance, then by scopeDistance, then by fullName.
// Distance values are compared according to the following rules:
//  0. Zero sorts first, regardless of sign.
//  1. If two nonzero values have opposite signs, the negative one sorts first.
//  2. If two nonzero values have the same sign, the one with the smaller
//     absolute value sorts first.
func affineEntrySort(entries []entry) {
	sort.Slice(entries, func(ai, bi int) bool {
		a, b := entries[ai], entries[bi]
		if a.pkgDistance == b.pkgDistance {
			if a.scopeDistance == b.scopeDistance {
				return a.candidate.FullName() < b.candidate.FullName()
			}
			if a.scopeDistance == 0 || b.scopeDistance == 0 {
				return a.scopeDistance == 0
			}
			if (a.scopeDistance < 0) == (b.scopeDistance < 0) {
				return max(a.scopeDistance, -a.scopeDistance) <
					max(b.scopeDistance, -b.scopeDistance)
			}
			return a.scopeDistance < 0
		}
		if a.pkgDistance == 0 || b.pkgDistance == 0 {
			return a.pkgDistance == 0
		}
		if (a.pkgDistance < 0) == (b.pkgDistance < 0) {
			return max(a.pkgDistance, -a.pkgDistance) <
				max(b.pkgDistance, -b.pkgDistance)
		}
		return a.pkgDistance < 0
	})
}

// Returns the distance between two names by counting the number of
// steps required to traverse from 'from' to 'to'. Does not consider proto
// scope semantics. If to is a child of from, the distance is negative.
func nameDistance(from, to protoreflect.FullName) int {
	if from == to {
		return 0
	}
	partsFrom := strings.Split(string(from), ".")
	partsTo := strings.Split(string(to), ".")
	if len(partsFrom) == 0 {
		return len(partsTo)
	} else if len(partsTo) == 0 {
		return len(partsFrom)
	}
	minLen := min(len(partsFrom), len(partsTo))
	for i := 0; i < minLen; i++ {
		if partsFrom[i] != partsTo[i] {
			return len(partsFrom) - i + len(partsTo) - i
		}
	}
	// this will either be (n>0)-(0) or (0)-(n>0)
	return (len(partsFrom) - minLen) - (len(partsTo) - minLen)
}

// Returns the least qualified name that would be required to refer to 'target'
// relative to the package 'fromPkg'.
// ex:
//
//	relativeFullName("foo.bar.baz.A", "foo.bar") => "baz.A"
//	relativeFullName("foo.bar.baz.A", "foo") => "bar.baz.A"
//	relativeFullName("foo.bar.baz.A", "foo.bar.baz") => "A"
//	relativeFullName("foo.bar.baz.A", "x.y") => "foo.bar.baz.A"
func relativeFullName(target, fromPkg protoreflect.FullName) protoreflect.FullName {
	targetPkg := target.Parent()
	// walk targetPkg up until it matches fromPkg, or if empty, it must be fully qualified
	stack := []protoreflect.Name{}
	for {
		if targetPkg == fromPkg {
			for i := len(stack) - 1; i >= 0; i-- {
				targetPkg = targetPkg.Append(stack[i])
			}
			return targetPkg.Append(target.Name())
		}
		if targetPkg == "" {
			return target
		}
		stack = append(stack, targetPkg.Name())
		targetPkg = targetPkg.Parent()
	}
}

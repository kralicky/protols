package lsp

import (
	"context"
	"fmt"
	"path"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protocompile/parser"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func (c *Cache) GetCompletions(params *protocol.CompletionParams) (result *protocol.CompletionList, err error) {
	defer func() {
		if result != nil && len(result.Items) > 0 {
			foundPreselect := false
			for i := range result.Items {
				if result.Items[i].Preselect {
					if !foundPreselect {
						foundPreselect = true
					} else {
						result.Items[i].Preselect = false
					}
				}
				result.Items[i].SortText = fmt.Sprintf("%05d", i)
			}
			if !foundPreselect {
				result.Items[0].Preselect = true
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

	latestAstValid, err := c.LatestDocumentContentsWellFormed(doc.URI, false)
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
	tokenAtOffset := searchTarget.AST().TokenAtOffset(posOffset)

	path, found := findNarrowestEnclosingScope(searchTarget, tokenAtOffset, params.Position)
	if found && maybeCurrentLinkRes != nil {
		completions := []protocol.CompletionItem{}
		desc, _, _ := deepPathSearch(path, searchTarget, maybeCurrentLinkRes)

		scope, existingOpts := findCompletionScopeAndExistingOptions(path, maybeCurrentLinkRes)

		switch node := path[len(path)-1].(type) {
		case *ast.MessageNode:
			partialName := strings.TrimSpace(textPrecedingCursor)
			if !isProto2(searchTarget.AST()) {
				// complete message types
				completions = append(completions, completeTypeNames(c, partialName, "", maybeCurrentLinkRes, desc.FullName(), params.Position)...)
			}
			completions = append(completions, messageKeywordCompletions(searchTarget, partialName)...)
		case *ast.MessageLiteralNode:
			if desc == nil {
				break
			}
			switch desc := desc.(type) {
			case protoreflect.MessageDescriptor:
				// complete field names
				// filter out fields that are already present
				existingFieldNames := []string{}

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

			case protoreflect.ExtensionTypeDescriptor:
				if _, ok := path[len(path)-2].(*ast.ExtendNode); ok {
					// this is a field of an extend node, not a message literal
					break
				}
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
		case *ast.CompactOptionsNode:
			completions = append(completions,
				c.completeOptionOrExtensionName(scope, path, searchTarget.AST(), nil, 0, maybeCurrentLinkRes, existingOpts, mapper, posOffset, params.Position)...)
		case *ast.OptionNode:
			completions = append(completions,
				c.completeOptionOrExtensionName(scope, path, searchTarget.AST(), nil, 0, maybeCurrentLinkRes, existingOpts, mapper, posOffset, params.Position)...)
		case *ast.FieldReferenceNode:
			nodeIdx := -1
			scope := scope
			switch prev := path[len(path)-2].(type) {
			case *ast.OptionNameNode:
				nodeIdx = slices.Index(prev.Parts, node)
			case *ast.MessageFieldNode:
				if desc == nil {
					if desc, _, _ := deepPathSearch(path[:len(path)-2], searchTarget, maybeCurrentLinkRes); desc != nil {
						nodeIdx = 0
						scope = desc
					}
				} else {
					nodeIdx = 0
					if fd, ok := desc.(protoreflect.FieldDescriptor); ok {
						scope = fd.ContainingMessage()
					}
				}
			}
			if nodeIdx == -1 {
				break
			}
			existingFields := map[string]struct{}{}
			if messageLitNode, ok := path[len(path)-3].(*ast.MessageLiteralNode); ok {
				for _, elem := range messageLitNode.Elements {
					if elem.IsIncomplete() {
						continue
					}
					existingFields[string(scope.FullName().Append(protoreflect.Name(elem.Name.Name.AsIdentifier())))] = struct{}{}
				}
			}
			completions = append(completions,
				c.completeOptionOrExtensionName(scope, path, searchTarget.AST(), node, nodeIdx, maybeCurrentLinkRes, existingFields, mapper, posOffset, params.Position)...)
		case *ast.OptionNameNode:
			// this can be the closest node in a few cases, such as after a trailing dot
			nodeChildren := node.Children()
			var lastPart *ast.FieldReferenceNode
			for i := 0; i < len(nodeChildren); i++ {
				if nodeChildren[i].Start() != tokenAtOffset {
					continue
				}
				for j := i; j >= 0; j-- {
					if frn, ok := nodeChildren[j].(*ast.FieldReferenceNode); ok {
						lastPart = frn
						break
					}
				}
			}
			if lastPart == nil {
				break
			}
			prevFd := maybeCurrentLinkRes.FindFieldDescriptorByFieldReferenceNode(lastPart)
			if prevFd == nil {
				break
			}
			items, err := c.deepCompleteOptionNames(prevFd, "", "", maybeCurrentLinkRes, nil, lastPart, params.Position)
			if err != nil {
				return nil, err
			}
			completions = append(completions, items...)

		case *ast.FieldNode:
			// check if we are completing a type name
			var shouldCompleteType bool
			var shouldCompleteKeywords bool
			var completeType string
			var completeTypeSuffix string

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
				if desc == nil {
					break
				}
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
					completeTypeSuffix = completeType[cursorIndexIntoType:]
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
				completions = append(completions, completeTypeNames(c, completeType, completeTypeSuffix, maybeCurrentLinkRes, scope, params.Position)...)
			}
			if shouldCompleteKeywords {
				completions = append(completions, messageKeywordCompletions(searchTarget, completeType)...)
			}
		case *ast.ImportNode:
			// complete import paths
			quoteIdx := strings.IndexRune(textPrecedingCursor, '"')
			if quoteIdx != -1 {
				partialPath := strings.TrimSpace(textPrecedingCursor[quoteIdx+1:])
				endQuoteIdx := strings.IndexRune(textFollowingCursor, '"')
				var partialPathSuffix string
				if endQuoteIdx != -1 {
					partialPathSuffix = strings.TrimSpace(textFollowingCursor[:endQuoteIdx])
				}

				existingImportPaths := []string{}
				if desc != nil {
					imports := maybeCurrentLinkRes.Imports()
					for i, l := 0, imports.Len(); i < l; i++ {
						// don't include the current import in the existing imports list
						imp := imports.Get(i)
						if imp == desc {
							continue
						}
						existingImportPaths = append(existingImportPaths, imp.Path())
					}
				}
				completions = append(completions, completeImports(c, partialPath, partialPathSuffix, existingImportPaths, params.Position)...)
			} else {
				if strings.TrimSpace(textPrecedingCursor) == "import" && node.Public == nil {
					completions = append(completions, protocol.CompletionItem{
						Label: "public",
						Kind:  protocol.KeywordCompletion,
					})
				}
			}
		case *ast.SyntaxNode:
			// complete syntax versions
			quoteIdx := strings.IndexRune(textPrecedingCursor, '"')
			if quoteIdx != -1 {
				partialVersion := strings.TrimSpace(textPrecedingCursor[quoteIdx+1:])
				endQuoteIdx := strings.IndexRune(textFollowingCursor, '"')
				var partialVersionSuffix string
				if endQuoteIdx != -1 {
					partialVersionSuffix = strings.TrimSpace(textFollowingCursor[:endQuoteIdx])
				}
				completions = append(completions, completeSyntaxVersions(partialVersion, partialVersionSuffix, params.Position)...)
			}
		case *ast.PackageNode:
			// complete package names
			completions = append(completions,
				c.completePackageNames(node, path, searchTarget.AST(), maybeCurrentLinkRes, mapper, posOffset, params.Position)...)

		}

		return &protocol.CompletionList{
			Items: completions,
		}, nil
	}

	return nil, nil
}

func (c *Cache) completeOptionOrExtensionName(
	scope protoreflect.Descriptor,
	path []ast.Node,
	fileNode *ast.FileNode,
	node *ast.FieldReferenceNode,
	nodeIdx int,
	linkRes linker.Result,
	existingOpts map[string]struct{},
	mapper *protocol.Mapper,
	posOffset int,
	pos protocol.Position,
) []protocol.CompletionItem {
	var completions []protocol.CompletionItem
	switch nodeIdx {
	case -1:
	case 0: // first part of the option name
		var partialName, partialNameSuffix string
		if node != nil {
			var err error
			partialName, partialNameSuffix, err = findPartialNames(fileNode, node.Name, mapper, posOffset)
			if err != nil {
				return nil
			}
		}
		items, err := c.deepCompleteOptionNames(scope, partialName, partialNameSuffix, linkRes, existingOpts, node, pos)
		if err != nil {
			return nil
		}
		completions = append(completions, items...)
	default: // parts after the first, which should only complete fields within the previous part
		var partialName, partialNameSuffix string
		if node != nil {
			var err error
			partialName, partialNameSuffix, err = findPartialNames(fileNode, node.Name, mapper, posOffset)
			if err != nil {
				return nil
			}
		}
		// nodeIdx > 0
		prevFieldRef := path[len(path)-2].(*ast.OptionNameNode).Parts[nodeIdx-1]
		// find the descriptor for the previous field reference
		prevFd := linkRes.FindFieldDescriptorByFieldReferenceNode(prevFieldRef)
		if prevFd == nil {
			break
		}
		items, err := c.deepCompleteOptionNames(prevFd, partialName, partialNameSuffix, linkRes, existingOpts, node, pos)
		if err != nil {
			return nil
		}
		completions = append(completions, items...)
	}
	return completions
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

func (c *Cache) completePackageNames(
	node *ast.PackageNode,
	nodePath []ast.Node,
	fileNode *ast.FileNode,
	linkRes linker.Result,
	mapper *protocol.Mapper,
	posOffset int,
	pos protocol.Position,
) []protocol.CompletionItem {
	var partialName, partialNameSuffix string
	if !node.IsIncomplete() {
		var err error
		partialName, partialNameSuffix, err = findPartialNames(fileNode, node.Name, mapper, posOffset)
		if err != nil {
			return nil
		}
	}

	dir := path.Dir(linkRes.Path())
	var candidates []protoreflect.FullName
	if base := protoreflect.FullName(path.Base(dir)); base.IsValid() {
		candidates = append(candidates, base)
	}

	var items []protocol.CompletionItem
	c.results.RangeFilesByPrefix(dir, func(f linker.File) bool {
		if f == linkRes {
			return true
		}
		if path.Dir(f.Path()) != dir {
			return true
		}
		pkgName := f.Package()
		candidates = append(candidates, pkgName)
		return true
	})
	slices.Sort(candidates)
	candidates = slices.Compact(candidates)

	replaceRange := protocol.Range{
		Start: adjustColumn(pos, -len(partialName)),
		End:   adjustColumn(pos, len(partialNameSuffix)),
	}
	for _, pkgName := range candidates {
		if strings.HasPrefix(string(pkgName), partialName) {
			item := protocol.CompletionItem{
				Label: string(pkgName),
				Kind:  protocol.ModuleCompletion,
				TextEdit: &protocol.Or_CompletionItem_textEdit{
					Value: protocol.InsertReplaceEdit{
						NewText: string(pkgName),
						Insert:  replaceRange,
						Replace: replaceRange,
					},
				},
			}
			if string(pkgName) == partialName+partialNameSuffix {
				item.Preselect = true
			}

			items = append(items, item)
		}
	}
	return items
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
		compl.TextEdit = &protocol.Or_CompletionItem_textEdit{
			Value: protocol.TextEdit{
				Range:   rng,
				NewText: fmt.Sprintf("%s%s[${0}]", name, operator),
			},
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
				compl.TextEdit = &protocol.Or_CompletionItem_textEdit{
					Value: protocol.TextEdit{
						Range:   rng,
						NewText: fmt.Sprintf("%s%s{\n  ${0}\n}", name, operator),
					},
				}
				textFmt := protocol.SnippetTextFormat
				compl.InsertTextFormat = &textFmt
				insMode := protocol.AdjustIndentation
				compl.InsertTextMode = &insMode
			}
		default:
			compl.TextEdit = &protocol.Or_CompletionItem_textEdit{
				Value: protocol.TextEdit{
					Range:   rng,
					NewText: name + operator,
				},
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

func findCompletionScopeAndExistingOptions(nodePath []ast.Node, linkRes linker.Result) (protoreflect.Descriptor, map[string]struct{}) {
	var scope protoreflect.Descriptor
	existing := map[string]struct{}{}
LOOP:
	for i := len(nodePath) - 1; i >= 0; i-- {
		switch node := nodePath[i].(type) {
		case ast.MessageDeclNode:
			scope = (*descriptorpb.MessageOptions)(nil).ProtoReflect().Descriptor()
			if desc := linkRes.MessageDescriptor(node); desc != nil && desc.Options != nil {
				existing = existingOptions(desc.GetOptions().ProtoReflect())
			}
			break LOOP
		case *ast.FieldNode:
			scope = (*descriptorpb.FieldOptions)(nil).ProtoReflect().Descriptor()
			if desc := linkRes.FieldDescriptor(node); desc != nil && desc.Options != nil {
				existing = existingOptions(desc.GetOptions().ProtoReflect())
			}
			break LOOP
		case *ast.FileNode:
			scope = (*descriptorpb.FileOptions)(nil).ProtoReflect().Descriptor()
			if linkRes.Options() != nil {
				existing = existingOptions(linkRes.Options().ProtoReflect())
			}
			break LOOP
		case *ast.MessageFieldNode:
			linkRes.Descriptor(node)
		}
	}
	return scope, existing
}

func existingOptions(opts protoreflect.Message) map[string]struct{} {
	existing := map[string]struct{}{}
	opts.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		existing[string(fd.FullName())] = struct{}{}
		return true
	})
	return existing
}

type completionScope int

const (
	messageScope completionScope = iota + 1
	fieldScope
	fileScope
)

func (c *Cache) deepCompleteOptionNames(
	prev protoreflect.Descriptor,
	partialName, partialNameSuffix string,
	linkRes linker.Result,
	existingOpts map[string]struct{},
	existingFieldRef *ast.FieldReferenceNode,
	pos protocol.Position,
) ([]protocol.CompletionItem, error) {
	var items []protocol.CompletionItem
	var prevMsg protoreflect.MessageDescriptor
	shouldCompleteExtensions := false
	switch prev := prev.(type) {
	case protoreflect.MessageDescriptor:
		prevMsg = prev
		shouldCompleteExtensions = true
	case protoreflect.FieldDescriptor:
		switch prev.Kind() {
		case protoreflect.MessageKind:
			prevMsg = prev.Message()
			if prevMsg.ExtensionRanges().Len() > 0 {
				shouldCompleteExtensions = true
			}
		default:
			return nil, nil
		}
	}

	replaceRange := protocol.Range{
		Start: adjustColumn(pos, -len(partialName)),
		End:   adjustColumn(pos, len(partialNameSuffix)),
	}
	fields := prevMsg.Fields()
	for i, l := 0, fields.Len(); i < l; i++ {
		fld := fields.Get(i)
		if !strings.HasPrefix(string(fld.Name()), partialName) {
			continue
		}
		if (partialNameSuffix == "" && fld.Name() != protoreflect.Name(partialName)) ||
			(fld.Name() != protoreflect.Name(partialName+partialNameSuffix)) {
			if _, ok := existingOpts[string(fld.FullName())]; ok && fld.Cardinality() != protoreflect.Repeated {
				continue
			}
		}
		item := protocol.CompletionItem{
			Label:  string(fld.Name()),
			Detail: fieldTypeDetail(fld),
			Kind:   protocol.FieldCompletion,
			TextEdit: &protocol.Or_CompletionItem_textEdit{
				Value: protocol.InsertReplaceEdit{
					NewText: string(fld.Name()),
					Insert:  replaceRange,
					Replace: replaceRange,
				},
			},
		}
		if string(fld.Name()) == partialName+partialNameSuffix {
			item.Preselect = true
		}
		items = append(items, item)
	}

	if shouldCompleteExtensions {
		// find any messages extending prevMsg
		for _, x := range c.FindExtensionsByMessage(prevMsg.FullName()) {
			item := newExtensionFieldCompletionItem(x, linkRes, partialName, partialNameSuffix, pos)
			open, close := "(", ")"
			if existingFieldRef != nil {
				if existingFieldRef.Open != nil {
					open = ""
				}
				if existingFieldRef.Close != nil {
					close = ""
				}
			}
			item.Label = fmt.Sprintf("(%s)", item.Label)
			prevEdit := item.TextEdit.Value.(protocol.InsertReplaceEdit)
			item.TextEdit = &protocol.Or_CompletionItem_textEdit{
				Value: protocol.InsertReplaceEdit{
					NewText: fmt.Sprintf("%s%s%s", open, prevEdit.NewText, close),
					Insert:  prevEdit.Insert,
					Replace: prevEdit.Replace,
				},
			}
			maybeResolveImport(&item, x, linkRes)
			items = append(items, item)
		}
	}
	return items, nil
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

func completeTypeNames(cache *Cache, partialName, partialNameSuffix string, linkRes linker.Result, scope protoreflect.FullName, pos protocol.Position) []protocol.CompletionItem {
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
		candidates = cache.FindAllDescriptorsByPrefix(context.TODO(), partialName, filter)
	}
	items := []protocol.CompletionItem{}
	labelPrefix := ""
	if strings.HasPrefix(partialName, ".") {
		labelPrefix = "."
	}
	distanceSort(candidates, linkRes.Package(), scope)
	for _, candidate := range candidates {
		var item protocol.CompletionItem
		switch candidate.(type) {
		case protoreflect.MessageDescriptor:
			item = protocol.CompletionItem{
				Label:  strings.TrimPrefix(labelPrefix+string(candidate.FullName()), trimPrefix),
				Kind:   protocol.ClassCompletion,
				Detail: string(candidate.FullName()),
			}
		case protoreflect.EnumDescriptor:
			item = protocol.CompletionItem{
				Label:  strings.TrimPrefix(labelPrefix+string(candidate.FullName()), trimPrefix),
				Kind:   protocol.EnumCompletion,
				Detail: string(candidate.FullName()),
			}
		default:
			continue
		}
		maybeResolveImport(&item, candidate, linkRes)
		insertText := strings.TrimPrefix(item.Label, partialName)
		replaceRange := protocol.Range{
			Start: pos,
			End:   adjustColumn(pos, len(partialNameSuffix)),
		}
		item.TextEdit = &protocol.Or_CompletionItem_textEdit{
			Value: protocol.InsertReplaceEdit{
				NewText: insertText,
				Insert:  replaceRange,
				Replace: replaceRange,
			},
		}
		items = append(items, item)
	}
	return items
}

func completeImports(cache *Cache, partialPath, partialPathSuffix string, existingImportPaths []string, pos protocol.Position) []protocol.CompletionItem {
	paths := cache.FindImportPathsByPrefix(context.TODO(), partialPath)
	items := []protocol.CompletionItem{}
	existing := map[protocol.DocumentURI]struct{}{}
	for _, path := range existingImportPaths {
		uri, err := cache.resolver.PathToURI(path)
		if err != nil {
			continue
		}
		existing[uri] = struct{}{}
	}
	for uri, importPath := range paths {
		if _, ok := existing[uri]; ok {
			continue
		}
		label := strings.TrimPrefix(importPath, path.Dir(partialPath)+"/")
		if base := path.Base(importPath); len(label) < len(base) {
			label = base
		}
		insertText := strings.TrimPrefix(importPath, partialPath)
		replaceRange := protocol.Range{
			Start: pos,
			End:   adjustColumn(pos, len(partialPathSuffix)),
		}
		items = append(items, protocol.CompletionItem{
			Label: label,
			Kind:  protocol.ModuleCompletion,
			TextEdit: &protocol.Or_CompletionItem_textEdit{
				Value: protocol.InsertReplaceEdit{
					NewText: insertText,
					Insert:  replaceRange,
					Replace: replaceRange,
				},
			},
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Label < items[j].Label
	})
	return items
}

func completeSyntaxVersions(partialVersion, partialVersionSuffix string, pos protocol.Position) []protocol.CompletionItem {
	items := []protocol.CompletionItem{}
	for _, version := range []string{"proto2", "proto3"} {
		replaceRange := protocol.Range{
			Start: pos,
			End:   adjustColumn(pos, len(partialVersionSuffix)),
		}
		if strings.HasPrefix(version, partialVersion) {
			insertText := strings.TrimPrefix(version, partialVersion)
			items = append(items, protocol.CompletionItem{
				Label: version,
				Kind:  protocol.KeywordCompletion,
				TextEdit: &protocol.Or_CompletionItem_textEdit{
					Value: protocol.InsertReplaceEdit{
						NewText: insertText,
						Insert:  replaceRange,
						Replace: replaceRange,
					},
				},
			})
		}
	}
	return items
}

func newMapFieldCompletionItem(fld protoreflect.FieldDescriptor, partialName, partialNameSuffix string, pos protocol.Position) protocol.CompletionItem {
	return protocol.CompletionItem{
		Label:            string(fld.Name()),
		Kind:             protocol.StructCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertTextFormat: &snippetMode,
		InsertText:       fmt.Sprintf("%s = {key: ${1:%s}, value: ${2:%s}};", fld.Name(), fieldTypeDetail(fld.MapKey()), fieldTypeDetail(fld.MapValue())),
	}
}

func newMessageFieldCompletionItem(fld protoreflect.FieldDescriptor, localPkg protoreflect.FullName, partialName, partialNameSuffix string, pos protocol.Position) protocol.CompletionItem {
	name := relativeFullName(fld.FullName(), localPkg)
	return protocol.CompletionItem{
		Label:            string(fld.FullName()),
		Kind:             protocol.StructCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertText:       string(name),
		CommitCharacters: []string{"."},
	}
}

func newExtensionFieldCompletionItem(fld protoreflect.ExtensionDescriptor, linkRes linker.Result, partialName, partialNameSuffix string, pos protocol.Position) protocol.CompletionItem {
	insertText := string(relativeFullName(fld.FullName(), linkRes.Package()))
	replaceRange := protocol.Range{
		Start: adjustColumn(pos, -len(partialName)),
		End:   adjustColumn(pos, len(partialNameSuffix)),
	}
	item := protocol.CompletionItem{
		Label:  insertText,
		Kind:   protocol.InterfaceCompletion,
		Detail: fieldTypeDetail(fld),
		TextEdit: &protocol.Or_CompletionItem_textEdit{
			Value: protocol.InsertReplaceEdit{
				NewText: insertText,
				Insert:  replaceRange,
				Replace: replaceRange,
			},
		},
	}
	return item
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

func newNonMessageRepeatedOptionCompletionItem(fld protoreflect.FieldDescriptor, localPkg protoreflect.FullName, partialName, partialNameSuffix string, pos protocol.Position) protocol.CompletionItem {
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
func relativeFullName(target, fromPkg protoreflect.FullName) string {
	targetPkg := target.Parent()
	// check if fromPkg and targetPkg share a common prefix
	{
		lastDot := 0
		i, l := 0, min(len(targetPkg), len(fromPkg))
		for ; i < l; i++ {
			if targetPkg[i] == fromPkg[i] {
				if targetPkg[i] == '.' {
					lastDot = i
				}
			} else {
				break
			}
		}
		if i == l {
			lastDot = i
		}
		if lastDot > 0 {
			if lastDot < len(targetPkg) {
				targetPkg = targetPkg[lastDot+1:]
			} else {
				targetPkg = ""
			}
			if lastDot < len(fromPkg) {
				fromPkg = fromPkg[lastDot+1:]
			} else {
				fromPkg = ""
			}
		}
	}
	// walk targetPkg up until it matches fromPkg, or if empty, it must be fully qualified
	stack := []protoreflect.Name{}
	pkg := targetPkg
	for {
		if pkg == fromPkg {
			var rel protoreflect.FullName
			for i := len(stack) - 1; i >= 0; i-- {
				rel = rel.Append(stack[i])
			}
			return string(rel.Append(target.Name()))
		}
		if pkg == "" {
			return string(targetPkg.Append(target.Name()))
		}
		stack = append(stack, pkg.Name())
		pkg = pkg.Parent()
	}
}

func findPartialNames(fileNode *ast.FileNode, node ast.Node, mapper *protocol.Mapper, posOffset int) (string, string, error) {
	pos := fileNode.NodeInfo(node)
	startOffset, endOffset, err := mapper.RangeOffsets(toRange(pos))
	if err != nil {
		return "", "", err
	}

	var partialName, partialNameSuffix string
	if startOffset < posOffset {
		partialName = string(mapper.Content[startOffset:posOffset])
	}
	if posOffset < endOffset {
		partialNameSuffix = string(mapper.Content[posOffset:endOffset])
	}
	return partialName, partialNameSuffix, nil
}

func maybeResolveImport(item *protocol.CompletionItem, desc protoreflect.Descriptor, linkRes linker.Result) {
	if _, err := linker.ResolverFromFile(linkRes).FindDescriptorByName(desc.FullName()); err != nil {
		importPath := desc.ParentFile().Path()
		item.AdditionalTextEdits = append(item.AdditionalTextEdits, editAddImport(linkRes, importPath))
		if item.Label == item.Detail {
			item.Detail = fmt.Sprintf("from %q", importPath)
		} else {
			item.Detail = fmt.Sprintf("%s (from %q)", item.Detail, importPath)
		}

	} else {
		if desc.ParentFile().Package() != linkRes.Package() {
			item.Detail = fmt.Sprintf("%s (from %q)", item.Detail, desc.ParentFile().Path())
		}
	}
}

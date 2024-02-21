package lsp

import (
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"unsafe"

	"github.com/bufbuild/protovalidate-go/celext"
	"github.com/google/cel-go/cel"
	celcommon "github.com/google/cel-go/common"
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	"github.com/kralicky/protocompile"
	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protocompile/parser"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
)

//go:generate stringer -type=tokenType,tokenModifier -trimprefix=semantic

type tokenType uint32

const (
	semanticTypeNamespace tokenType = iota
	semanticTypeType
	semanticTypeClass
	semanticTypeEnum
	semanticTypeInterface
	semanticTypeStruct
	semanticTypeTypeParameter
	semanticTypeParameter
	semanticTypeVariable
	semanticTypeProperty
	semanticTypeEnumMember
	semanticTypeEvent
	semanticTypeFunction
	semanticTypeMethod
	semanticTypeMacro
	semanticTypeKeyword
	semanticTypeModifier
	semanticTypeComment
	semanticTypeString
	semanticTypeNumber
	semanticTypeRegexp
	semanticTypeOperator
	semanticTypeDecorator
)

type tokenModifier uint32

const (
	semanticModifierDeclaration tokenModifier = 1 << iota
	semanticModifierDefinition
	semanticModifierReadonly
	semanticModifierStatic
	semanticModifierDeprecated
	semanticModifierAbstract
	semanticModifierAsync
	semanticModifierModification
	semanticModifierDocumentation
	semanticModifierDefaultLibrary
)

type tokenLanguage uint32

const (
	tokenLanguageProto tokenLanguage = iota
	tokenLanguageCel
)

type semanticItem struct {
	lang tokenLanguage

	line, start uint32 // 0-indexed
	len         uint32
	typ         tokenType
	mods        tokenModifier

	// An AST node associated with this token. Used for hover, definitions, etc.
	node ast.Node

	path []ast.Node
}

type semanticItemsOptions struct {
	skipComments bool
}

type semanticItems struct {
	// options used when computing semantic tokens
	options semanticItemsOptions

	// the generated data
	items []semanticItem

	parseRes     parser.Result // cannot be nil
	maybeLinkRes linker.Result // can be nil if there are no linker results available
}

func (s *semanticItems) AST() *ast.FileNode {
	if s.maybeLinkRes != nil {
		return s.maybeLinkRes.AST()
	}
	return s.parseRes.AST()
}

func (c *Cache) ComputeSemanticTokens(doc protocol.TextDocumentIdentifier) ([]uint32, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	if ok, err := c.latestDocumentContentsWellFormedLocked(doc.URI, false); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("document contents not well formed")
	}

	result, err := semanticTokensFull(c, doc)
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (c *Cache) ComputeSemanticTokensRange(doc protocol.TextDocumentIdentifier, rng protocol.Range) ([]uint32, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	if ok, err := c.latestDocumentContentsWellFormedLocked(doc.URI, false); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("document contents not well formed")
	}

	result, err := semanticTokensRange(c, doc, rng)
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func semanticTokensFull(cache *Cache, doc protocol.TextDocumentIdentifier) (*protocol.SemanticTokens, error) {
	parseRes, err := cache.FindParseResultByURI(doc.URI)
	if err != nil {
		return nil, err
	}
	maybeLinkRes, _ := cache.FindResultOrPartialResultByURI(doc.URI)

	enc := semanticItems{
		parseRes:     parseRes,
		maybeLinkRes: maybeLinkRes,
	}
	computeSemanticTokens(cache, &enc)
	ret := &protocol.SemanticTokens{
		Data: enc.Data(),
	}
	return ret, err
}

func semanticTokensRange(cache *Cache, doc protocol.TextDocumentIdentifier, rng protocol.Range) (*protocol.SemanticTokens, error) {
	parseRes, err := cache.FindParseResultByURI(doc.URI)
	if err != nil {
		return nil, err
	}
	maybeLinkRes, _ := cache.FindResultOrPartialResultByURI(doc.URI)

	mapper, err := cache.GetMapper(doc.URI)
	if err != nil {
		return nil, err
	}

	enc := semanticItems{
		parseRes:     parseRes,
		maybeLinkRes: maybeLinkRes,
	}
	a := enc.AST()
	startOff, endOff, _ := mapper.RangeOffsets(rng)
	startToken := a.TokenAtOffset(startOff)
	endToken := a.TokenAtOffset(endOff)
	computeSemanticTokens(cache, &enc, ast.WithRange(startToken, endToken))
	ret := &protocol.SemanticTokens{
		Data: enc.Data(),
	}
	return ret, err
}

var DebugCheckOverlappingTokens = false

func computeSemanticTokens(cache *Cache, e *semanticItems, walkOptions ...ast.WalkOption) {
	e.inspect(cache, e.AST(), walkOptions...)
	sort.Slice(e.items, func(i, j int) bool {
		if e.items[i].line != e.items[j].line {
			return e.items[i].line < e.items[j].line
		}
		return e.items[i].start < e.items[j].start
	})
	if !DebugCheckOverlappingTokens {
		return
	}

	// check for overlapping tokens, at most five times per run
	reportCount := 0
	overlapping := []semanticItem{}
	for i := 1; i < len(e.items)-1; i++ {
		if reportCount >= 5 {
			break
		}
		prevItem := e.items[i-1]
		item := e.items[i]
		if prevItem.line == item.line && prevItem.start+prevItem.len > item.start {
			// add the rest of the tokens on this line to the list of overlapping tokens
			for j := i - 1; j >= 0; j-- {
				if e.items[j].line != item.line {
					break
				}
				overlapping = append(overlapping, e.items[j])
			}
			slices.Reverse(overlapping)
			overlapping = append(overlapping, item)
			skip := 0
			for j := i + 1; j < len(e.items); j++ {
				if e.items[j].line != item.line {
					break
				}
				overlapping = append(overlapping, e.items[j])
				skip++
			}
			// log a detailed error message
			path, _ := cache.resolver.PathToURI(*e.parseRes.FileDescriptorProto().Name)
			mapper, _ := cache.GetMapper(path)
			lineStart, lineEnd, err := mapper.RangeOffsets(protocol.Range{
				Start: protocol.Position{
					Line:      uint32(item.line),
					Character: 0,
				},
				End: protocol.Position{
					Line:      uint32(item.line + 1),
					Character: 0,
				},
			})
			if err != nil {
				panic(err)
			}
			lineText := strings.ReplaceAll(string(mapper.Content[lineStart:lineEnd]), "\t", " ")
			// example output:
			// ==== overlapping tokens ====
			// /path/to/file.proto:line
			//   optional foo = bar;
			//   ^^^^^^^^             [token 1 info]
			//            ^^^^^^^^^   [token 1 info]
			//            ^^^ 				[token 2 info]

			msg := strings.Builder{}
			msg.WriteString("==== overlapping tokens ====\n")
			msg.WriteString(fmt.Sprintf("%s:%d\n", path, item.line+1))
			msg.WriteString(fmt.Sprintf("%s\n", lineText))
			for _, item := range overlapping {
				// write spaces, then ^s, then spaces until the end of the line + 1, then a message
				msg.WriteString(strings.Repeat(" ", int(item.start)))
				msg.WriteString(strings.Repeat("^", int(item.len)))
				msg.WriteString(strings.Repeat(" ", max(len(lineText)+1-int(item.start+item.len), 0)))
				msg.WriteString(fmt.Sprintf("  [type: %s; modifiers: %s]\n", item.typ.String(), item.mods.String()))
			}
			fmt.Fprintln(os.Stderr, msg.String())
			reportCount++
			overlapping = []semanticItem{}

			// continue to the next line
			i += skip - 1
		}
	}
}

func (s *semanticItems) mktokens(node ast.Node, path []ast.Node, tt tokenType, mods tokenModifier) {
	if node == nil {
		return
	}
	node = ast.Unwrap(node)

	info := s.AST().NodeInfo(node)
	if !info.IsValid() {
		return
	}

	length := (info.End().Col - 1) - (info.Start().Col - 1)

	nodeTk := semanticItem{
		lang:  tokenLanguageProto,
		line:  uint32(info.Start().Line - 1),
		start: uint32(info.Start().Col - 1),
		len:   uint32(length),
		typ:   tt,
		mods:  mods,
		node:  node,
		path:  slices.Clone(path),
	}
	s.items = append(s.items, nodeTk)

	s.mkcomments(node)
}

func (s *semanticItems) mktokens_cel(str *ast.StringLiteralNode, start, end int32, tt tokenType, mods tokenModifier) {
	lineInfo := s.AST().NodeInfo(str)
	lineStart := lineInfo.Start()

	nodeTk := semanticItem{
		lang:  tokenLanguageCel,
		line:  uint32(lineStart.Line - 1),
		start: uint32(int32(lineStart.Col) + start),
		len:   uint32(end - start),
		typ:   tt,
		mods:  mods,
	}
	s.items = append(s.items, nodeTk)
}

func (s *semanticItems) mkcomments(node ast.Node) {
	if s.options.skipComments {
		return
	}

	info := s.AST().NodeInfo(node)

	leadingComments := info.LeadingComments()
	for i := 0; i < leadingComments.Len(); i++ {
		comment := leadingComments.Index(i)
		cstart, cend := comment.Start(), comment.End()
		if cend.Line > cstart.Line {
			s.multilineComment(comment, cstart, cend)
			continue
		}
		text := comment.RawText()
		if text, ok := strings.CutPrefix(text, "//protols:"); ok {
			key := strings.SplitN(text, " ", 2)[0]
			if len(key) > 0 {
				switch node := node.(type) {
				case *ast.IdentNode:
					if node.IsKeyword && (node.Val == "syntax" || node.Val == "edition") {
						// pragmas are 3 semantic tokens:
						// 1. '//' (comment)
						// 2. 'protols:key' (macro)
						// 3. ' value' (comment)
						keyLen := len("protols:") + len(key)
						s.items = append(s.items,
							semanticItem{
								line:  uint32(cstart.Line - 1),
								start: uint32(cstart.Col - 1),
								len:   uint32(2),
								typ:   semanticTypeComment,
							},
							semanticItem{
								line:  uint32(cstart.Line - 1),
								start: uint32(cstart.Col - 1 + 2),
								len:   uint32(keyLen),
								typ:   semanticTypeMacro,
							},
						)
						if len(text) > keyLen+2 {
							s.items = append(s.items, semanticItem{
								line:  uint32(cstart.Line - 1),
								start: uint32(cstart.Col - 1 + 2 + keyLen),
								len:   uint32(cend.Col - (cstart.Col - 1) - 2 - keyLen),
								typ:   semanticTypeComment,
							})
						}
						continue
					}
				}
			}
		}
		s.items = append(s.items, semanticItem{
			line:  uint32(cstart.Line - 1),
			start: uint32(cstart.Col - 1),
			len:   uint32(cend.Col - (cstart.Col - 1)),
			typ:   semanticTypeComment,
		})
	}

	trailingComments := info.TrailingComments()
	for i := 0; i < trailingComments.Len(); i++ {
		comment := trailingComments.Index(i)
		if comment.IsVirtual() {
			continue // prevent overlapping tokens
		}
		cstart, cend := comment.Start(), comment.End()
		if cend.Line > cstart.Line {
			s.multilineComment(comment, cstart, cend)
			continue
		}
		s.items = append(s.items, semanticItem{
			line:  uint32(cstart.Line - 1),
			start: uint32(cstart.Col - 1),
			len:   uint32((cend.Col) - (cstart.Col - 1)),
			typ:   semanticTypeComment,
		})
	}
}

func (s *semanticItems) multilineComment(comment ast.Comment, cstart, cend ast.SourcePos) {
	text := comment.RawText()
	lines := strings.Split(text, "\n")
	lineNumbers := make([]uint32, len(lines))
	for i := range lines {
		lineNumbers[i] = uint32(cstart.Line - 1 + i)
	}
	// create a token for the first line
	s.items = append(s.items, semanticItem{
		line:  uint32(cstart.Line - 1),
		start: uint32(cstart.Col - 1),
		len:   uint32(len(lines[0])),
		typ:   semanticTypeComment,
	})
	// create a token for each line between the first and last lines
	for i := 1; i < len(lines)-1; i++ {
		s.items = append(s.items, semanticItem{
			line:  lineNumbers[i],
			start: 0,
			len:   uint32(len(lines[i])),
			typ:   semanticTypeComment,
		})
	}
	// create a token for the last line
	s.items = append(s.items, semanticItem{
		line:  uint32(cend.Line - 1),
		start: 0,
		len:   uint32(cend.Col),
		typ:   semanticTypeComment,
	})
}

var celEnv *cel.Env

func init() {
	celEnv, _ = celext.DefaultEnv(false)
	celEnv, _ = celEnv.Extend(cel.EnableMacroCallTracking())
}

func (s *semanticItems) inspect(cache *Cache, node ast.Node, walkOptions ...ast.WalkOption) {
	tracker := &ast.AncestorTracker{}
	// check if node is a non-nil interface to a nil pointer
	if ast.IsNil(node) {
		return
	}
	walkOptions = append(walkOptions, tracker.AsWalkOptions()...)
	embeddedStringLiterals := make(map[*ast.StringLiteralNode]struct{})

	// NB: when calling mktokens in composite node visitors:
	// - ensure node paths are manually adjusted if creating tokens for a child node
	// - ensure tokens for child nodes are created in the correct order
	ast.Inspect(node, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.StringLiteralNode:
			if _, ok := embeddedStringLiterals[node]; ok {
				s.mkcomments(node)
				return true
			}
			s.mktokens(node, tracker.Path(), semanticTypeString, 0)
		case *ast.UintLiteralNode:
			s.mktokens(node, tracker.Path(), semanticTypeNumber, 0)
		case *ast.FloatLiteralNode:
			s.mktokens(node, tracker.Path(), semanticTypeNumber, 0)
		case *ast.SpecialFloatLiteralNode:
			s.mktokens(node, tracker.Path(), semanticTypeNumber, 0)
			return false
		case *ast.IdentNode:
			if node.IsKeyword {
				s.mktokens(node, tracker.Path(), semanticTypeKeyword, 0)
			}
		case *ast.RuneNode:
			if node.Virtual || node.Rune == 0 {
				return true
			}
			switch node.Rune {
			case '}', '{', '.', ',', '<', '>', '(', ')', '[', ']', ';', ':':
				s.mkcomments(node)
			default:
				s.mktokens(node, tracker.Path(), semanticTypeOperator, 0)
			}
		case *ast.ExtendNode:
			switch extendee := node.Extendee.Unwrap().(type) {
			case *ast.IdentNode:
				s.mktokens(extendee, append(tracker.Path(), extendee), semanticTypeType, 0)
			case *ast.CompoundIdentNode:
				modifier := tokenModifier(0)
				if strings.Contains(string(extendee.AsIdentifier()), "google.protobuf.") {
					modifier = semanticModifierDefaultLibrary
				}
				s.mktokens(extendee, append(tracker.Path(), extendee), semanticTypeType, modifier)
				// for _, node := range extendee.OrderedNodes() {
				// 	s.mktokens(node, append(tracker.Path(), node), semanticTypeType, modifier)
				// }
			}
		case *ast.OneofNode:
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeInterface, semanticModifierDefinition)
		case *ast.MessageNode:
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeType, semanticModifierDefinition)
		case *ast.GroupNode:
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeType, semanticModifierDefinition)
		case *ast.FieldNode:
			var modifier tokenModifier
			if !ast.IsNil(node.FieldType) {
				if id := string(node.FieldType.AsIdentifier()); protocompile.IsScalarType(id) || protocompile.IsWellKnownType(protoreflect.FullName(id)) {
					modifier = semanticModifierDefaultLibrary
				}

				if node.Label == nil || node.Label.Start() != node.FieldType.Start() {
					// for incomplete nodes, the field type might be the same as the label
					// if the type is missing
					switch fldType := node.FieldType.Unwrap().(type) {
					case *ast.IdentNode:
						s.mktokens(fldType, append(tracker.Path(), fldType), semanticTypeType, modifier)
					case *ast.CompoundIdentNode:
						for _, node := range fldType.OrderedNodes() {
							s.mktokens(node, append(tracker.Path(), node), semanticTypeType, modifier)
						}
					}
				}
			}
			if node.Name != nil {
				s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeVariable, semanticModifierDefinition)
			}
		case *ast.OptionNameNode:
			path := tracker.Path()
			for _, part := range node.OrderedNodes() {
				switch node := part.(type) {
				case *ast.FieldReferenceNode:
					if node.IsExtension() {
						name := node.GetName().Unwrap()
						s.mktokens(node.Open, append(path, node, node.Open), semanticTypeOperator, 0)
						s.mktokens(name, append(path, node, name), semanticTypeProperty, 0)
						s.mktokens(node.Close, append(path, node, node.Close), semanticTypeOperator, 0)
					} else {
						// handle "default" and "json_name" pseudo-options
						name := node.Name.Unwrap()
						if name.AsIdentifier() == "default" || name.AsIdentifier() == "json_name" {
							// treat it as a keyword
							s.mktokens(name, append(path, node, name), semanticTypeKeyword, 0)
						} else {
							s.mktokens(name, append(path, node, name), semanticTypeProperty, 0)
						}
					}
				case *ast.RuneNode:
					s.mktokens(part, append(path, part), semanticTypeProperty, 0)
				}
			}
			return false
		case *ast.OptionNode:
			if node.Name != nil && len(node.Name.Parts) > 0 {
				switch val := node.Val.Unwrap().(type) {
				case *ast.ArrayLiteralNode:
					s.inspectArrayLiteral(node.Name.Parts[len(node.Name.Parts)-1], val, tracker)
				default:
					s.inspectFieldLiteral(node.Name.Parts[len(node.Name.Parts)-1], node.Val, tracker)
				}
			}
		case *ast.MessageLiteralNode:
			hasExpressionField := false
			hasIdField := false
			for _, elem := range node.Elements {
				if elem.Name == nil {
					continue
				}
				if elem.Name.Name.AsIdentifier() == "expression" {
					hasExpressionField = true
				} else if elem.Name.Name.AsIdentifier() == "id" {
					hasIdField = true
				} else if elem.Name.Name.AsIdentifier() == "key" {
					_ = 1
				}
				if hasExpressionField && hasIdField {
					break
				}
			}
			if hasExpressionField && hasIdField {
				tokens, _ := s.inspectCelExpr(node)
				for _, lit := range tokens {
					embeddedStringLiterals[lit] = struct{}{}
				}
			}
		case *ast.MessageFieldNode:
			path := tracker.Path()
			fieldRef := node.Name
			if fieldRef.IsAnyTypeReference() {
				// [type.googleapis.com/foo.bar.baz]
				open := fieldRef.Open
				urlPrefix := fieldRef.UrlPrefix.Unwrap()
				slash := fieldRef.Slash
				name := fieldRef.Name.Unwrap()
				close := fieldRef.Close
				s.mktokens(open, append(path, fieldRef, open), semanticTypeOperator, 0)
				s.mktokens(urlPrefix, append(path, fieldRef, urlPrefix), semanticTypeType, semanticModifierDefaultLibrary)
				s.mktokens(slash, append(path, fieldRef, slash), semanticTypeType, semanticModifierDefaultLibrary)
				s.mktokens(name, append(path, fieldRef, name), semanticTypeType, 0)
				s.mktokens(close, append(path, fieldRef, close), semanticTypeOperator, 0)
			} else if fieldRef.IsExtension() {
				// [foo.bar.baz]
				open := fieldRef.Open
				name := fieldRef.GetName().Unwrap()
				close := fieldRef.Close
				s.mktokens(open, append(path, fieldRef, open), semanticTypeOperator, 0)
				s.mktokens(name, append(path, fieldRef, name), semanticTypeProperty, 0)
				s.mktokens(close, append(path, fieldRef, close), semanticTypeOperator, 0)
			} else {
				s.mktokens(fieldRef, append(path, fieldRef), semanticTypeProperty, 0)
			}
			switch val := node.Val.Unwrap().(type) {
			case *ast.ArrayLiteralNode:
				s.inspectArrayLiteral(node, val, tracker)
			default:
				s.inspectFieldLiteral(node, node.Val, tracker)
			}
		case *ast.MapFieldNode:
			vt := node.MapType.ValueType.Unwrap()
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeProperty, 0)
			s.mktokens(node.MapType.KeyType, append(tracker.Path(), node.MapType, node.MapType.KeyType), semanticTypeType, 0)
			s.mktokens(vt, append(tracker.Path(), node.MapType, vt), semanticTypeType, 0)
		case *ast.RPCTypeNode:
			if node.IsIncomplete() {
				return true
			}
			mt := node.MessageType.Unwrap()
			s.mktokens(mt, append(tracker.Path(), mt), semanticTypeType, 0)
		case *ast.RPCNode:
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeFunction, 0)
		case *ast.ServiceNode:
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeClass, 0)
		case *ast.PackageNode:
			if node.IsIncomplete() {
				return true
			}
			name := node.Name.Unwrap()
			s.mktokens(name, append(tracker.Path(), name), semanticTypeNamespace, 0)
		case *ast.EnumNode:
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeClass, 0)
		case *ast.EnumValueNode:
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeEnumMember, 0)
		}
		return true
	}, walkOptions...)
}

func (s *semanticItems) inspectFieldLiteral(node ast.Node, val *ast.ValueNode, tracker *ast.AncestorTracker) {
	if s.maybeLinkRes == nil {
		return
	}
	var fd protoreflect.FieldDescriptor
	switch node := node.(type) {
	case *ast.FieldReferenceNode:
		fd = s.maybeLinkRes.FindFieldDescriptorByFieldReferenceNode(node)
	case *ast.MessageFieldNode:
		fd = s.maybeLinkRes.FindFieldDescriptorByMessageFieldNode(node)
	}

	if fd == nil {
		return
	}
	switch fd.Kind() {
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		if sfl := val.GetSpecialFloatLiteral(); sfl != nil && sfl.Keyword != nil {
			s.mktokens(val, append(tracker.Path(), sfl.Keyword), semanticTypeNumber, 0)
		}
	case protoreflect.BoolKind:
		if id := val.GetIdent(); id != nil {
			switch id.AsIdentifier() {
			case "true", "false":
				s.mktokens(id, append(tracker.Path(), id), semanticTypeKeyword, 0)
			}
		}
	case protoreflect.EnumKind:
		if enum := fd.Enum(); enum != nil {
			if val := val.GetIdent(); val != nil {
				if enum.Values().ByName(protoreflect.Name(val.AsIdentifier())) != nil {
					s.mktokens(val, append(tracker.Path(), val), semanticTypeEnumMember, 0)
				}
			}
		}
	}
}

func (s *semanticItems) inspectArrayLiteral(node ast.Node, val *ast.ArrayLiteralNode, tracker *ast.AncestorTracker) {
	if s.maybeLinkRes == nil {
		return
	}
	var fd protoreflect.FieldDescriptor
	switch node := node.(type) {
	case *ast.FieldReferenceNode:
		fd = s.maybeLinkRes.FindFieldDescriptorByFieldReferenceNode(node)
	case *ast.MessageFieldNode:
		fd = s.maybeLinkRes.FindFieldDescriptorByMessageFieldNode(node)
	}
	if fd == nil {
		return
	}
	switch fd.Kind() {
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		for _, elem := range val.Elements {
			if sfl := elem.GetSpecialFloatLiteral(); sfl != nil && sfl.Keyword != nil {
				s.mktokens(sfl, append(tracker.Path(), sfl), semanticTypeNumber, 0)
			}
		}
	case protoreflect.BoolKind:
		for _, elem := range val.Elements {
			if id := elem.GetIdent(); id != nil && id.IsKeyword && (id.AsIdentifier() == "true" || id.AsIdentifier() == "false") {
				s.mktokens(elem, append(tracker.Path(), elem), semanticTypeKeyword, 0)
			}
		}
	case protoreflect.EnumKind:
		if enum := fd.Enum(); enum != nil {
			for _, elem := range val.Elements {
				if id := elem.GetIdent(); id != nil {
					name := id.AsIdentifier()
					if enum.Values().ByName(name) != nil {
						s.mktokens(id, append(tracker.Path(), id), semanticTypeEnumMember, 0)
					}
				}
			}
		}
	}
}

func (s *semanticItems) inspectCelExpr(messageLit *ast.MessageLiteralNode) ([]*ast.StringLiteralNode, []ProtoDiagnostic) {
	for _, elem := range messageLit.Elements {
		if elem.Name.Name.AsIdentifier() == "expression" {
			diagnostics := []ProtoDiagnostic{}
			stringNodes := []*ast.StringLiteralNode{}
			var celExpr string
			switch val := elem.Val.Unwrap().(type) {
			case *ast.StringLiteralNode:
				stringNodes = append(stringNodes, val)
				celExpr = val.AsString()
			case *ast.CompoundStringLiteralNode:
				lines := []string{}
				for _, part := range val.Elements {
					if str := part.GetStringLiteral(); str != nil {
						stringNodes = append(stringNodes, str)
						strVal := strings.TrimSpace(str.AsString())
						lines = append(lines, strVal)
					}
				}
				celExpr = strings.Join(lines, "\n")
			}
			// escape backslashes that would have been un-escaped by the parser
			celExpr = strings.ReplaceAll(celExpr, `\`, `\\`)
			parsed, issues := celEnv.Parse(celExpr)
			if issues != nil && issues.Err() != nil {
				slog.Warn("error parsing CEL expression",
					"location", s.AST().NodeInfo(stringNodes[0]).String(),
				)
				fmt.Println(issues.Err())
				// TODO
				// diagnostics = append(diagnostics, ProtoDiagnostic{
				// 	Pos:      s.AST().NodeInfo(stringNodes[0]),
				// 	Severity: protocol.SeverityWarning,
				// 	Error:    issues.Err(),
				// })
				continue
			}
			ast := getAst(parsed)
			sourceInfo := ast.SourceInfo()
			celast.PreOrderVisit(ast.Expr(), celast.NewExprVisitor(func(e celast.Expr) {
				switch e.Kind() {
				case celast.UnspecifiedExprKind:
				case celast.CallKind:
					call := e.AsCall()
					if displayName, ok := operators.FindReverse(call.FunctionName()); ok && len(displayName) > 0 {
						start := sourceInfo.GetStartLocation(e.ID())
						end := start.Column() + len(displayName)
						s.mktokens_cel(stringNodes[start.Line()-1], int32(start.Column()), int32(end), semanticTypeOperator, 0)
						return
					}
				case celast.IdentKind:
					ident := e.AsIdent()
					start := sourceInfo.GetStartLocation(e.ID())
					end := start.Column() + len(ident)

					tokenType := semanticTypeVariable
					if ident == "this" {
						tokenType = semanticTypeKeyword
					}
					s.mktokens_cel(stringNodes[start.Line()-1], int32(start.Column()), int32(end), tokenType, 0)
				case celast.LiteralKind:
					val := e.AsLiteral().Value()
					switch val.(type) {
					case int, int32, int64, uint, uint32, uint64, float32, float64:
						start := sourceInfo.GetStartLocation(e.ID())
						end := start.Column() + len(fmt.Sprint(val))

						s.mktokens_cel(stringNodes[start.Line()-1], int32(start.Column()), int32(end), semanticTypeNumber, 0)
					case string:
						start := sourceInfo.GetStartLocation(e.ID())
						end := start.Column() + len(val.(string)) + 2 // +2 for quotes
						s.mktokens_cel(stringNodes[start.Line()-1], int32(start.Column()), int32(end), semanticTypeString, 0)
					case bool:
						start := sourceInfo.GetStartLocation(e.ID())
						end := start.Column() + len(fmt.Sprint(val))

						s.mktokens_cel(stringNodes[start.Line()-1], int32(start.Column()), int32(end), semanticTypeKeyword, 0)
					}
				}
			}))

			return stringNodes, diagnostics
		}
	}
	return nil, nil
}

// there is no method to get the underlying ast i guess?? lol
func getAst(parsed *cel.Ast) *celast.AST {
	return (*struct {
		source celcommon.Source
		impl   *celast.AST
	})(unsafe.Pointer(parsed)).impl
}

func (e *semanticItems) Data() []uint32 {
	// each semantic token needs five values
	// (see Integer Encoding for Tokens in the LSP spec)
	x := make([]uint32, 5*len(e.items))
	var j int
	var last semanticItem
	for i := 0; i < len(e.items); i++ {
		item := e.items[i]
		if j == 0 {
			x[0] = e.items[0].line
		} else {
			x[j] = item.line - last.line
		}
		x[j+1] = item.start
		if j > 0 && x[j] == 0 {
			x[j+1] = item.start - last.start
		}
		x[j+2] = item.len
		x[j+3] = uint32(item.typ)
		x[j+4] = uint32(item.mods)
		j += 5
		last = item
	}
	return x[:j]
}

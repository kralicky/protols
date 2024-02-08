package lsp

import (
	"fmt"
	"log/slog"
	"os"
	"reflect"
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

	parseRes parser.Result // cannot be nil
	linkRes  linker.Result // can be nil if there are no linker results available
}

func (s *semanticItems) AST() *ast.FileNode {
	if s.linkRes != nil {
		return s.linkRes.AST()
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
		parseRes: parseRes,
		linkRes:  maybeLinkRes,
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
		parseRes: parseRes,
		linkRes:  maybeLinkRes,
	}
	a := enc.AST()
	startOff, endOff, _ := mapper.RangeOffsets(rng)
	startToken := a.ItemAtOffset(startOff)
	endToken := a.ItemAtOffset(endOff)
	computeSemanticTokens(cache, &enc, ast.WithRange(startToken, endToken))
	ret := &protocol.SemanticTokens{
		Data: enc.Data(),
	}
	return ret, err
}

const debugCheckOverlappingTokens = false

func computeSemanticTokens(cache *Cache, e *semanticItems, walkOptions ...ast.WalkOption) {
	e.inspect(cache, e.AST(), walkOptions...)
	sort.Slice(e.items, func(i, j int) bool {
		if e.items[i].line != e.items[j].line {
			return e.items[i].line < e.items[j].line
		}
		return e.items[i].start < e.items[j].start
	})
	if !debugCheckOverlappingTokens {
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
	if node == (ast.NoSourceNode{}) {
		return
	}

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
				case *ast.KeywordNode:
					if node.Val == "syntax" || node.Val == "edition" {
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
	if reflect.ValueOf(node).IsNil() {
		return
	}
	walkOptions = append(walkOptions, tracker.AsWalkOptions()...)
	embeddedStringLiterals := make(map[*ast.StringLiteralNode]struct{})

	// NB: when calling mktokens in composite node visitors:
	// - ensure node paths are manually adjusted if creating tokens for a child node
	// - ensure tokens for child nodes are created in the correct order
	ast.Walk(node, &ast.SimpleVisitor{
		DoVisitStringLiteralNode: func(node *ast.StringLiteralNode) error {
			if _, ok := embeddedStringLiterals[node]; ok {
				s.mkcomments(node)
				return nil
			}
			s.mktokens(node, tracker.Path(), semanticTypeString, 0)
			return nil
		},
		DoVisitUintLiteralNode: func(node *ast.UintLiteralNode) error {
			s.mktokens(node, tracker.Path(), semanticTypeNumber, 0)
			return nil
		},
		DoVisitFloatLiteralNode: func(node *ast.FloatLiteralNode) error {
			s.mktokens(node, tracker.Path(), semanticTypeNumber, 0)
			return nil
		},
		DoVisitSpecialFloatLiteralNode: func(node *ast.SpecialFloatLiteralNode) error {
			s.mktokens(node, tracker.Path(), semanticTypeNumber, 0)
			return nil
		},
		DoVisitKeywordNode: func(node *ast.KeywordNode) error {
			s.mktokens(node, tracker.Path(), semanticTypeKeyword, 0)
			return nil
		},
		DoVisitRuneNode: func(node *ast.RuneNode) error {
			switch node.Rune {
			case '}', '{', '.', ',', '<', '>', '(', ')', '[', ']', ';', ':':
				s.mkcomments(node)
			default:
				s.mktokens(node, tracker.Path(), semanticTypeOperator, 0)
			}
			return nil
		},
		DoVisitExtendNode: func(node *ast.ExtendNode) error {
			switch extendee := node.Extendee.(type) {
			case *ast.IdentNode:
				s.mktokens(extendee, append(tracker.Path(), extendee), semanticTypeType, 0)
			case *ast.CompoundIdentNode:
				modifier := tokenModifier(0)
				if strings.Contains(extendee.Val, "google.protobuf.") {
					modifier = semanticModifierDefaultLibrary
				}
				for _, node := range extendee.Children() {
					s.mktokens(node, append(tracker.Path(), node), semanticTypeType, modifier)
				}
			}
			return nil
		},
		DoVisitOneofNode: func(node *ast.OneofNode) error {
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeInterface, semanticModifierDefinition)
			return nil
		},
		DoVisitMessageNode: func(node *ast.MessageNode) error {
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeType, semanticModifierDefinition)
			return nil
		},
		DoVisitGroupNode: func(node *ast.GroupNode) error {
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeType, semanticModifierDefinition)
			return nil
		},
		DoVisitFieldNode: func(node *ast.FieldNode) error {
			var modifier tokenModifier
			if id := string(node.FldType.AsIdentifier()); protocompile.IsScalarType(id) || protocompile.IsWellKnownType(protoreflect.FullName(id)) {
				modifier = semanticModifierDefaultLibrary
			}
			if !node.Label.IsPresent() || node.Label.Start() != node.FldType.Start() {
				// for incomplete nodes, the field type might be the same as the label
				// if the type is missing
				switch fldType := node.FldType.(type) {
				case *ast.IdentNode:
					s.mktokens(node.FldType, append(tracker.Path(), node.FldType), semanticTypeType, modifier)
				case *ast.CompoundIdentNode:
					for _, node := range fldType.Children() {
						s.mktokens(node, append(tracker.Path(), node), semanticTypeType, modifier)
					}
				}
			}
			s.mktokens(node.FieldName(), append(tracker.Path(), node.FieldName()), semanticTypeVariable, semanticModifierDefinition)
			return nil
		},
		DoVisitFieldReferenceNode: func(node *ast.FieldReferenceNode) error {
			if node.IsIncomplete() {
				return nil
			}
			if node.IsAnyTypeReference() {
				s.mktokens(node.URLPrefix, append(tracker.Path(), node.URLPrefix), semanticTypeType, semanticModifierDefaultLibrary)
				s.mktokens(node.Slash, append(tracker.Path(), node.Slash), semanticTypeType, semanticModifierDefaultLibrary)
				s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeType, 0)
			} else if node.IsExtension() {
				s.mktokens(node.Open, append(tracker.Path(), node.Open), semanticTypeOperator, 0)
				s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeVariable, semanticModifierStatic)
				s.mktokens(node.Close, append(tracker.Path(), node.Close), semanticTypeOperator, 0)
			} else {
				// handle "default" and "json_name" pseudo-options
				if node.Name.AsIdentifier() == "default" || node.Name.AsIdentifier() == "json_name" {
					// treat it as a keyword
					s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeKeyword, 0)
				} else {
					s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeProperty, 0)
				}
			}
			return nil
		},
		DoVisitOptionNode: func(node *ast.OptionNode) error {
			if node.Name != nil && len(node.Name.Parts) > 0 {
				switch val := node.Val.(type) {
				case ast.IdentValueNode:
					s.inspectFieldLiteral(node.Name.Parts[len(node.Name.Parts)-1], val, tracker)
				case *ast.ArrayLiteralNode:
					s.inspectArrayLiteral(node.Name.Parts[len(node.Name.Parts)-1], val, tracker)
				}
			}
			return nil
		},
		DoVisitMessageLiteralNode: func(node *ast.MessageLiteralNode) error {
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
			return nil
		},
		DoVisitMessageFieldNode: func(node *ast.MessageFieldNode) error {
			switch val := node.Val.(type) {
			case ast.IdentValueNode:
				s.inspectFieldLiteral(node, val, tracker)
			case *ast.ArrayLiteralNode:
				s.inspectArrayLiteral(node, val, tracker)
			}
			return nil
		},
		DoVisitMapFieldNode: func(node *ast.MapFieldNode) error {
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeProperty, 0)
			s.mktokens(node.MapType.KeyType, append(tracker.Path(), node.MapType, node.MapType.KeyType), semanticTypeType, 0)
			s.mktokens(node.MapType.ValueType, append(tracker.Path(), node.MapType, node.MapType.ValueType), semanticTypeType, 0)
			return nil
		},
		DoVisitRPCTypeNode: func(node *ast.RPCTypeNode) error {
			if node.IsIncomplete() {
				return nil
			}
			s.mktokens(node.MessageType, append(tracker.Path(), node.MessageType), semanticTypeType, 0)
			return nil
		},
		DoVisitRPCNode: func(node *ast.RPCNode) error {
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeFunction, 0)
			return nil
		},
		DoVisitServiceNode: func(node *ast.ServiceNode) error {
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeClass, 0)
			return nil
		},
		DoVisitPackageNode: func(node *ast.PackageNode) error {
			if node.IsIncomplete() {
				return nil
			}
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeNamespace, 0)
			return nil
		},
		DoVisitEnumNode: func(node *ast.EnumNode) error {
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeClass, 0)
			return nil
		},
		DoVisitEnumValueNode: func(node *ast.EnumValueNode) error {
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeEnumMember, 0)
			return nil
		},
	}, walkOptions...)
}

func (s *semanticItems) inspectFieldLiteral(node ast.Node, val ast.IdentValueNode, tracker *ast.AncestorTracker) {
	var fd protoreflect.FieldDescriptor
	switch node := node.(type) {
	case *ast.FieldReferenceNode:
		fd = s.linkRes.FindFieldDescriptorByFieldReferenceNode(node)
	case *ast.MessageFieldNode:
		fd = s.linkRes.FindFieldDescriptorByMessageFieldNode(node)
	}

	if fd == nil {
		return
	}
	switch fd.Kind() {
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		switch val.AsIdentifier() {
		case "inf", "infinity", "nan", "-inf", "-infinity", "-nan":
			s.mktokens(val, append(tracker.Path(), val), semanticTypeNumber, 0)
		}
	case protoreflect.BoolKind:
		switch val.AsIdentifier() {
		case "true", "false":
			s.mktokens(val, append(tracker.Path(), val), semanticTypeKeyword, 0)
		}
	case protoreflect.EnumKind:
		if enum := fd.Enum(); enum != nil {
			if enum.Values().ByName(protoreflect.Name(val.AsIdentifier())) != nil {
				s.mktokens(val, append(tracker.Path(), val), semanticTypeEnumMember, 0)
			}
		}
	}
}

func (s *semanticItems) inspectArrayLiteral(node ast.Node, val *ast.ArrayLiteralNode, tracker *ast.AncestorTracker) {
	var fd protoreflect.FieldDescriptor
	switch node := node.(type) {
	case *ast.FieldReferenceNode:
		fd = s.linkRes.FindFieldDescriptorByFieldReferenceNode(node)
	case *ast.MessageFieldNode:
		fd = s.linkRes.FindFieldDescriptorByMessageFieldNode(node)
	}
	if fd == nil {
		return
	}
	switch fd.Kind() {
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		for _, elem := range val.Elements {
			switch val := elem.Value().(type) {
			case ast.Identifier:
				switch val {
				case "inf", "infinity", "nan", "-inf", "-infinity", "-nan":
					s.mktokens(elem, append(tracker.Path(), elem), semanticTypeNumber, 0)
				}
			}
		}
	case protoreflect.BoolKind:
		for _, elem := range val.Elements {
			switch val := elem.Value().(type) {
			case ast.Identifier:
				switch val {
				case "true", "false":
					s.mktokens(elem, append(tracker.Path(), elem), semanticTypeKeyword, 0)
				}
			}
		}
	case protoreflect.EnumKind:
		if enum := fd.Enum(); enum != nil {
			for _, elem := range val.Elements {
				switch val := elem.Value().(type) {
				case ast.Identifier:
					if enum.Values().ByName(protoreflect.Name(val)) != nil {
						s.mktokens(elem, append(tracker.Path(), elem), semanticTypeEnumMember, 0)
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
			switch val := elem.Val.(type) {
			case *ast.StringLiteralNode:
				stringNodes = append(stringNodes, val)
				celExpr = val.AsString()
			case *ast.CompoundStringLiteralNode:
				lines := []string{}
				for _, part := range val.Children() {
					if str, ok := part.(*ast.StringLiteralNode); ok {
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

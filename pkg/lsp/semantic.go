package lsp

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"unsafe"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protovalidate-go/celext"
	"github.com/google/cel-go/cel"
	celcommon "github.com/google/cel-go/common"
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	"golang.org/x/exp/slices"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
)

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

func semanticTokensFull(cache *Cache, doc protocol.TextDocumentIdentifier) (*protocol.SemanticTokens, error) {
	parseRes, err := cache.FindParseResultByURI(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	maybeLinkRes, _ := cache.FindResultByURI(doc.URI.SpanURI())

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
	parseRes, err := cache.FindParseResultByURI(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	maybeLinkRes, _ := cache.FindResultByURI(doc.URI.SpanURI())

	mapper, err := cache.GetMapper(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	a := parseRes.AST()
	startOff, endOff, _ := mapper.RangeOffsets(rng)
	startToken := a.TokenAtOffset(startOff)
	endToken := a.TokenAtOffset(endOff)

	enc := semanticItems{
		parseRes: parseRes,
		linkRes:  maybeLinkRes,
	}
	computeSemanticTokens(cache, &enc, ast.WithRange(startToken, endToken))
	ret := &protocol.SemanticTokens{
		Data: enc.Data(),
	}
	return ret, err
}

func computeSemanticTokens(cache *Cache, e *semanticItems, walkOptions ...ast.WalkOption) {
	e.inspect(cache, e.parseRes.AST(), walkOptions...)
}

func findNarrowestSemanticToken(parseRes parser.Result, tokens []semanticItem, pos protocol.Position) (narrowest semanticItem, found bool) {
	// find the narrowest token that contains the position and also has a node
	// associated with it. The set of tokens will contain all the tokens that
	// contain the position, scoped to the narrowest top-level declaration (message, service, etc.)
	narrowestLen := uint32(math.MaxUint32)

	for _, token := range tokens {
		if token.lang != tokenLanguageProto {
			// ignore non-proto tokens
			continue
		}
		if pos.Line != token.line {
			if token.line > pos.Line {
				// Stop searching once we've passed the line
				break
			}
			continue // Skip tokens not on the same line
		}
		if pos.Character < token.start || pos.Character > token.start+token.len {
			continue // Skip tokens that don't contain the position
		}
		if token.len < narrowestLen {
			// Found a narrower token
			narrowest, narrowestLen = token, token.len
			found = true

			if _, isTerminal := token.node.(ast.TerminalNode); isTerminal {
				break
			}
		}
	}

	return
}

func (s *semanticItems) mktokens(node ast.Node, path []ast.Node, tt tokenType, mods tokenModifier) {
	info := s.parseRes.AST().NodeInfo(node)
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
	lineInfo := s.parseRes.AST().NodeInfo(str)
	lineStart := lineInfo.Start()
	// lineEnd := lineInfo.End()

	// quoteStartTk := semanticItem{
	// 	lang:  tokenLanguageCel,
	// 	line:  uint32(lineStart.Line - 1),
	// 	start: uint32(lineStart.Col - 1),
	// 	len:   1,
	// 	typ:   semanticTypeOperator,
	// 	mods:  0,
	// }
	// quoteEndTk := semanticItem{
	// 	lang:  tokenLanguageCel,
	// 	line:  uint32(lineStart.Line - 1),
	// 	start: uint32(lineEnd.Col - 1),
	// 	len:   1,
	// 	typ:   semanticTypeOperator,
	// 	mods:  0,
	// }

	nodeTk := semanticItem{
		lang:  tokenLanguageCel,
		line:  uint32(lineStart.Line - 1),
		start: uint32(int32(lineStart.Col) + start),
		len:   uint32(end - start),
		typ:   tt,
		mods:  mods,
	}
	// s.items = append(s.items, quoteStartTk, nodeTk, quoteEndTk)
	s.items = append(s.items, nodeTk)
}

func (s *semanticItems) mkcomments(node ast.Node) {
	if s.options.skipComments {
		return
	}

	info := s.parseRes.AST().NodeInfo(node)

	leadingComments := info.LeadingComments()
	for i := 0; i < leadingComments.Len(); i++ {
		comment := leadingComments.Index(i)
		cstart, cend := comment.Start(), comment.End()
		if cend.Line > cstart.Line {
			s.multilineComment(comment, cstart, cend)
			continue
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
		DoVisitSyntaxNode: func(node *ast.SyntaxNode) error {
			s.mktokens(node.Keyword, append(tracker.Path(), node.Keyword), semanticTypeKeyword, 0)
			s.mktokens(node.Equals, append(tracker.Path(), node.Equals), semanticTypeOperator, 0)
			s.mktokens(node.Syntax, append(tracker.Path(), node.Syntax), semanticTypeString, 0)
			return nil
		},
		DoVisitFileNode: func(node *ast.FileNode) error {
			s.mkcomments(node.EOF)
			return nil
		},
		DoVisitStringLiteralNode: func(node *ast.StringLiteralNode) error {
			if _, ok := embeddedStringLiterals[node]; ok {
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
			case '}', '{', '.', ',', '<', '>', '(', ')', '[', ']', ';':
				s.mkcomments(node)
			default:
				s.mktokens(node, tracker.Path(), semanticTypeOperator, 0)
			}
			return nil
		},
		DoVisitExtendNode: func(en *ast.ExtendNode) error {
			s.mktokens(en.Extendee, append(tracker.Path(), en.Extendee), semanticTypeType, semanticModifierDefaultLibrary)
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
		DoVisitFieldNode: func(node *ast.FieldNode) error {
			var modifier tokenModifier
			if id := string(node.FldType.AsIdentifier()); protocompile.IsScalarType(id) || protocompile.IsWellKnownType(protoreflect.FullName(id)) {
				modifier = semanticModifierDefaultLibrary
			}
			s.mktokens(node.FldType, append(tracker.Path(), node.FldType), semanticTypeType, modifier)
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeVariable, semanticModifierDefinition)
			return nil
		},
		DoVisitFieldReferenceNode: func(node *ast.FieldReferenceNode) error {
			if node.IsAnyTypeReference() {
				s.mktokens(node.URLPrefix, append(tracker.Path(), node.URLPrefix), semanticTypeType, semanticModifierDefaultLibrary)
				s.mktokens(node.Slash, append(tracker.Path(), node.Slash), semanticTypeType, semanticModifierDefaultLibrary)
				s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeType, 0)
			} else if node.IsExtension() {
				s.mktokens(node.Open, append(tracker.Path(), node.Open), semanticTypeOperator, 0)
				s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeVariable, semanticModifierStatic)
				s.mktokens(node.Close, append(tracker.Path(), node.Close), semanticTypeOperator, 0)
			} else {
				s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeParameter, 0)
			}
			return nil
		},
		// DoVisitOptionNode: func(node *ast.OptionNode) error {
		// 	if node.Name != nil {
		// 		if len(node.Name.Parts) >= 2 &&
		// 			strings.HasPrefix(string(node.Name.Parts[0].Name.AsIdentifier()), "buf.validate.") &&
		// 			node.Name.Parts[1].Name.AsIdentifier() == "cel" {
		// 			for _, n := range node.Name.Children() {
		// 				fr, ok := n.(*ast.FieldReferenceNode)
		// 				if !ok {
		// 					continue
		// 				}
		// 				if fr.Name.AsIdentifier() != "cel" {
		// 					continue
		// 				}
		// 				if messageLit, ok := node.Val.(*ast.MessageLiteralNode); ok {
		// 					for _, lit := range s.inspectCelExpr(messageLit) {
		// 						embeddedStringLiterals[lit] = struct{}{}
		// 					}
		// 				}
		// 			}
		// 		}
		// 	}
		// 	return nil
		// },
		// DoVisitMessageFieldNode: func(node *ast.MessageFieldNode) error {
		// 	// <field reference>: <value>
		// 	return nil
		// },
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
				}
				if hasExpressionField && hasIdField {
					break
				}
			}
			if hasExpressionField && hasIdField {
				for _, lit := range s.inspectCelExpr(node) {
					embeddedStringLiterals[lit] = struct{}{}
				}
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
		DoVisitTerminalNode: func(node ast.TerminalNode) error {
			s.mkcomments(node)
			return nil
		},
	}, walkOptions...)
}

func (s *semanticItems) inspectCelExpr(messageLit *ast.MessageLiteralNode) []*ast.StringLiteralNode {
	for _, elem := range messageLit.Elements {
		if elem.Name.Name.AsIdentifier() == "expression" {
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
			parsed, issues := celEnv.Parse(celExpr)
			if issues != nil && issues.Err() != nil {
				return nil
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

			return stringNodes
		}
	}
	return nil
}

// there is no method to get the underlying ast i guess?? lol
func getAst(parsed *cel.Ast) *celast.AST {
	return (*struct {
		source celcommon.Source
		impl   *celast.AST
	})(unsafe.Pointer(parsed)).impl
}

func (e *semanticItems) Data() []uint32 {
	// binary operators, at least, will be out of order
	sort.Slice(e.items, func(i, j int) bool {
		if e.items[i].line != e.items[j].line {
			return e.items[i].line < e.items[j].line
		}
		return e.items[i].start < e.items[j].start
	})
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

package lsp

import (
	"math"
	"sort"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/parser"
	"golang.org/x/exp/slices"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
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

type semanticItem struct {
	line, start uint32 // 0-indexed
	len         uint32
	typ         tokenType
	mods        tokenModifier

	// An AST node associated with this token. Used for hover, definitions, etc.
	node ast.Node

	path []ast.Node
}

type semanticItems struct {
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

func (s *semanticItems) mkcomments(node ast.Node) {
	info := s.parseRes.AST().NodeInfo(node)
	leadingComments := info.LeadingComments()
	for i := 0; i < leadingComments.Len(); i++ {
		comment := leadingComments.Index(i)
		commentTk := semanticItem{
			line:  uint32(comment.Start().Line - 1),
			start: uint32(comment.Start().Col - 1),
			len:   uint32((comment.End().Col) - (comment.Start().Col - 1)),
			typ:   semanticTypeComment,
		}
		s.items = append(s.items, commentTk)
	}

	trailingComments := info.TrailingComments()
	for i := 0; i < trailingComments.Len(); i++ {
		comment := trailingComments.Index(i)
		commentTk := semanticItem{
			line:  uint32(comment.Start().Line - 1),
			start: uint32(comment.Start().Col - 1),
			len:   uint32((comment.End().Col) - (comment.Start().Col - 1)),
			typ:   semanticTypeComment,
		}
		s.items = append(s.items, commentTk)
	}
}

func (s *semanticItems) inspect(cache *Cache, node ast.Node, walkOptions ...ast.WalkOption) {
	tracker := &ast.AncestorTracker{}
	walkOptions = append(walkOptions, tracker.AsWalkOptions()...)
	// NB: when calling mktokens in composite node visitors:
	// - ensure node paths are manually adjusted if creating tokens for a child node
	// - ensure tokens for child nodes are created in the correct order
	ast.Walk(node, &ast.SimpleVisitor{
		DoVisitSyntaxNode: func(node *ast.SyntaxNode) error {
			s.mkcomments(node.Syntax)
			s.mktokens(node.Keyword, nil, semanticTypeKeyword, 0)
			s.mktokens(node.Equals, nil, semanticTypeOperator, 0)
			s.mktokens(node.Syntax, nil, semanticTypeString, 0)
			return nil
		},
		DoVisitFileNode: func(node *ast.FileNode) error {
			s.mkcomments(node.EOF)
			return nil
		},
		DoVisitStringLiteralNode: func(node *ast.StringLiteralNode) error {
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
			case '}', ';', '{', '.', ',', '<', '>', '(', ')':
				s.mkcomments(node)
			default:
				s.mktokens(node, tracker.Path(), semanticTypeOperator, 0)
			}
			return nil
		},
		DoVisitOneofNode: func(node *ast.OneofNode) error {
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeClass, 0)
			return nil
		},
		DoVisitMessageNode: func(node *ast.MessageNode) error {
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeClass, 0)
			return nil
		},
		DoVisitFieldNode: func(node *ast.FieldNode) error {
			s.mktokens(node.FldType, append(tracker.Path(), node.FldType), semanticTypeType, 0)
			s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeProperty, 0)
			return nil
		},
		DoVisitFieldReferenceNode: func(node *ast.FieldReferenceNode) error {
			if node.IsAnyTypeReference() {
				s.mktokens(node.URLPrefix, append(tracker.Path(), node.URLPrefix), semanticTypeNamespace, 0)
				s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeType, 0)
			} else if node.IsExtension() {
				s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeType, 0)
			} else {
				s.mktokens(node.Name, append(tracker.Path(), node.Name), semanticTypeProperty, 0)
			}
			return nil
		},
		// DoVisitOptionNode: func(node *ast.OptionNode) error {
		// 	return nil
		// },
		// DoVisitMessageFieldNode: func(node *ast.MessageFieldNode) error {
		// 	// <field reference>: <value>
		// 	return nil
		// },
		// DoVisitMessageLiteralNode: func(node *ast.MessageLiteralNode) error {
		// 	s.mktokens(node.Open, semanticTypeOperator, 0)
		// 	s.mktokens(node.Close, semanticTypeOperator, 0)
		// 	return nil
		// },
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

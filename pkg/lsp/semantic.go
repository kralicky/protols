package lsp

import (
	"math"
	"sort"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/parser"
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

type semItem struct {
	line, start uint32
	len         uint32
	typ         tokenType
	mods        tokenModifier

	// An AST node associated with this token. Used for hover, definitions, etc.
	node ast.Node
}

type encoded struct {
	// the generated data
	items []semItem

	res        parser.Result
	mapper     *protocol.Mapper
	rng        *protocol.Range
	start, end ast.Token
}

func semanticTokensFull(cache *Cache, doc protocol.TextDocumentIdentifier) (*protocol.SemanticTokens, error) {
	enc, err := computeSemanticTokens(cache, doc, nil)
	if err != nil {
		return nil, err
	}
	ret := &protocol.SemanticTokens{
		Data: enc.Data(),
	}
	return ret, err
}

func semanticTokensRange(cache *Cache, doc protocol.TextDocumentIdentifier, rng protocol.Range) (*protocol.SemanticTokens, error) {
	enc, err := computeSemanticTokens(cache, doc, &rng)
	if err != nil {
		return nil, err
	}
	ret := &protocol.SemanticTokens{
		Data: enc.Data(),
	}
	return ret, err
}

func computeSemanticTokens(cache *Cache, td protocol.TextDocumentIdentifier, rng *protocol.Range) (*encoded, error) {
	file, err := cache.FindParseResultByURI(td.URI.SpanURI())
	if err != nil {
		return nil, err
	}

	mapper, err := cache.GetMapper(td.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	a := file.AST()
	var startToken, endToken ast.Token
	if rng == nil {
		startToken = a.Start()
		endToken = a.End()
	} else {
		startOff, endOff, _ := mapper.RangeOffsets(*rng)
		startToken = a.TokenAtOffset(startOff)
		endToken = a.TokenAtOffset(endOff)
	}

	e := &encoded{
		rng:    rng,
		res:    file,
		mapper: mapper,
		start:  startToken,
		end:    endToken,
	}
	if a.Syntax != nil {
		start, end := a.Syntax.Start(), a.Syntax.End()
		if end >= e.start && start <= e.end {
			e.mkcomments(a.Syntax)
			e.mktokens(a.Syntax.Keyword, semanticTypeKeyword, 0)
			e.mktokens(a.Syntax.Equals, semanticTypeOperator, 0)
			e.mktokens(a.Syntax.Syntax, semanticTypeString, 0)
		}
	}
	for _, node := range a.Decls {
		// only look at the decls that overlap the range
		start, end := node.Start(), node.End()
		if end < e.start || start > e.end {
			continue
		}
		e.inspect(node)
	}
	if endToken == a.End() {
		e.mkcomments(a.EOF)
	}

	return e, nil
}

func findNarrowestSemanticToken(tokens []semItem, pos protocol.Position) (narrowest semItem, found bool) {
	// find the narrowest token that contains the position and also has a node
	// associated with it. The set of tokens will contain all the tokens that
	// contain the position, scoped to the narrowest top-level declaration (message, service, etc.)
	narrowestLen := uint32(math.MaxUint32)

	for _, token := range tokens {
		if pos.Line != token.line {
			continue // Skip tokens not on the same line
		}
		if pos.Character < token.start || pos.Character > token.start+token.len {
			continue // Skip tokens that don't contain the position
		}
		if token.len < narrowestLen {
			// Found a narrower token, update narrowest and narrowestLen
			narrowest, narrowestLen = token, token.len
			found = true
		}
	}

	return
}

func (s *encoded) mktokens(node ast.Node, tt tokenType, mods tokenModifier) {
	info := s.res.AST().NodeInfo(node)
	if !info.IsValid() {
		return
	}

	length := (info.End().Col - 1) - (info.Start().Col - 1)

	nodeTk := semItem{
		line:  uint32(info.Start().Line - 1),
		start: uint32(info.Start().Col - 1),
		len:   uint32(length),
		typ:   tt,
		mods:  mods,
		node:  node,
	}
	s.items = append(s.items, nodeTk)

	s.mkcomments(node)
}

func (s *encoded) mkcomments(node ast.Node) {
	info := s.res.AST().NodeInfo(node)
	leadingComments := info.LeadingComments()
	for i := 0; i < leadingComments.Len(); i++ {
		comment := leadingComments.Index(i)
		commentTk := semItem{
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
		commentTk := semItem{
			line:  uint32(comment.Start().Line - 1),
			start: uint32(comment.Start().Col - 1),
			len:   uint32((comment.End().Col) - (comment.Start().Col - 1)),
			typ:   semanticTypeComment,
		}
		s.items = append(s.items, commentTk)
	}
}

func (s *encoded) inspect(node ast.Node) {
	ast.Walk(node, &ast.SimpleVisitor{
		DoVisitStringLiteralNode: func(node *ast.StringLiteralNode) error {
			s.mktokens(node, semanticTypeString, 0)
			return nil
		},
		DoVisitUintLiteralNode: func(node *ast.UintLiteralNode) error {
			s.mktokens(node, semanticTypeNumber, 0)
			return nil
		},
		DoVisitFloatLiteralNode: func(node *ast.FloatLiteralNode) error {
			s.mktokens(node, semanticTypeNumber, 0)
			return nil
		},
		DoVisitSpecialFloatLiteralNode: func(node *ast.SpecialFloatLiteralNode) error {
			s.mktokens(node, semanticTypeNumber, 0)
			return nil
		},
		DoVisitKeywordNode: func(node *ast.KeywordNode) error {
			s.mktokens(node, semanticTypeKeyword, 0)
			return nil
		},
		DoVisitRuneNode: func(node *ast.RuneNode) error {
			switch node.Rune {
			case '}', ';', '{', '.', ',', '<', '>', '(', ')':
				s.mkcomments(node)
			default:
				s.mktokens(node, semanticTypeOperator, 0)
			}
			return nil
		},
		DoVisitOneofNode: func(node *ast.OneofNode) error {
			s.mktokens(node.Name, semanticTypeClass, 0)
			return nil
		},
		DoVisitMessageNode: func(node *ast.MessageNode) error {
			s.mktokens(node.Name, semanticTypeClass, 0)
			return nil
		},
		DoVisitFieldNode: func(node *ast.FieldNode) error {
			s.mktokens(node.Name, semanticTypeProperty, 0)
			s.mktokens(node.FldType, semanticTypeType, 0)
			return nil
		},
		DoVisitFieldReferenceNode: func(node *ast.FieldReferenceNode) error {
			if node.IsExtension() {
				if node.IsAnyTypeReference() {
					s.mktokens(node.URLPrefix, semanticTypeType, 0)
					s.mktokens(node.Slash, semanticTypeType, 0)
					s.mktokens(node.Name, semanticTypeType, 0)
				} else {
					s.mktokens(node.Name, semanticTypeType, 0)
				}
			} else {
				s.mktokens(node.Name, semanticTypeProperty, 0)
			}
			return nil
		},
		DoVisitMapFieldNode: func(node *ast.MapFieldNode) error {
			s.mktokens(node.Name, semanticTypeProperty, 0)
			s.mktokens(node.MapType.KeyType, semanticTypeType, 0)
			s.mktokens(node.MapType.ValueType, semanticTypeType, 0)
			return nil
		},
		DoVisitRPCTypeNode: func(node *ast.RPCTypeNode) error {
			s.mktokens(node.MessageType, semanticTypeType, 0)
			return nil
		},
		DoVisitRPCNode: func(node *ast.RPCNode) error {
			s.mktokens(node.Name, semanticTypeFunction, 0)
			return nil
		},
		DoVisitServiceNode: func(sn *ast.ServiceNode) error {
			s.mktokens(sn.Name, semanticTypeClass, 0)
			return nil
		},
		DoVisitPackageNode: func(node *ast.PackageNode) error {
			s.mktokens(node.Name, semanticTypeNamespace, 0)
			return nil
		},
		DoVisitEnumNode: func(node *ast.EnumNode) error {
			s.mktokens(node.Name, semanticTypeClass, 0)
			return nil
		},
		DoVisitEnumValueNode: func(node *ast.EnumValueNode) error {
			s.mktokens(node.Name, semanticTypeEnumMember, 0)
			return nil
		},
		DoVisitTerminalNode: func(node ast.TerminalNode) error {
			s.mkcomments(node)
			return nil
		},
	})
}

func (e *encoded) Data() []uint32 {
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
	var last semItem
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

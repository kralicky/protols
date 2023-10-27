package lsp

import (
	"github.com/bufbuild/protocompile/ast"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
)

type ranger interface {
	Start() ast.SourcePos
	End() ast.SourcePos
}

func toRange[T ranger](t T) protocol.Range {
	return positionsToRange(t.Start(), t.End())
}

func adjustColumns(r protocol.Range, leftAdjust int, rightAdjust int) protocol.Range {
	return protocol.Range{
		Start: protocol.Position{
			Line:      r.Start.Line,
			Character: r.Start.Character + uint32(leftAdjust),
		},
		End: protocol.Position{
			Line:      r.End.Line,
			Character: r.End.Character + uint32(rightAdjust),
		},
	}
}

func positionsToRange(start, end ast.SourcePos) protocol.Range {
	return protocol.Range{
		Start: protocol.Position{
			Line:      uint32(start.Line - 1),
			Character: uint32(start.Col - 1),
		},
		End: protocol.Position{
			Line:      uint32(end.Line - 1),
			Character: uint32(end.Col - 1),
		},
	}
}

package lsp

import (
	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
)

type ranger interface {
	Start() ast.SourcePos
	End() ast.SourcePos
}

func toRange[T ranger](t T) protocol.Range {
	return positionsToRange(t.Start(), t.End())
}

func toPosition(pos ast.SourcePos) protocol.Position {
	return protocol.Position{
		Line:      uint32(pos.Line - 1),
		Character: uint32(pos.Col - 1),
	}
}

func positionsToRange(start, end ast.SourcePos) protocol.Range {
	return protocol.Range{
		Start: toPosition(start),
		End:   toPosition(end),
	}
}

func pointToRange(point ast.SourcePos) protocol.Range {
	return protocol.Range{
		Start: toPosition(point),
		End:   toPosition(point),
	}
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

func adjustColumn(r protocol.Position, adjust int) protocol.Position {
	return protocol.Position{
		Line:      r.Line,
		Character: r.Character + uint32(adjust),
	}
}

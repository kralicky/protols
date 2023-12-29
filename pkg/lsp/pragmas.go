package lsp

import "github.com/kralicky/protocompile/ast"

func init() {
	ast.PragmaKey = "protols"
}

var PragmaNoFormat = "nofmt"

package test

import (
	"testing"

	"github.com/kralicky/protols/pkg/lsp"
)

func TestMain(m *testing.M) {
	lsp.DebugCheckOverlappingTokens = true
	Main(m)
}

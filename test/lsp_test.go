package test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/kralicky/protols/pkg/lsp"
	"github.com/kralicky/tools-lite/gopls/pkg/test/integration"
	"github.com/kralicky/tools-lite/gopls/pkg/test/integration/fake"
)

func TestMain(m *testing.M) {
	lsp.DebugCheckOverlappingTokens = true
	Main(m)
}

func TestBasic(t *testing.T) {
	const src = `
-- go.mod --
module example.com

go 1.22
-- main.go --
package main

func main() {}

-- test.proto --
syntax = "proto3";

package test;

message Test {
	string test = 1;
}

`
	Run(t, src, func(t *testing.T, env *integration.Env) {
		env.OpenFile("test.proto")
		tokens := env.SemanticTokensFull("test.proto")
		want := []fake.SemanticToken{
			{Token: "syntax", TokenType: "keyword"},
			{Token: "=", TokenType: "operator"},
			{Token: `"proto3"`, TokenType: "string"},
			{Token: "package", TokenType: "keyword"},
			{Token: "test", TokenType: "namespace"},
			{Token: "message", TokenType: "keyword"},
			{Token: "Test", TokenType: "type", Mod: "definition"},
			{Token: "string", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "test", TokenType: "variable", Mod: "definition"},
			{Token: "=", TokenType: "operator"},
			{Token: "1", TokenType: "number"},
		}
		if x := cmp.Diff(want, tokens); x != "" {
			t.Errorf("Semantic tokens do not match (-want +got):\n%s", x)
		}
	})
}

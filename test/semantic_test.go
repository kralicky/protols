package test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/kralicky/tools-lite/gopls/pkg/test/integration"
	"github.com/kralicky/tools-lite/gopls/pkg/test/integration/fake"
)

func TestSemanticTokens(t *testing.T) {
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
	string test = 1 [json_name = "test"];
	int32 test2 = 2 [default = 5, e = "e" "e" "e"];
	message Nested {
		option (a) = "b";
		option (b) = 5;
		option (c) = true;
		option (d) = -inf;
	}
}

extend google.protobuf.MessageOptions {
	string a = 50000;
	int32 b = 50001;
	bool c = 50002;
	float d = 50003;
}

extend google.protobuf.FieldOptions {
	string e = 50004;
}
-- test2.proto --
syntax = "proto2";

package bufbuild.protocompile.test2;

message X {
  optional X x = 1;
  extensions 100 to max;
}

extend X {
  optional X y = 100;
}

extend google.protobuf.FieldOptions {
  optional X x = 1000;
}

message Y {
  optional X x = 1 [
    (.bufbuild.protocompile.test2.x).x.(y).x.(y).(y).x.x.(y).x = {
      x: {[bufbuild.protocompile.test2.y]: {x: {}}}
    }
  ];
	optional bytes escaped_bytes = 3 [default = "\0\001\a\b\f\n\r\t\v\\\'\"\xfe"];
	optional string utf8_string = 4 [default = "\341\210\264"]; // this is utf-8 for \u1234
	optional string mixed_string = 5 [default = "foo\xFFbar\u1234baz\t"];
}

`
	Run(t, src, func(t *testing.T, env *integration.Env) {
		t.Skip()
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
			{Token: "json_name", TokenType: "keyword"},
			{Token: "=", TokenType: "operator"},
			{Token: `"test"`, TokenType: "string"},
			{Token: "int32", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "test2", TokenType: "variable", Mod: "definition"},
			{Token: "=", TokenType: "operator"},
			{Token: "2", TokenType: "number"},
			{Token: "default", TokenType: "keyword"},
			{Token: "=", TokenType: "operator"},
			{Token: "5", TokenType: "number"},
			{Token: "e", TokenType: "property"},
			{Token: "=", TokenType: "operator"},
			{Token: `"e"`, TokenType: "string"},
			{Token: `"e"`, TokenType: "string"},
			{Token: `"e"`, TokenType: "string"},
			{Token: "message", TokenType: "keyword"},
			{Token: "Nested", TokenType: "type", Mod: "definition"},
			{Token: "option", TokenType: "keyword"},
			{Token: "(", TokenType: "operator"},
			{Token: "a", TokenType: "property"},
			{Token: ")", TokenType: "operator"},
			{Token: "=", TokenType: "operator"},
			{Token: `"b"`, TokenType: "string"},
			{Token: "option", TokenType: "keyword"},
			{Token: "(", TokenType: "operator"},
			{Token: "b", TokenType: "property"},
			{Token: ")", TokenType: "operator"},
			{Token: "=", TokenType: "operator"},
			{Token: "5", TokenType: "number"},
			{Token: "option", TokenType: "keyword"},
			{Token: "(", TokenType: "operator"},
			{Token: "c", TokenType: "property"},
			{Token: ")", TokenType: "operator"},
			{Token: "=", TokenType: "operator"},
			{Token: "true", TokenType: "keyword"},
			{Token: "option", TokenType: "keyword"},
			{Token: "(", TokenType: "operator"},
			{Token: "d", TokenType: "property"},
			{Token: ")", TokenType: "operator"},
			{Token: "=", TokenType: "operator"},
			{Token: "-", TokenType: "operator"},
			{Token: "inf", TokenType: "number"},
			{Token: "extend", TokenType: "keyword"},
			{Token: "google.protobuf.MessageOptions", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "string", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "a", TokenType: "variable", Mod: "definition"},
			{Token: "=", TokenType: "operator"},
			{Token: "50000", TokenType: "number"},
			{Token: "int32", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "b", TokenType: "variable", Mod: "definition"},
			{Token: "=", TokenType: "operator"},
			{Token: "50001", TokenType: "number"},
			{Token: "bool", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "c", TokenType: "variable", Mod: "definition"},
			{Token: "=", TokenType: "operator"},
			{Token: "50002", TokenType: "number"},
			{Token: "float", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "d", TokenType: "variable", Mod: "definition"},
			{Token: "=", TokenType: "operator"},
			{Token: "50003", TokenType: "number"},
			{Token: "extend", TokenType: "keyword"},
			{Token: "google.protobuf.FieldOptions", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "string", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "e", TokenType: "variable", Mod: "definition"},
			{Token: "=", TokenType: "operator"},
			{Token: "50004", TokenType: "number"},
		}
		if x := cmp.Diff(want, tokens); x != "" {
			t.Errorf("Semantic tokens do not match (-want +got):\n%s", x)
		}
	})
	Run(t, src, func(t *testing.T, env *integration.Env) {
		env.OpenFile("test2.proto")
		tokens := env.SemanticTokensFull("test2.proto")
		want := []fake.SemanticToken{
			{Token: "syntax", TokenType: "keyword"},
			{Token: "=", TokenType: "operator"},
			{Token: `"proto2"`, TokenType: "string"},
			{Token: "package", TokenType: "keyword"},
			{Token: "bufbuild.protocompile.test2", TokenType: "namespace"},
			{Token: "message", TokenType: "keyword"},
			{Token: "X", TokenType: "type", Mod: "definition"},
			{Token: "optional", TokenType: "keyword"},
			{Token: "X", TokenType: "type"},
			{Token: "x", TokenType: "variable", Mod: "definition"},
			{Token: "=", TokenType: "operator"},
			{Token: "1", TokenType: "number"},
			{Token: "extensions", TokenType: "keyword"},
			{Token: "100", TokenType: "number"},
			{Token: "to", TokenType: "keyword"},
			{Token: "max", TokenType: "keyword"},
			{Token: "extend", TokenType: "keyword"},
			{Token: "X", TokenType: "type"},
			{Token: "optional", TokenType: "keyword"},
			{Token: "X", TokenType: "type"},
			{Token: "y", TokenType: "variable", Mod: "definition"},
			{Token: "=", TokenType: "operator"},
			{Token: "100", TokenType: "number"},
			{Token: "extend", TokenType: "keyword"},
			{Token: "google.protobuf.FieldOptions", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "optional", TokenType: "keyword"},
			{Token: "X", TokenType: "type"},
			{Token: "x", TokenType: "variable", Mod: "definition"},
			{Token: "=", TokenType: "operator"},
			{Token: "1000", TokenType: "number"},
			{Token: "message", TokenType: "keyword"},
			{Token: "Y", TokenType: "type", Mod: "definition"},
			{Token: "optional", TokenType: "keyword"},
			{Token: "X", TokenType: "type"},
			{Token: "x", TokenType: "variable", Mod: "definition"},
			{Token: "=", TokenType: "operator"},
			{Token: "1", TokenType: "number"},
			{Token: "(", TokenType: "operator"},
			{Token: ".bufbuild.protocompile.test2.x", TokenType: "property"},
			{Token: ")", TokenType: "operator"},
			{Token: ".", TokenType: "property"},
			{Token: "x", TokenType: "property"},
			{Token: ".", TokenType: "property"},
			{Token: "(", TokenType: "operator"},
			{Token: "y", TokenType: "property"},
			{Token: ")", TokenType: "operator"},
			{Token: ".", TokenType: "property"},
			{Token: "x", TokenType: "property"},
			{Token: ".", TokenType: "property"},
			{Token: "(", TokenType: "operator"},
			{Token: "y", TokenType: "property"},
			{Token: ")", TokenType: "operator"},
			{Token: ".", TokenType: "property"},
			{Token: "(", TokenType: "operator"},
			{Token: "y", TokenType: "property"},
			{Token: ")", TokenType: "operator"},
			{Token: ".", TokenType: "property"},
			{Token: "x", TokenType: "property"},
			{Token: ".", TokenType: "property"},
			{Token: "x", TokenType: "property"},
			{Token: ".", TokenType: "property"},
			{Token: "(", TokenType: "operator"},
			{Token: "y", TokenType: "property"},
			{Token: ")", TokenType: "operator"},
			{Token: ".", TokenType: "property"},
			{Token: "x", TokenType: "property"},
			{Token: "=", TokenType: "operator"},
			{Token: "x", TokenType: "property"},
			{Token: "[", TokenType: "operator"},
			{Token: "bufbuild.protocompile.test2.y", TokenType: "property"},
			{Token: "]", TokenType: "operator"},
			{Token: "x", TokenType: "property"},
			{Token: "optional", TokenType: "keyword"},
			{Token: "bytes", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "escaped_bytes", TokenType: "variable", Mod: "definition"},
			{Token: "=", TokenType: "operator"},
			{Token: "3", TokenType: "number"},
			{Token: "default", TokenType: "keyword"},
			{Token: "=", TokenType: "operator"},
			{Token: `"`, TokenType: "string"},
			{Token: `\0`, TokenType: "regexp"},
			{Token: `\001`, TokenType: "regexp"},
			{Token: `\a`, TokenType: "regexp"},
			{Token: `\b`, TokenType: "regexp"},
			{Token: `\f`, TokenType: "regexp"},
			{Token: `\n`, TokenType: "regexp"},
			{Token: `\r`, TokenType: "regexp"},
			{Token: `\t`, TokenType: "regexp"},
			{Token: `\v`, TokenType: "regexp"},
			{Token: `\\`, TokenType: "regexp"},
			{Token: `\'`, TokenType: "regexp"},
			{Token: `\"`, TokenType: "regexp"},
			{Token: `\xfe`, TokenType: "regexp"},
			{Token: `"`, TokenType: "string"},
			{Token: "optional", TokenType: "keyword"},
			{Token: "string", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "utf8_string", TokenType: "variable", Mod: "definition"},
			{Token: "=", TokenType: "operator"},
			{Token: "4", TokenType: "number"},
			{Token: "default", TokenType: "keyword"},
			{Token: "=", TokenType: "operator"},
			{Token: `"`, TokenType: "string"},
			{Token: `\341`, TokenType: "regexp"},
			{Token: `\210`, TokenType: "regexp"},
			{Token: `\264`, TokenType: "regexp"},
			{Token: `"`, TokenType: "string"},
			{Token: "// this is utf-8 for \\u1234", TokenType: "comment"},
			{Token: "optional", TokenType: "keyword"},
			{Token: "string", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "mixed_string", TokenType: "variable", Mod: "definition"},
			{Token: "=", TokenType: "operator"},
			{Token: "5", TokenType: "number"},
			{Token: "default", TokenType: "keyword"},
			{Token: "=", TokenType: "operator"},
			{Token: `"`, TokenType: "string"},
			{Token: "foo", TokenType: "string"},
			{Token: `\xFF`, TokenType: "regexp"},
			{Token: "bar", TokenType: "string"},
			{Token: `\u1234`, TokenType: "regexp"},
			{Token: "baz", TokenType: "string"},
			{Token: `\t`, TokenType: "regexp"},
			{Token: `"`, TokenType: "string"},
		}
		if x := cmp.Diff(want, tokens); x != "" {
			t.Errorf("Semantic tokens do not match (-want +got):\n%s", x)
		}
	})
}

func TestComments(t *testing.T) {
	const src = `
-- test.proto --
extend .google. /* comment */ protobuf.ExtensionRangeOptions {
  optional string label = 20000;
}`
	Run(t, src, func(t *testing.T, env *integration.Env) {
		env.OpenFile("test.proto")
		tokens := env.SemanticTokensFull("test.proto")
		want := []fake.SemanticToken{
			{Token: "extend", TokenType: "keyword"},
			{Token: ".", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "google", TokenType: "type", Mod: "defaultLibrary"},
			{Token: ".", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "/* comment */", TokenType: "comment"},
			{Token: "protobuf", TokenType: "type", Mod: "defaultLibrary"},
			{Token: ".", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "ExtensionRangeOptions", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "optional", TokenType: "keyword"},
			{Token: "string", TokenType: "type", Mod: "defaultLibrary"},
			{Token: "label", TokenType: "variable", Mod: "definition"},
			{Token: "=", TokenType: "operator"},
			{Token: "20000", TokenType: "number"},
		}
		if x := cmp.Diff(want, tokens); x != "" {
			t.Errorf("Semantic tokens do not match (-want +got):\n%s", x)
		}
	})
}

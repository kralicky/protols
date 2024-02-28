package format_test

import (
	"strings"
	"testing"

	"github.com/kralicky/protocompile/parser"
	"github.com/kralicky/protocompile/reporter"
	"github.com/kralicky/protols/pkg/format"
	"github.com/stretchr/testify/require"
)

func TestFormat(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		0: {
			input: `
message Simple {
   optional string name = 1;
   optional uint64 id = 2;
   optional bytes _extra = 3; // default JSON name will be capitalized
   repeated bool _ = 4; // default JSON name will be empty(!)
}`[1:],
			want: `
message Simple {
  optional string name   = 1;
  optional uint64 id     = 2;
  optional bytes  _extra = 3; // default JSON name will be capitalized
  repeated bool   _      = 4; // default JSON name will be empty(!)
}`[1:],
		},
		1: {
			input: `
extend . google. // identifier broken up strangely should still be accepted
  protobuf .
   ExtensionRangeOptions {
	optional string label = 20000;
}`[1:],
			want: `
extend .google. /* identifier broken up strangely should still be accepted */protobuf.ExtensionRangeOptions {
  optional string label = 20000;
}`[1:],
		},
		2: {
			input: `
message Test {
	optional string foo = 1 [json_name = "|foo|"];
	repeated int32 array = 2;
	optional Simple s = 3;
	repeated Simple r = 4;
	map<string, int32> m = 5;

	optional bytes b = 6 [default = "\0\1\2\3\4\5\6\7fubar!"];
	repeated float floats = 7;
	repeated bool bools = 8;
	repeated Test.Nested._NestedNested.EEE enums = 9;
}`[1:],
			want: `
message Test {
  optional string    foo   = 1 [json_name = "|foo|"];
  repeated int32     array = 2;
  optional Simple    s     = 3;
  repeated Simple    r     = 4;
  map<string, int32> m     = 5;

  optional bytes                         b      = 6 [default = "\0\1\2\3\4\5\6\7fubar!"];
  repeated float                         floats = 7;
  repeated bool                          bools  = 8;
  repeated Test.Nested._NestedNested.EEE enums  = 9;
}`[1:],
		},
		3: {
			input: `
message Another {
	option (.foo.bar.rept) = {
		foo: "abc" s < name: "foo", id: 123 >, array: [1, 2 ,3], r:[<name:"f">, {name:"s"}, {id:456} ],
	};
	option (foo.bar.rept) = {
		foo: "def" s { name: "bar", id: 321 }, array: [3, 2 ,1], r:{name:"g"} r:{name:"s"}};
	option (rept) = { foo: "def" };
	option (eee) = V1;
option (a) = { fff: OK };
option (a).test = { m { key: "foo" value: 100 } m { key: "bar" value: 200 }};
option (a).test.foo = "m&m";
option (a).test.s.name = "yolo";
	option (a).test.s.id = 98765;
	option (a).test.array = 1;
	option (a).test.array = 2;
	option (a).test.(.foo.bar.Test.Nested._NestedNested._garblez2) = "whoah!";
}`[1:],
			want: `
message Another {
  option (.foo.bar.rept) = {
    foo:   "abc"
    s:     <name: "foo", id: 123>
    array: [1, 2, 3]
    r:     [<name: "f">, {name: "s"}, {id: 456}]
  };
  option (foo.bar.rept) = {
    foo:   "def"
    s:     {name: "bar", id: 321}
    array: [3, 2, 1]
    r:     {name: "g"}
    r:     {name: "s"}
  };
  option (rept)                                                  = {foo: "def"};
  option (eee)                                                   = V1;
  option (a)                                                     = {fff: OK};
  option (a).test                                                = {m: {key: "foo", value: 100}, m: {key: "bar", value: 200}};
  option (a).test.foo                                            = "m&m";
  option (a).test.s.name                                         = "yolo";
  option (a).test.s.id                                           = 98765;
  option (a).test.array                                          = 1;
  option (a).test.array                                          = 2;
  option (a).test.(.foo.bar.Test.Nested._NestedNested._garblez2) = "whoah!";
}`[1:],
		},
		4: {
			input: `option (.foo.bar.rept) = { r:[<name:"f">, {name:"s"}, {id:456} ], };`,
			want: `
option (.foo.bar.rept) = {r: [<name: "f">, {name: "s"}, {id: 456}]};`[1:],
		},
		5: {
			input: `option (.foo.bar.rept) = { foo: "abc" s < name: "foo", id: 123 >, array: [1, 2 ,3], };`,
			want:  `option (.foo.bar.rept) = {foo: "abc", s: <name: "foo", id: 123>, array: [1, 2, 3]};`,
		},
		6: {
			input: `option (.foo.bar.rept) = {
				foo: "abc" s < name: "foo", id: 123 >, array: [1, 2 ,3], };`,
			want: `
option (.foo.bar.rept) = {
  foo:   "abc"
  s:     <name: "foo", id: 123>
  array: [1, 2, 3]
};`[1:],
		},
		7: {
			input: `option (.foo.bar.rept) = {
				foo: "abc" s < name: "foo", id: 123 >, array: [
					1, 2 ,3], };`,
			want: `
option (.foo.bar.rept) = {
  foo: "abc"
  s:   <name: "foo", id: 123>
  array: [
    1,
    2,
    3
  ]
};`[1:],
		},
		8: {
			input: `
option (foo.bar.rept) = {
  foo: "def"
  s:   {name: "bar", id: 321}
  array: [      3,
    2,
    1
  ]
  r: {name: "g"}
  r: {name: "s"}
};`[1:],
			want: `
option (foo.bar.rept) = {
  foo:   "def"
  s:     {name: "bar", id: 321}
  array: [3, 2, 1]
  r:     {name: "g"}
  r:     {name: "s"}
};`[1:],
		},
	}

	for _, c := range cases[8:] {
		root, err := parser.Parse("", strings.NewReader(c.input), reporter.NewHandler(nil), 0)
		require.NoError(t, err)

		got, err := format.PrintNode(root, root)
		require.NoError(t, err)

		require.Equal(t, c.want, got)
	}
}

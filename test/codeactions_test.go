package test

import (
	"testing"

	"github.com/kralicky/tools-lite/gopls/pkg/protocol"
	"github.com/kralicky/tools-lite/gopls/pkg/test/integration"
	"github.com/stretchr/testify/require"
)

func TestSimplifyRepeatedOptions(t *testing.T) {
	const src = `
-- options.proto --
import "google/protobuf/descriptor.proto";

message MapTest {
  map<string, string> kvs = 1;
}

extend google.protobuf.FieldOptions {
  optional MapTest mapTest = 1234;
}

extend google.protobuf.MessageOptions {
  optional MapTest mapTest2 = 1235;
}

message Foo {
  optional string field1 = 1 [
    (mapTest) = {
      kvs: [
        {key: "key1", value: "value1"},
        {key: "key2", value: "value2"}
      ],
    }
  ];

  option (mapTest2) = {
    kvs: [
      {key: "a", value: "1"}
    ],
    kvs: [
      {key: "b", value: "2"}
    ],
    kvs: {key: "c", value: "3"}, // c3
  };
  option (mapTest2).kvs = { key: "d" , value:"4" };
  option (mapTest2).kvs = {
    key:   "e", // e
    value: "5", // 5
  };
  option (mapTest2).kvs = {
    key:   "f", // f
    value: "6", // 6
  };
}
`
	const want = `
import "google/protobuf/descriptor.proto";

message MapTest {
  map<string, string> kvs = 1;
}

extend google.protobuf.FieldOptions {
  optional MapTest mapTest = 1234;
}

extend google.protobuf.MessageOptions {
  optional MapTest mapTest2 = 1235;
}

message Foo {
  optional string field1 = 1 [
    (mapTest) = {
      kvs: [
        {key: "key1", value: "value1"},
        {key: "key2", value: "value2"}
      ],
    }
  ];

  option (mapTest2) = {
    kvs: [
      {key: "a", value: "1"},
      {key: "b", value: "2"},
      {key: "c", value: "3"}, // c3
      {key: "d", value: "4"},
      {
        key:   "e", // e
        value: "5", // 5
      },
      {
        key:   "f", // f
        value: "6", // 6
      }
    ],
  };
}
`

	for _, location := range []string{
		`option \(mapTest2\)\.()kvs = { key`,
		`option \(mapTest2\)\.()kvs = {\n`,
		`()kvs: \[\n      \{key: "a",`,
		`()kvs: \[\n      \{key: "b",`,
		`()kvs: \{key: "c"`,
	} {
		Run(t, src, func(t *testing.T, env *integration.Env) {
			env.OpenFile("options.proto")
			var diag protocol.PublishDiagnosticsParams
			env.OnceMet(
				integration.Diagnostics(integration.ForFile("options.proto")),
				integration.ReadDiagnostics("options.proto", &diag),
			)
			require.Equal(t, 1, len(diag.Diagnostics), "expected 1 diagnostic, got %v", len(diag.Diagnostics))
			require.Equal(t, diag.Diagnostics[0].Message, "no syntax specified; defaulting to proto2 syntax")
			actions, err := env.Editor.CodeActions(env.Ctx, env.RegexpSearch("options.proto", location), nil, protocol.RefactorRewrite)
			require.NoError(t, err)
			require.Len(t, actions, 1)
			env.ApplyCodeAction(actions[0])
			env.SaveBuffer("options.proto")
			require.Equal(t, want[1:], env.BufferText("options.proto"))
		})
	}
}

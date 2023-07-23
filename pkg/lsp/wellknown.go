package lsp

// import some extra well-known types
import (
	_ "google.golang.org/genproto/googleapis/api/annotations"
	_ "google.golang.org/genproto/googleapis/api/httpbody"
	_ "google.golang.org/genproto/googleapis/api/label"
	_ "google.golang.org/genproto/googleapis/rpc/code"
	_ "google.golang.org/genproto/googleapis/rpc/context"
	_ "google.golang.org/genproto/googleapis/rpc/context/attribute_context"
	_ "google.golang.org/genproto/googleapis/rpc/errdetails"
	_ "google.golang.org/genproto/googleapis/rpc/http"
	_ "google.golang.org/genproto/googleapis/rpc/status"
)

var (
	wellKnownFileOptions = map[string]string{
		"java_package":                  "string",
		"java_outer_classname":          "string",
		"java_multiple_files":           "bool",
		"java_generate_equals_and_hash": "bool",
		"java_string_check_utf8":        "bool",
		"optimize_for":                  "google.protobuf.FileOptions.OptimizeMode",
		"go_package":                    "string",
		"cc_generic_services":           "bool",
		"java_generic_services":         "bool",
		"py_generic_services":           "bool",
		"php_generic_services":          "bool",
		"deprecated":                    "bool",
		"cc_enable_arenas":              "bool",
		"objc_class_prefix":             "string",
		"csharp_namespace":              "string",
		"swift_prefix":                  "string",
		"php_class_prefix":              "string",
		"php_namespace":                 "string",
		"php_metadata_namespace":        "string",
		"ruby_package":                  "string",
	}
)

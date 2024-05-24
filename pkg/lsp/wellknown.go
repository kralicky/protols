package lsp

// import some extra well-known types
import (
	"strings"

	_ "buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	_ "buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate/priv"
	_ "google.golang.org/genproto/googleapis/api/annotations"
	_ "google.golang.org/genproto/googleapis/api/configchange"
	_ "google.golang.org/genproto/googleapis/api/distribution"
	_ "google.golang.org/genproto/googleapis/api/error_reason"
	_ "google.golang.org/genproto/googleapis/api/expr/v1beta1"
	_ "google.golang.org/genproto/googleapis/api/httpbody"
	_ "google.golang.org/genproto/googleapis/api/label"
	_ "google.golang.org/genproto/googleapis/api/metric"
	_ "google.golang.org/genproto/googleapis/api/monitoredres"
	_ "google.golang.org/genproto/googleapis/api/serviceconfig"
	_ "google.golang.org/genproto/googleapis/api/visibility"
	_ "google.golang.org/genproto/googleapis/privacy/dlp/v2"
	_ "google.golang.org/genproto/googleapis/rpc/code"
	_ "google.golang.org/genproto/googleapis/rpc/context"
	_ "google.golang.org/genproto/googleapis/rpc/context/attribute_context"
	_ "google.golang.org/genproto/googleapis/rpc/errdetails"
	_ "google.golang.org/genproto/googleapis/rpc/http"
	_ "google.golang.org/genproto/googleapis/rpc/status"
	_ "google.golang.org/genproto/googleapis/type/calendarperiod"
	_ "google.golang.org/genproto/googleapis/type/color"
	_ "google.golang.org/genproto/googleapis/type/date"
	_ "google.golang.org/genproto/googleapis/type/datetime"
	_ "google.golang.org/genproto/googleapis/type/dayofweek"
	_ "google.golang.org/genproto/googleapis/type/decimal"
	_ "google.golang.org/genproto/googleapis/type/expr"
	_ "google.golang.org/genproto/googleapis/type/fraction"
	_ "google.golang.org/genproto/googleapis/type/interval"
	_ "google.golang.org/genproto/googleapis/type/latlng"
	_ "google.golang.org/genproto/googleapis/type/localized_text"
	_ "google.golang.org/genproto/googleapis/type/money"
	_ "google.golang.org/genproto/googleapis/type/month"
	_ "google.golang.org/genproto/googleapis/type/phone_number"
	_ "google.golang.org/genproto/googleapis/type/postaladdress"
	_ "google.golang.org/genproto/googleapis/type/quaternion"
	_ "google.golang.org/genproto/googleapis/type/timeofday"
)

var wellKnownFileOptions = map[string]string{
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

func IsWellKnownPath(path string) bool {
	return strings.HasPrefix(path, "google/")
}

var wellKnownModuleImports = []string{
	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate/validate.proto",
}

package grpc

import (
	"google.golang.org/protobuf/compiler/protogen"

	_ "google.golang.org/genproto/googleapis/api/annotations"
	_ "google.golang.org/genproto/googleapis/rpc/code"
	_ "google.golang.org/genproto/googleapis/rpc/context"
	_ "google.golang.org/genproto/googleapis/rpc/context/attribute_context"
	_ "google.golang.org/genproto/googleapis/rpc/errdetails"
	_ "google.golang.org/genproto/googleapis/rpc/http"
	_ "google.golang.org/genproto/googleapis/rpc/status"
)

const version = "1.3.0"

var requireUnimplemented *bool

func SetRequireUnimplemented(req bool) {
	*requireUnimplemented = req
}

func init() {
	t := false
	requireUnimplemented = &t
}

var Generator = generator{}

type generator struct{}

func (generator) Name() string {
	return "go-grpc"
}

func (generator) Generate(gen *protogen.Plugin) error {
	for _, f := range gen.Files {
		if f.Generate {
			generateFile(gen, f)
		}
	}
	return nil
}

package grpc

import (
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"

	_ "google.golang.org/genproto/googleapis/api/annotations"
	_ "google.golang.org/genproto/googleapis/rpc/code"
	_ "google.golang.org/genproto/googleapis/rpc/context"
	_ "google.golang.org/genproto/googleapis/rpc/context/attribute_context"
	_ "google.golang.org/genproto/googleapis/rpc/errdetails"
	_ "google.golang.org/genproto/googleapis/rpc/http"
	_ "google.golang.org/genproto/googleapis/rpc/status"
)

const version = "1.5.1"

var Generator = grpcGenerator{}

type grpcGenerator struct{}

func (grpcGenerator) Name() string {
	return "go-grpc"
}

type generator struct {
	requireUnimplemented bool
	useGenericStreams    bool
}

func (grpcGenerator) Generate(plugin *protogen.Plugin) error {
	plugin.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL) | uint64(pluginpb.CodeGeneratorResponse_FEATURE_SUPPORTS_EDITIONS)
	plugin.SupportedEditionsMinimum = descriptorpb.Edition_EDITION_PROTO2
	plugin.SupportedEditionsMaximum = descriptorpb.Edition_EDITION_2023
	for _, file := range plugin.Files {
		if !file.Generate {
			continue
		}

		g := generator{
			requireUnimplemented: false,
			useGenericStreams:    true,
		}

		if proto.HasExtension(file.Desc.Options(), E_Generator) {
			ext := proto.GetExtension(file.Desc.Options(), E_Generator).(*GeneratorOptions)
			if ext.RequireUnimplemented != nil {
				g.requireUnimplemented = *ext.RequireUnimplemented
			}
			if ext.UseGenericStreams != nil {
				g.useGenericStreams = *ext.UseGenericStreams
			}
		}

		g.generateFile(plugin, file)
	}
	return nil
}

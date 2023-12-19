package golang

import (
	gengo "google.golang.org/protobuf/cmd/protoc-gen-go/internal_gengo"
	"google.golang.org/protobuf/compiler/protogen"
)

var Generator = generator{}

type generator struct{}

func (generator) Name() string {
	return "go"
}

func (generator) Generate(gen *protogen.Plugin) error {
	for _, f := range gen.Files {
		if f.Generate {
			gengo.GenerateFile(gen, f)
		}
	}
	return nil
}

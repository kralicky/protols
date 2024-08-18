//go:build mage

package main

import (
	"go/format"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
)

var Aliases = map[string]any{
	"install": Install.Release,
}

type Install mg.Namespace

func (Install) Release() error {
	return sh.RunWith(map[string]string{
		"CGO_ENABLED": "0",
	}, mg.GoCmd(), "install", "-ldflags", "-w -s", "-trimpath", "./cmd/protols")
}

func (Install) Debug() error {
	return sh.RunWith(map[string]string{
		"CGO_ENABLED": "0",
	}, mg.GoCmd(), "install", "-gcflags", "all=-N -l", "./cmd/protols")
}

func Test() error {
	return sh.RunV(mg.GoCmd(), "test", "-race", "./...")
}

type Maintenance mg.Namespace

func (Maintenance) SyncUpstreamGrpcGen() error {
	const upstreamUrl = "https://raw.githubusercontent.com/grpc/grpc-go/master/cmd/protoc-gen-go-grpc/grpc.go"
	const filename = "sdk/codegen/generators/golang/grpc/upstream_gen.go"

	resp, err := http.Get(upstreamUrl)
	if err != nil {
		return err
	}
	upstreamBytes, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}

	upstream := string(upstreamBytes)
	upstream = strings.Replace(upstream, "package main", "package grpc", 1)
	// remove serviceGenerateHelper type
	upstream = strings.Replace(upstream, "type serviceGenerateHelper struct{}", "", 1)
	// remove global helper instance
	upstream = strings.Replace(upstream, "var helper serviceGenerateHelperInterface = serviceGenerateHelper{}", "", 1)
	// convert all serviceGenerateHelper methods to generator methods
	upstream = strings.ReplaceAll(upstream, "func (serviceGenerateHelper) ", "func (x_ generator)")
	// replace usages of global helper methods with generator methods
	upstream = strings.ReplaceAll(upstream, "helper.", "x_.")
	// replace usages of global variables with generator scoped variables
	upstream = strings.ReplaceAll(upstream, "*requireUnimplemented", "x_.requireUnimplemented")
	upstream = strings.ReplaceAll(upstream, "*useGenericStreams", "x_.useGenericStreams")

	// find all unexported functions in $filename
	matches := regexp.MustCompile(`(?m)^func ([a-z][a-zA-Z]+)\(`).FindAllStringSubmatch(upstream, -1)

	for _, match := range matches {
		funcName := match[1]
		// prefix existing usages with 'x_.'
		upstream = strings.ReplaceAll(upstream, funcName+"(", "x_."+funcName+"(")
		// convert the function to be a generator method
		upstream = strings.ReplaceAll(upstream, "func x_."+funcName, "func (x_ generator) "+funcName)
		// fix up doc comments
		upstream = strings.ReplaceAll(upstream, "// x_."+funcName, "// "+funcName)
	}

	formattedBytes, err := format.Source([]byte(upstream))
	if err != nil {
		return err
	}
	return os.WriteFile(filename, formattedBytes, 0644)
}

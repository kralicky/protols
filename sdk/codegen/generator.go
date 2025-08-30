package codegen

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	_ "github.com/kralicky/codegen/cli"
	_ "github.com/kralicky/codegen/pathbuilder"
	"github.com/kralicky/protols/pkg/sources"
	"github.com/kralicky/protols/sdk/codegen/generators/golang"
	"github.com/kralicky/protols/sdk/codegen/generators/golang/grpc"
	"github.com/kralicky/protols/sdk/driver"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/gofeaturespb"
	"google.golang.org/protobuf/types/pluginpb"
)

type Generator interface {
	Name() string
	Generate(gen *protogen.Plugin) error
}

type GeneratedFile struct {
	// Basename of the generated file.
	Name string
	// Path where this file can be written to, such that it will be in the same
	// directory as the source proto it was generated from. Calling WriteToDisk
	// will write the file to this path. This will be a relative path if
	// the source file was given as a relative path.
	SourceRelPath string
	// Go package (not including the file name) defined in the source proto.
	Package string
	// Generated file content.
	Content string
}

func (g *GeneratedFile) Read(p []byte) (int, error) {
	return copy(p, g.Content), nil
}

func (g *GeneratedFile) WriteToDisk() error {
	return os.WriteFile(g.SourceRelPath, []byte(g.Content), 0o644)
}

type GenerateStrategy int

const (
	// Generates only workspace-local descriptors
	WorkspaceLocalDescriptorsOnly GenerateStrategy = iota
	// Generates all descriptors, including those from dependencies, except
	// package google.protobuf.
	AllDescriptorsExceptGoogleProtobuf
)

type GenerateCodeOptions struct {
	strategy             GenerateStrategy
	fixInvalidGoPackages bool
}

type GenerateCodeOption func(*GenerateCodeOptions)

func (o *GenerateCodeOptions) apply(opts ...GenerateCodeOption) {
	for _, op := range opts {
		op(o)
	}
}

func WithGenerateStrategy(strategy GenerateStrategy) GenerateCodeOption {
	return func(o *GenerateCodeOptions) {
		o.strategy = strategy
	}
}

func WithFixInvalidGoPackages(fixPackagePaths bool) GenerateCodeOption {
	return func(o *GenerateCodeOptions) {
		o.fixInvalidGoPackages = fixPackagePaths
	}
}

// Generates code for each source file found in the given search directories,
// using one or more code generators.
func GenerateCode(generators []Generator, searchDirs []string, opts ...GenerateCodeOption) ([]*GeneratedFile, error) {
	options := &GenerateCodeOptions{
		strategy: WorkspaceLocalDescriptorsOnly,
	}
	options.apply(opts...)

	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for i, dir := range searchDirs {
		if !filepath.IsAbs(dir) {
			searchDirs[i] = filepath.Join(wd, dir)
		}
	}
	driver := driver.NewDriver(wd, driver.WithRenameStrategy(driver.RestoreExternalGoModuleDescriptorNames))
	results, err := driver.Compile(sources.SearchDirs(searchDirs...))
	if err != nil {
		return nil, err
	}

	if results.Error {
		return nil, fmt.Errorf("errors occurred during compilation")
	}

	sourcePkgDirs := map[string]string{}
	for _, desc := range results.AllDescriptors {
		uri := results.FileURIsByPath[desc.Path()]
		if uri.IsFile() {
			sourcePkgDirs[filepath.Dir(desc.Path())] = filepath.Dir(uri.Path())
		}
	}

	toGenerate := []string{}
	switch options.strategy {
	case WorkspaceLocalDescriptorsOnly:
		for _, desc := range results.WorkspaceLocalDescriptors {
			dir := sourcePkgDirs[filepath.Dir(desc.Path())]
			for _, searchDir := range searchDirs {
				if strings.HasPrefix(dir, searchDir) {
					toGenerate = append(toGenerate, desc.Path())
					break
				}
			}
		}

	case AllDescriptorsExceptGoogleProtobuf:
		for _, desc := range results.AllDescriptors {
			if desc.Package() == "google.protobuf" {
				continue
			}
			toGenerate = append(toGenerate, desc.Path())
		}
	}

	if options.fixInvalidGoPackages {
		for _, file := range results.AllDescriptorProtos {
			goPackage := file.GetOptions().GetGoPackage()
			if goPackage != "" && !strings.Contains(goPackage, ".") && !strings.Contains(goPackage, "/") {
				*file.Options.GoPackage = path.Dir(file.GetName())
			}
		}
	}

	plugin, err := (protogen.Options{
		DefaultAPILevel: gofeaturespb.GoFeatures_API_OPEN,
	}).New(&pluginpb.CodeGeneratorRequest{
		FileToGenerate: toGenerate,
		ProtoFile:      results.AllDescriptorProtos,
	})
	if err != nil {
		return nil, err
	}

	for _, g := range generators {
		if err := g.Generate(plugin); err != nil {
			return nil, err
		}
	}

	response := plugin.Response()
	if response.Error != nil {
		return nil, errors.New(response.GetError())
	}

	var outputs []*GeneratedFile
	for _, f := range response.GetFile() {
		pkg, name := filepath.Split(f.GetName())
		pkg = strings.TrimSuffix(pkg, "/")
		dir, ok := sourcePkgDirs[pkg]
		if !ok {
			if strings.Contains(pkg, "google/") {
				dir = pkg[strings.Index(pkg, "google/"):]
			} else {
				dir = pkg
			}
		}
		relPath := path.Join(dir, name)
		outputs = append(outputs, &GeneratedFile{
			Name:          name,
			Package:       pkg,
			SourceRelPath: relPath,
			Content:       f.GetContent(),
		})
	}

	return outputs, nil
}

func DefaultGenerators() []Generator {
	return []Generator{
		golang.Generator,
		grpc.Generator,
	}
}

func GenerateWorkspace() error {
	files, err := GenerateCode(
		DefaultGenerators(),
		[]string{"."},
		WithGenerateStrategy(WorkspaceLocalDescriptorsOnly),
	)
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := file.WriteToDisk(); err != nil {
			return err
		}
	}
	return nil
}

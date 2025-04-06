package plugin

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	sdkutil "github.com/kralicky/protols/sdk/util"

	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protols/pkg/lsp"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/gofeaturespb"
	"google.golang.org/protobuf/types/pluginpb"
)

type plugin struct {
	*protogen.Plugin

	enumsByName    map[protoreflect.FullName]*protogen.Enum
	messagesByName map[protoreflect.FullName]*protogen.Message
}

func New(targets linker.Files, closure []linker.Result, pathMappings lsp.PathMappings) (*protogen.Plugin, error) {
	implicitGoPackagesByDir := make(map[string]string)
	var errs []string

	resultProtos := make([]*descriptorpb.FileDescriptorProto, len(closure))
RESULTS:
	for i, desc := range closure {
		fdp := proto.Clone(desc.FileDescriptorProto()).(*descriptorpb.FileDescriptorProto)

		sanitizePkg := func(goPackage string) string {
			// fix up any incomplete go_package options if we have the info available
			// this will transform e.g. `go_package = "bar"` to `go_package = "github.com/foo/bar"`
			if !strings.Contains(goPackage, ".") && !strings.Contains(goPackage, "/") {
				p := path.Dir(desc.Path())
				if strings.HasSuffix(p, goPackage) {
					return p
				} else {
					return fmt.Sprintf("%s;%s", p, goPackage)
				}
			}
			return goPackage
		}
		if fdp.Options == nil {
			fdp.Options = &descriptorpb.FileOptions{}
		}
		if fdp.Options.GoPackage != nil {
			goPackage := fdp.Options.GetGoPackage()
			if sanitized := sanitizePkg(goPackage); sanitized != goPackage {
				*fdp.Options.GoPackage = sanitized
			}
		} else {
			// if there is no go_package option:
			// - if any other files in the same directory have a go_package option,
			//   and are in the same package, use their go_package.
			// - if sibling files have different packages, we can't infer a go_package
			//   and must skip the file.
			// - iff all files in the directory have no go_package option and all
			//   have the same package, use the directory name as the package name.
			pathDir := path.Dir(desc.Path())
			var canInfer bool
			if _, ok := implicitGoPackagesByDir[pathDir]; !ok {
				canInfer = true
				for _, otherDesc := range closure {
					if otherDesc == desc {
						continue
					}
					if path.Dir(otherDesc.Path()) != pathDir {
						continue
					}

					otherFileOptions := otherDesc.Options().(*descriptorpb.FileOptions)
					if otherFileOptions != nil && otherFileOptions.GoPackage != nil {
						canInfer = false
						if desc.Package() == otherDesc.Package() {
							implicitGoPackagesByDir[pathDir] = sanitizePkg(otherFileOptions.GetGoPackage())
							break
						} else {
							a, b := desc.Package(), otherDesc.Package()
							if b < a {
								a, b = b, a
							}
							errs = append(errs, fmt.Sprintf("could not infer go_package for %s: inconsistent package names in directory %s (%s, %s)", desc.Path(), pathDir, a, b))
							continue RESULTS
						}
					} else if desc.Package() != otherDesc.Package() {
						a, b := desc.Package(), otherDesc.Package()
						if b < a {
							a, b = b, a
						}
						errs = append(errs, fmt.Sprintf("inconsistent package names in directory %s (%s, %s)", pathDir, a, b))
						continue RESULTS
					}
				}
			}
			if _, ok := implicitGoPackagesByDir[pathDir]; !ok {
				// no go_package options in any files in this directory
				if canInfer {
					implicitGoPackagesByDir[pathDir] = pathDir
				}
			}
			if ipath, ok := implicitGoPackagesByDir[pathDir]; ok {
				fdp.Options.GoPackage = &ipath
			}
		}

		resultProtos[i] = fdp
	}
	if errs != nil {
		slices.Sort(errs)
		errs = slices.Compact(errs)
		return nil, errors.New(strings.Join(errs, "; \n"))
	}
	sdkutil.RestoreDescriptorNames(resultProtos, pathMappings)

	filesToGenerate := make([]string, 0, len(targets))
	for _, t := range targets {
		filesToGenerate = append(filesToGenerate, t.Path())
	}

	protogenPlugin, err := protogen.Options{
		DefaultAPILevel: gofeaturespb.GoFeatures_API_OPEN,
	}.New(&pluginpb.CodeGeneratorRequest{
		ProtoFile:      resultProtos,
		FileToGenerate: filesToGenerate,
	})
	if err != nil {
		panic(err)
	}
	return protogenPlugin, nil
}

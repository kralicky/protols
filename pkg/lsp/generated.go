package lsp

import (
	"context"
	"fmt"
	"go/ast"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/kralicky/protocompile"
	"github.com/kralicky/protols/pkg/x/protogen/strs"
	"github.com/kralicky/tools-lite/gopls/pkg/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (c *Cache) FindGeneratedDefinition(ctx context.Context, params protocol.TextDocumentPositionParams) ([]protocol.Location, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	desc, _, err := c.FindTypeDescriptorAtLocation(params)
	if err != nil {
		return nil, fmt.Errorf("no generated definition found: %w", err)
	}

	parentUri, err := c.resolver.PathToURI(desc.ParentFile().Path())
	if err != nil {
		return nil, fmt.Errorf("no generated definition found: %w", err)
	}

	genFiles, err := c.resolver.FindGeneratedFiles(parentUri, desc.ParentFile())
	if err != nil {
		return nil, fmt.Errorf("no generated definition found: %w", err)
	}

	var locations []protocol.Location
	switch desc := desc.(type) {
	case protoreflect.ServiceDescriptor:
		for _, gen := range genFiles {
			spec := lookupTypeSpec(gen, desc, "%sServer")
			if spec != nil {
				locations = append(locations, nodeLocation(gen, spec.Name))
			}
		}
	case protoreflect.MethodDescriptor:
		descIdent := GoIdent(desc)
		if parentSvc, ok := desc.Parent().(protoreflect.ServiceDescriptor); ok {
			for _, gen := range genFiles {
				if spec := lookupTypeSpec(gen, parentSvc, "%sServer"); spec != nil {
					if intf, ok := spec.Type.(*ast.InterfaceType); ok {
						for _, mtd := range intf.Methods.List {
							if len(mtd.Names) == 1 && mtd.Names[0].Name == descIdent {
								locations = append(locations, nodeLocation(gen, mtd.Names[0]))
								break
							}
						}
					}
				}
			}
		}
	case protoreflect.MessageDescriptor:
		for _, gen := range genFiles {
			spec := lookupTypeSpec(gen, desc, "%s")
			if spec != nil {
				locations = append(locations, nodeLocation(gen, spec.Name))
			}
		}
	case protoreflect.EnumDescriptor:
		for _, gen := range genFiles {
			spec := lookupTypeSpec(gen, desc, "%s")
			if spec != nil {
				locations = append(locations, nodeLocation(gen, spec.Name))
			}
		}
	case protoreflect.EnumValueDescriptor:
		if parentEnum, ok := desc.Parent().(protoreflect.EnumDescriptor); ok {
			// names of enum values are different for top-level enums and enums
			// nested in messages.
			// see newEnumValue() in protobuf-go/compiler/protogen/protogen.go
			var identPrefix string
			switch container := parentEnum.Parent().(type) {
			case protoreflect.MessageDescriptor:
				identPrefix = GoIdent(container)
			case protoreflect.FileDescriptor:
				identPrefix = GoIdent(parentEnum)
			}
			for _, gen := range genFiles {
				spec := lookupValueSpec(gen, desc, identPrefix+"_%s")
				if spec != nil {
					locations = append(locations, nodeLocation(gen, spec.Names[0]))
				}
			}
		}
	case protoreflect.FieldDescriptor:
		if desc.IsExtension() {
			// look up E_<name> var
			for _, gen := range genFiles {
				if spec := lookupValueSpec(gen, desc, "E_%s"); spec != nil {
					locations = append(locations, nodeLocation(gen, spec.Names[0]))
				}
			}
		} else {
			descIdent := GoIdent(desc)
			for _, gen := range genFiles {
				if msgSpec := lookupTypeSpec(gen, desc.ContainingMessage(), "%s"); msgSpec != nil {
					if structType, ok := msgSpec.Type.(*ast.StructType); ok {
						for _, field := range structType.Fields.List {
							if len(field.Names) == 1 && field.Names[0].Name == descIdent {
								locations = append(locations, nodeLocation(gen, field.Names[0]))
								break
							}
						}
					}
				}
			}
		}
	}

	if len(locations) == 0 {
		return nil, fmt.Errorf("no generated definition found for %s", desc.FullName())
	}
	return locations, nil
}

func nodeLocation(gen ParsedGoFile, node ast.Node) protocol.Location {
	return protocol.Location{
		URI: protocol.URIFromPath(gen.Filename),
		Range: protocol.Range{
			Start: gen.Position(node.Pos()),
			End:   gen.Position(node.End()),
		},
	}
}

func lookupObjDecl[T ast.Node](pf ParsedGoFile, desc protoreflect.Descriptor, formatStr string) T {
	obj := pf.Scope.Lookup(fmt.Sprintf(formatStr, GoIdent(desc)))
	if obj == nil {
		var zero T
		return zero
	}
	t, _ := obj.Decl.(T)
	return t
}

var (
	lookupTypeSpec  = lookupObjDecl[*ast.TypeSpec]
	lookupValueSpec = lookupObjDecl[*ast.ValueSpec]
)

func GoIdent(desc protoreflect.Descriptor) string {
	switch desc := desc.(type) {
	case protoreflect.EnumValueDescriptor:
		// enum values are not camel-cased
		return strs.GoSanitized(string(desc.Name()))
	case protoreflect.FieldDescriptor, protoreflect.MethodDescriptor:
		return strs.GoCamelCase(string(desc.Name()))
	default:
		name := strs.GoCamelCase(strings.TrimPrefix(string(desc.FullName()), string(desc.ParentFile().Package())+"."))
		return name
	}
}

func tryResolvePathToGeneratedImport(files []ParsedGoFile, unresolvedPath string, whence protocompile.ImportContext) (string, error) {
	// if there is a unique go import path which suffix-matches `path.Dir(path)`, use it
	pathSuffix := path.Dir(unresolvedPath)
	var candidates []string
	for _, f := range files {
		for _, imp := range f.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if strings.HasSuffix(importPath, pathSuffix) && !slices.Contains(candidates, importPath) {
				candidates = append(candidates, importPath)
			}
		}
	}
	if len(candidates) == 1 {
		return path.Join(candidates[0], path.Base(unresolvedPath)), nil
	}

	// I wrote the comment below late at night a while ago but can't remember
	// what exactly the situation was or why i came to that conclusion, because
	// this does not seem like an issue. keeping it here for posterity though

	// handle files that import other files in the same package - the go code generator
	// would not generate import statements for these files, since they are in the same package.
	// example cloud.google.com/go/monitoring/apiv3/v2/monitoringpb/metric.proto imports
	// common.proto but it's in the same package, so the generated go code does not import it.
	// however, we need it to resolve some type references.

	return "", fmt.Errorf("could not determine previously-generated import path for %s", unresolvedPath)
}

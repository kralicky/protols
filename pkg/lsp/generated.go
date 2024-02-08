package lsp

import (
	"context"
	"fmt"
	"go/ast"
	"strings"

	"github.com/kralicky/protols/pkg/x/protogen/strs"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (c *Cache) FindGeneratedDefinition(ctx context.Context, params protocol.TextDocumentPositionParams) ([]protocol.Location, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	linkRes, err := c.FindResultByURI(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	desc, _, err := c.FindTypeDescriptorAtLocation(params)
	if err != nil {
		return nil, err
	}

	genFiles, err := c.resolver.FindGeneratedFiles(params.TextDocument.URI, linkRes)
	if err != nil {
		return nil, err
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
			parentEnumIdent := GoIdent(parentEnum)
			for _, gen := range genFiles {
				spec := lookupValueSpec(gen, desc, parentEnumIdent+"_%s")
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
	case protoreflect.FieldDescriptor, protoreflect.MethodDescriptor:
		return strs.GoCamelCase(string(desc.Name()))
	default:
		return strs.GoCamelCase(strings.TrimPrefix(string(desc.FullName()), string(desc.ParentFile().Package())+"."))
	}
}

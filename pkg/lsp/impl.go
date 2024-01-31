package lsp

import (
	"fmt"
	"go/ast"
	"strings"

	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (c *Cache) FindImplementations(params protocol.TextDocumentPositionParams) ([]protocol.Location, error) {
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
	case protoreflect.FieldDescriptor:
		return GoCamelCase(string(desc.Name()))
	default:
		return GoCamelCase(strings.TrimPrefix(string(desc.FullName()), string(desc.ParentFile().Package())+"."))
	}
}

// ========================================================
// code below copied from protobuf/internal/strs/strings.go
// ========================================================

// GoCamelCase camel-cases a protobuf name for use as a Go identifier.
//
// If there is an interior underscore followed by a lower case letter,
// drop the underscore and convert the letter to upper case.
func GoCamelCase(s string) string {
	// Invariant: if the next letter is lower case, it must be converted
	// to upper case.
	// That is, we process a word at a time, where words are marked by _ or
	// upper case letter. Digits are treated as words.
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.' && i+1 < len(s) && isASCIILower(s[i+1]):
			// Skip over '.' in ".{{lowercase}}".
		case c == '.':
			b = append(b, '_') // convert '.' to '_'
		case c == '_' && (i == 0 || s[i-1] == '.'):
			// Convert initial '_' to ensure we start with a capital letter.
			// Do the same for '_' after '.' to match historic behavior.
			b = append(b, 'X') // convert '_' to 'X'
		case c == '_' && i+1 < len(s) && isASCIILower(s[i+1]):
			// Skip over '_' in "_{{lowercase}}".
		case isASCIIDigit(c):
			b = append(b, c)
		default:
			// Assume we have a letter now - if not, it's a bogus identifier.
			// The next word is a sequence of characters that must start upper case.
			if isASCIILower(c) {
				c -= 'a' - 'A' // convert lowercase to uppercase
			}
			b = append(b, c)

			// Accept lower case sequence that follows.
			for ; i+1 < len(s) && isASCIILower(s[i+1]); i++ {
				b = append(b, s[i+1])
			}
		}
	}
	return string(b)
}

func isASCIILower(c byte) bool {
	return 'a' <= c && c <= 'z'
}

func isASCIIUpper(c byte) bool {
	return 'A' <= c && c <= 'Z'
}

func isASCIIDigit(c byte) bool {
	return '0' <= c && c <= '9'
}

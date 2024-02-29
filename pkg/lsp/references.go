package lsp

import (
	"context"
	"slices"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/tools-lite/gopls/pkg/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (c *Cache) FindReferenceLocationsForTypeDescriptor(desc protoreflect.Descriptor) ([]protocol.Location, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	var locations []protocol.Location
	for span := range findNodeReferences(desc, c.results) {
		filename := span.NodeInfo.Start().Filename
		uri, err := c.resolver.PathToURI(filename)
		if err != nil {
			continue
		}
		locations = append(locations, protocol.Location{
			URI:   uri,
			Range: toRange(span.NodeInfo),
		})
	}
	return locations, nil
}

func (c *Cache) FindReferencesForTypeDescriptor(desc protoreflect.Descriptor) ([]ast.NodeReference, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	var refs []ast.NodeReference
	for node := range findNodeReferences(desc, c.results) {
		refs = append(refs, node)
	}
	return refs, nil
}

func (c *Cache) FindReferences(ctx context.Context, params protocol.TextDocumentPositionParams, refCtx protocol.ReferenceContext) ([]protocol.Location, error) {
	desc, _, err := c.FindTypeDescriptorAtLocation(params)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		if locations := c.TryFindPackageReferences(params); locations != nil {
			if !refCtx.IncludeDeclaration {
				locations = slices.DeleteFunc(locations, func(loc protocol.Location) bool {
					return loc.URI == params.TextDocument.URI
				})
			}
			return locations, nil
		}
		return nil, nil
	}

	var locations []protocol.Location

	if refCtx.IncludeDeclaration {
		if l, err := c.FindDefinitionForTypeDescriptor(desc); err == nil {
			locations = append(locations, l)
		} else {
			return nil, err
		}
	}

	refs, err := c.FindReferenceLocationsForTypeDescriptor(desc)
	if err != nil {
		return nil, err
	}
	locations = append(locations, refs...)
	return locations, nil
}

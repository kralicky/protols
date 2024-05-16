package lsp

import (
	"fmt"
	"strings"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protols/pkg/format"
	"github.com/kralicky/tools-lite/gopls/pkg/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (c *Cache) TryFindPackageReferences(params protocol.TextDocumentPositionParams) []protocol.Location {
	parseRes, err := c.FindParseResultByURI(params.TextDocument.URI)
	if err != nil {
		return nil
	}

	mapper, err := c.GetMapper(params.TextDocument.URI)
	if err != nil {
		return nil
	}

	offset, err := mapper.PositionOffset(params.Position)
	if err != nil {
		return nil
	}

	fileNode := parseRes.AST()

	tokenAtOffset, comment := fileNode.ItemAtOffset(offset)
	if tokenAtOffset == ast.TokenError || comment.IsValid() {
		return nil
	}

LOOP:
	for _, decl := range fileNode.Decls {
		if decl := decl.GetPackage(); decl != nil {
			if decl.Name != nil {
				if tokenAtOffset >= decl.Name.Start() && tokenAtOffset <= decl.Name.End() {
					switch name := decl.Name.Unwrap().(type) {
					case *ast.IdentNode:
						return c.FindPackageNameRefs(protoreflect.FullName(name.Val), false)
					case *ast.CompoundIdentNode:
						idents := name.FilterIdents()
						for i, ident := range idents {
							if tokenAtOffset >= ident.Start() && tokenAtOffset <= ident.End() {
								var parts []string
								for j := 0; j <= i; j++ {
									parts = append(parts, idents[j].Val)
								}
								return c.FindPackageNameRefs(protoreflect.FullName(strings.Join(parts, ".")), i < len(idents)-1)
							}
						}
					}
				}
			}
			break LOOP
		}
	}
	return nil
}

func (c *Cache) FindPackageNameRefs(name protoreflect.FullName, prefixMatch bool) []protocol.Location {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()

	var locations []protocol.Location
	searchFunc := c.results.RangeFilesByPackage
	if prefixMatch {
		searchFunc = c.results.RangeFilesByPackagePrefix
	}
	searchFunc(name, func(f linker.File) bool {
		if f.IsPlaceholder() {
			return true
		}
		uri, err := c.resolver.PathToURI(f.Path())
		if err != nil {
			return true
		}
		res := f.(linker.Result)
		resFileNode := res.AST()
		if resFileNode == nil {
			return true
		}
		for _, decl := range resFileNode.Decls {
			pkgNode := decl.GetPackage()
			if pkgNode == nil {
				continue
			}

			var rng protocol.Range
			switch node := pkgNode.Name.Unwrap().(type) {
			case *ast.IdentNode:
				if info := resFileNode.NodeInfo(node); info.IsValid() {
					rng = toRange(info)
				}
			case *ast.CompoundIdentNode:
				if !prefixMatch {
					if info := resFileNode.NodeInfo(node); info.IsValid() {
						rng = toRange(info)
					}
				} else {
					idents := node.FilterIdents()
					parts := make([]string, 0, len(idents))
					for i, part := range idents {
						parts = append(parts, part.Val)
						if strings.Join(parts, ".") == string(name) {
							start := resFileNode.NodeInfo(idents[0])
							end := resFileNode.NodeInfo(idents[i])
							if start.IsValid() && end.IsValid() {
								rng = protocol.Range{
									Start: toPosition(start.Start()),
									End:   toPosition(end.End()),
								}
							}
							break
						}
					}
				}
			}
			if rng == (protocol.Range{}) {
				return true
			}
			locations = append(locations, protocol.Location{
				URI:   uri,
				Range: rng,
			})
		}
		return true
	})
	return locations
}

func (c *Cache) tryHoverPackageNode(params protocol.TextDocumentPositionParams) *protocol.Hover {
	parseRes, err := c.FindParseResultByURI(params.TextDocument.URI)
	if err != nil {
		return nil
	}

	mapper, err := c.GetMapper(params.TextDocument.URI)
	if err != nil {
		return nil
	}

	offset, err := mapper.PositionOffset(params.Position)
	if err != nil {
		return nil
	}

	fileNode := parseRes.AST()

	tokenAtOffset, comment := fileNode.ItemAtOffset(offset)
	if tokenAtOffset == ast.TokenError || comment.IsValid() {
		return nil
	}

LOOP:
	for _, decl := range fileNode.Decls {
		if decl := decl.GetPackage(); decl != nil {
			if decl.Name == nil {
				break
			}

			makeStandardHover := func() *protocol.Hover {
				info := fileNode.NodeInfo(decl.Name)
				text, err := format.PrintNode(fileNode, decl)
				if err != nil {
					return nil
				}
				return &protocol.Hover{
					Contents: protocol.MarkupContent{
						Kind:  protocol.Markdown,
						Value: fmt.Sprintf("```protobuf\n%s\n```\n", text),
					},
					Range: toRange(info),
				}
			}

			switch name := decl.Name.Unwrap().(type) {
			case *ast.IdentNode:
				if tokenAtOffset >= decl.Name.Start() && tokenAtOffset <= decl.Name.End() {
					return makeStandardHover()
				}
			case *ast.CompoundIdentNode:
				parts := []string{}
				idents := name.FilterIdents()
				for i, part := range idents {
					parts = append(parts, part.Val)
					if tokenAtOffset >= part.Start() && tokenAtOffset <= part.End() {
						if i == len(idents)-1 {
							return makeStandardHover()
						}

						start := fileNode.NodeInfo(idents[0])
						end := fileNode.NodeInfo(idents[i])
						if start.IsValid() && end.IsValid() {
							partialName := protoreflect.FullName(strings.Join(parts, "."))
							return &protocol.Hover{
								Contents: protocol.MarkupContent{
									Kind:  protocol.Markdown,
									Value: fmt.Sprintf("```protobuf\npackage %s.*\n```\n", partialName),
								},
								Range: protocol.Range{
									Start: toPosition(start.Start()),
									End:   toPosition(end.End()),
								},
							}
						}
					}
				}
			}

			break LOOP
		}
	}
	return nil
}

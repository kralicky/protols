package lsp

import (
	"fmt"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (c *Cache) ComputeInlayHints(doc protocol.TextDocumentIdentifier, rng protocol.Range) ([]protocol.InlayHint, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	if ok, err := c.latestDocumentContentsWellFormedLocked(doc.URI, true); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("document contents not well formed")
	}

	hints := []protocol.InlayHint{}
	hints = append(hints, c.computeMessageLiteralHints(doc, rng)...)
	hints = append(hints, c.computeImportHints(doc, rng)...)
	return hints, nil
}

func (c *Cache) computeMessageLiteralHints(doc protocol.TextDocumentIdentifier, rng protocol.Range) []protocol.InlayHint {
	var hints []protocol.InlayHint
	res, err := c.FindResultByURI(doc.URI)
	if err != nil {
		return nil
	}
	mapper, err := c.GetMapper(doc.URI)
	if err != nil {
		return nil
	}
	a := res.AST()
	startOff, endOff, _ := mapper.RangeOffsets(rng)
	startToken := a.ItemAtOffset(startOff)
	endToken := a.ItemAtOffset(endOff)
	res.RangeFieldReferenceNodesWithDescriptors(func(node ast.Node, desc protoreflect.FieldDescriptor) bool {
		switch node := node.(type) {
		case *ast.FieldReferenceNode:
			if node.Start() > endToken || node.End() < startToken {
				return true
			}
			if node.IsAnyTypeReference() {
				return true
			}
			// insert before the closing )
			closeParen := node.Close
			if closeParen == nil {
				return true
			}
			info := a.NodeInfo(closeParen)
			if desc.Kind() == protoreflect.MessageKind && !desc.IsMap() && !desc.IsList() {
				fieldHint := protocol.InlayHint{
					Kind:         protocol.Type,
					PaddingLeft:  true,
					PaddingRight: false,
				}
				fieldHint.Position = protocol.Position{
					Line:      uint32(info.Start().Line) - 1,
					Character: uint32(info.Start().Col) - 1,
				}
				var location *protocol.Location
				if l, err := c.FindDefinitionForTypeDescriptor(desc.Message()); err == nil {
					location = &l
				}

				fieldHint.Label = append(fieldHint.Label, protocol.InlayHintLabelPart{
					Value: string(desc.Message().Name()),
					// Tooltip:  makeTooltip(desc.Message()),
					Location: location,
				})
				// fieldHint.PaddingLeft = false
				hints = append(hints, fieldHint)
			}
		}
		return true
	})
	return hints
}

func (c *Cache) computeImportHints(doc protocol.TextDocumentIdentifier, rng protocol.Range) []protocol.InlayHint {
	// show inlay hints for imports that resolve to different paths
	var hints []protocol.InlayHint
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()

	res, err := c.FindParseResultByURI(doc.URI)
	if err != nil {
		return nil
	}
	resAst := res.AST()
	if resAst == nil {
		return nil
	}
	var imports []*ast.ImportNode
	// get the source positions of the import statements
	for _, decl := range resAst.Decls {
		if imp, ok := decl.(*ast.ImportNode); ok {
			if imp.IsIncomplete() {
				continue
			}
			imports = append(imports, imp)
		}
	}

	dependencyPaths := res.FileDescriptorProto().Dependency
	// if the ast doesn't contain "google/protobuf/descriptor.proto" but the file descriptor does, filter it

	found := false
	for _, imp := range imports {
		if imp.Name.AsString() == "google/protobuf/descriptor.proto" {
			found = true
			break
		}
	}
	if !found {
		for i, dep := range dependencyPaths {
			if dep == "google/protobuf/descriptor.proto" {
				dependencyPaths = append(dependencyPaths[:i], dependencyPaths[i+1:]...)
				break
			}
		}
	}

	for i, imp := range imports {
		importPath := imp.Name.AsString()
		resolvedPath := dependencyPaths[i]
		nameInfo := resAst.NodeInfo(imp.Name)
		if resolvedPath != importPath {
			hints = append(hints, protocol.InlayHint{
				Kind:         protocol.Type,
				PaddingLeft:  true,
				PaddingRight: false,
				Position: protocol.Position{
					Line:      uint32(nameInfo.Start().Line) - 1,
					Character: uint32(nameInfo.End().Col) + 2,
				},
				TextEdits: []protocol.TextEdit{
					{
						Range:   adjustColumns(toRange(nameInfo), +1, -1),
						NewText: resolvedPath,
					},
				},
				Label: []protocol.InlayHintLabelPart{
					{
						Tooltip: &protocol.OrPTooltipPLabel{
							Value: fmt.Sprintf("Import resolves to %s", resolvedPath),
						},
						Value: resolvedPath,
					},
				},
			})
		}
	}

	return hints
}

func buildMessageLiteralHints(lit *ast.MessageLiteralNode, msg protoreflect.MessageDescriptor, a *ast.FileNode) []protocol.InlayHint {
	msgFields := msg.Fields()
	var hints []protocol.InlayHint
	for _, field := range lit.Elements {
		fieldDesc := msgFields.ByName(protoreflect.Name(field.Name.Value()))
		if fieldDesc == nil {
			continue
		}
		fieldHint := protocol.InlayHint{
			Kind:         protocol.Type,
			PaddingLeft:  true,
			PaddingRight: true,
		}
		kind := fieldDesc.Kind()
		if kind == protoreflect.MessageKind {
			info := a.NodeInfo(field.Val)
			fieldHint.Position = protocol.Position{
				Line:      uint32(info.Start().Line) - 1,
				Character: uint32(info.Start().Col) - 1,
			}
			fieldHint.Label = append(fieldHint.Label, protocol.InlayHintLabelPart{
				Value:   string(fieldDesc.Message().FullName()),
				Tooltip: makeTooltip(fieldDesc.Message()),
			})
			switch val := field.Val.(type) {
			case *ast.MessageLiteralNode:
				hints = append(hints, buildMessageLiteralHints(val, fieldDesc.Message(), a)...)
			case *ast.ArrayLiteralNode:
			default:
			}
			fieldHint.PaddingLeft = false
		}
		hints = append(hints, fieldHint)
	}
	return hints
}

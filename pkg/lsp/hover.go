package lsp

import (
	"fmt"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protols/pkg/format"
	"github.com/kralicky/tools-lite/gopls/pkg/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (c *Cache) ComputeHover(params protocol.TextDocumentPositionParams) (*protocol.Hover, error) {
	desc, rng, err := c.FindTypeDescriptorAtLocation(params)
	if err != nil {
		return nil, err
	} else if desc == nil {
		return c.tryHoverPackageNode(params), nil
	}

	location, err := c.FindDefinitionForTypeDescriptor(desc)
	if err != nil {
		return nil, err
	}

	parseRes, err := c.FindParseResultByURI(location.URI)
	if err != nil {
		return nil, err
	}

	mapper, err := c.GetMapper(location.URI)
	if err != nil {
		return nil, err
	}

	offset, err := mapper.PositionOffset(location.Range.Start)
	if err != nil {
		return nil, err
	}

	tokenAtOffset, comment := parseRes.AST().ItemAtOffset(offset)
	if tokenAtOffset == ast.TokenError || comment.IsValid() {
		return nil, nil
	}

	path, found := findPathIntersectingToken(parseRes, tokenAtOffset, location.Range.Start)
	if !found {
		return nil, nil
	}
	node := path.Index(-1).Value.Message().Interface().(ast.Node)
	text, err := format.PrintNode(parseRes.AST(), node)
	if err != nil {
		return nil, err
	}
	return &protocol.Hover{
		Contents: protocol.MarkupContent{
			Kind:  protocol.Markdown,
			Value: fmt.Sprintf("```protobuf\n%s\n```\n", text),
		},
		Range: rng,
	}, nil
}

func makeTooltip(d protoreflect.Descriptor) *protocol.OrPTooltipPLabel {
	str, err := format.PrintDescriptor(d)
	if err != nil {
		return nil
	}
	return &protocol.OrPTooltipPLabel{
		Value: protocol.MarkupContent{
			Kind:  protocol.Markdown,
			Value: fmt.Sprintf("```protobuf\n%s\n```\n", str),
		},
	}
}

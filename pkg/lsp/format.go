package lsp

import (
	"bytes"
	"fmt"

	"github.com/kralicky/protols/pkg/format"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
	"github.com/kralicky/tools-lite/pkg/diff"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (c *Cache) FormatDocument(doc protocol.TextDocumentIdentifier, options protocol.FormattingOptions, maybeRange ...protocol.Range) ([]protocol.TextEdit, error) {
	// check if the file has any parse errors; if it does, don't try to format
	// the document as we will end up erasing anything the user has typed
	// since the last time the document was successfully parsed.
	if ok, err := c.LatestDocumentContentsWellFormed(doc.URI, true); err != nil {
		return nil, err
	} else if !ok {
		return nil, nil
	}
	mapper, err := c.GetMapper(doc.URI)
	if err != nil {
		return nil, err
	}
	res, err := c.FindParseResultByURI(doc.URI)
	if err != nil {
		return nil, err
	}
	resAst := res.AST()
	if resAst == nil {
		return nil, nil
	}
	if _, ok := resAst.Pragma(PragmaNoFormat); ok {
		return nil, nil
	}
	// format whole file
	buf := bytes.NewBuffer(make([]byte, 0, len(mapper.Content)))
	format := format.NewFormatter(buf, res.AST())
	if err := format.Run(); err != nil {
		return nil, err
	}

	edits := diff.Bytes(mapper.Content, buf.Bytes())
	return protocol.EditsFromDiffEdits(mapper, edits)
}

func makeTooltip(d protoreflect.Descriptor) *protocol.OrPTooltipPLabel {
	str, err := format.PrintDescriptor(d)
	if err != nil {
		return nil
	}
	return &protocol.OrPTooltipPLabel{
		Value: protocol.MarkupContent{
			Kind:  protocol.Markdown,
			Value: fmt.Sprintf("```protobuf\n%s\n```", str),
		},
	}
}

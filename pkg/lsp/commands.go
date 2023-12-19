package lsp

import (
	"encoding/json"

	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
)

type SelectRangeParams struct {
	// A range in the current document that should be selected. Setting the
	// start and end position to the same location has the effect of
	// moving the cursor to that location.
	SelectRange protocol.Range `json:"selectRange"`
	// A range in the current document that will be revealed by the editor.
	RevealRange protocol.Range `json:"revealRange"`
	// Whether to highlight the revealed range.
	HighlightRevealedRange bool `json:"highlightRevealedRange"`
}

func NewSelectRangeCommand(params SelectRangeParams) *protocol.Command {
	paramsData, _ := json.Marshal(params)
	return &protocol.Command{
		Command: "protols.api.selectRange",
		Arguments: []json.RawMessage{
			json.RawMessage(paramsData),
		},
	}
}

type SyntheticFileContentsRequest struct {
	// The URI of the file to update.
	URI string `json:"uri"`
}

type DocumentASTRequest struct {
	// The URI of the file to retrieve the AST for.
	URI string `json:"uri"`
}

type ReindexWorkspacesRequest struct{}

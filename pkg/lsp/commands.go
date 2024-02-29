package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"

	"github.com/kralicky/protols/pkg/format"
	"github.com/kralicky/tools-lite/gopls/pkg/file"
	"github.com/kralicky/tools-lite/gopls/pkg/protocol"
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
	URI     string `json:"uri"`
	Version int32  `json:"version"`
}

type ReindexWorkspacesRequest struct{}

type RefreshModulesRequest struct {
	Workspace protocol.WorkspaceFolder `json:"workspace"`
}

type GeneratedDefinitionParams struct {
	protocol.TextDocumentPositionParams
}

type GenerateCodeRequest struct {
	// The URIs of the files to generate code for. All URIs in this list must
	// belong to the same workspace; the server will look at the first URI in
	// the list to determine which workspace to use.
	URIs []protocol.DocumentURI `json:"uris"`
}

type GenerateWorkspaceRequest struct {
	Workspace protocol.WorkspaceFolder `json:"workspace"`
}

type UnknownCommandHandler interface {
	Execute(ctx context.Context, uc UnknownCommand) (any, error)
}

type UnknownCommand struct {
	Command   string
	Arguments []json.RawMessage
	Cache     *Cache
}

// ExecuteCommand implements protocol.Server.
func (s *Server) ExecuteCommand(ctx context.Context, params *protocol.ExecuteCommandParams) (any, error) {
	switch params.Command {
	case "protols/syntheticFileContents":
		var req SyntheticFileContentsRequest
		if err := json.Unmarshal(params.Arguments[0], &req); err != nil {
			return nil, err
		}
		c, err := s.CacheForURI(protocol.DocumentURI(req.URI))
		if err != nil {
			return nil, err
		}
		return c.GetSyntheticFileContents(ctx, protocol.DocumentURI(req.URI))
	case "protols/ast":
		var req DocumentASTRequest
		if err := json.Unmarshal(params.Arguments[0], &req); err != nil {
			return nil, err
		}
		c, err := s.CacheForURI(protocol.DocumentURI(req.URI))
		if err != nil {
			return nil, err
		}
		if err := c.WaitDocumentVersion(ctx, protocol.DocumentURI(req.URI), req.Version); err != nil {
			return nil, err
		}
		parseRes, err := c.FindParseResultByURI(protocol.DocumentURI(req.URI))
		if err != nil {
			return nil, err
		}
		return format.DumpAST(parseRes.AST(), parseRes), nil
	case "protols/reindexWorkspaces":
		s.cachesMu.Lock()
		allWorkspaces := []protocol.WorkspaceFolder{}
		openOverlays := map[protocol.WorkspaceFolder][]file.Modification{}
		for _, c := range s.caches {
			allWorkspaces = append(allWorkspaces, c.workspace)

			for _, overlay := range c.resolver.Overlays() {
				df, err := c.resolver.OpenFileFromDisk(ctx, overlay.URI())
				if err != nil {
					slog.Error("failed to open file from disk", "uri", overlay.URI(), "err", err)
					continue
				}
				dfContent, err := df.Content()
				if err != nil {
					slog.Error("failed to read file from disk", "uri", overlay.URI(), "err", err)
					continue
				}
				openOverlays[c.workspace] = append(openOverlays[c.workspace], file.Modification{
					URI:        overlay.URI(),
					Action:     file.Open,
					OnDisk:     false,
					Version:    df.Version(),
					Text:       dfContent,
					LanguageID: "protobuf",
				})
				if !overlay.SameContentsOnDisk() {
					// generate an additional change event for the overlay
					editorContent, _ := overlay.Content() // always returns nil error
					openOverlays[c.workspace] = append(openOverlays[c.workspace],
						file.Modification{
							URI:     overlay.URI(),
							Action:  file.Change,
							OnDisk:  false,
							Version: max(overlay.Version(), 2),
							Text:    editorContent,
						},
					)
				}
			}
		}
		slog.Info("reindexing workspaces")
		for path := range s.caches {
			s.cacheDestroyLocked(path, errors.New("reindexing workspaces"))
		}
		clear(s.caches)
		runtime.GC()
		for _, folder := range allWorkspaces {
			path := protocol.DocumentURI(folder.URI).Path()
			c := NewCache(folder)
			s.cacheInitLocked(c, path)
			if changes, ok := openOverlays[folder]; ok {
				c.DidModifyFiles(ctx, changes)
			}
		}
		s.cachesMu.Unlock()
		return nil, nil
	case "protols/refreshModules":
		s.cachesMu.Lock()
		for _, c := range s.caches {
			if c.resolver.goLanguageDriver == nil {
				return nil, fmt.Errorf("go language driver not available for workspace %s", c.workspace.Name)
			}
			c.resolver.goLanguageDriver.RefreshModules()
		}
		s.cachesMu.Unlock()
		return nil, nil
	case "protols/goToGeneratedDefinition":
		var req GeneratedDefinitionParams
		if err := json.Unmarshal(params.Arguments[0], &req); err != nil {
			return nil, err
		}
		c, err := s.CacheForURI(req.TextDocument.URI)
		if err != nil {
			return nil, err
		}
		return c.FindGeneratedDefinition(ctx, req.TextDocumentPositionParams)
	default:
		var jsonData map[string]interface{}
		if err := json.Unmarshal(params.Arguments[0], &jsonData); err != nil {
			return nil, err
		}
		// check for fields "uri" or "uris" to inject a cache into the request
		uc := UnknownCommand{
			Command:   params.Command,
			Arguments: params.Arguments,
		}
		if uri, ok := jsonData["uri"]; ok {
			c, err := s.CacheForURI(protocol.DocumentURI(uri.(string)))
			if err != nil {
				return nil, err
			}
			uc.Cache = c

		} else if uris, ok := jsonData["uris"]; ok {
			c, err := s.CacheForURI(protocol.DocumentURI(uris.([]any)[0].(string)))
			if err != nil {
				return nil, err
			}
			uc.Cache = c
		} else if workspace, ok := jsonData["workspace"]; ok {
			c, err := s.CacheForWorkspace(protocol.WorkspaceFolder{
				URI:  workspace.(map[string]any)["uri"].(string),
				Name: workspace.(map[string]any)["name"].(string),
			})
			if err != nil {
				return nil, err
			}
			uc.Cache = c
		}
		if h, ok := s.unknownCommandHandlers[params.Command]; ok {
			return h.Execute(ctx, uc)
		}
		return nil, fmt.Errorf("unknown command %q", params.Command)
	}
}

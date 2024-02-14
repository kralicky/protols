package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
)

func (c *Cache) GetCodeActions(ctx context.Context, params *protocol.CodeActionParams) ([]protocol.CodeAction, error) {
	want, err := requestedActionKinds(params.Context.Only)
	if err != nil {
		return nil, err
	}

	diagnostics := params.Context.Diagnostics
	var result []protocol.CodeAction
	for _, d := range diagnostics {
		if d.Data == nil {
			continue
		}
		dataJson, err := d.Data.MarshalJSON()
		if err != nil {
			return nil, err
		}
		data := DiagnosticData{}
		if err := json.Unmarshal(dataJson, &data); err != nil {
			return nil, err
		}
		result = append(result, c.toProtocolCodeActions(data.CodeActions, &d, want)...)

		if data.Metadata != nil {
			if diagKind, ok := data.Metadata[diagnosticKind]; ok {
				switch diagKind {
				case diagnosticKindUndeclaredName:
					name := data.Metadata["name"]
					if name != "" {
						kind := protocol.QuickFix
						if !want[protocol.QuickFix] {
							if want[protocol.SourceOrganizeImports] {
								kind = protocol.SourceOrganizeImports
							} else if want[protocol.SourceFixAll] {
								kind = protocol.SourceFixAll
							} else {
								break
							}
						}
						actions := RefactorUndeclaredName(ctx, c, params.TextDocument.URI, name, kind)
						if len(actions) != 1 && kind == protocol.SourceOrganizeImports {
							// don't use the organize imports action if it's ambiguous
							break
						}
						result = append(result, actions...)
					}
				}
			}
		}
	}
	if want[protocol.RefactorExtract] || want[protocol.RefactorInline] || want[protocol.RefactorRewrite] {
		if linkRes, err := c.FindResultByURI(params.TextDocument.URI); err == nil {
			mapper, err := c.GetMapper(params.TextDocument.URI)
			if err != nil {
				return nil, err
			}
			result = append(result, FindRefactorActions(ctx, params, linkRes, mapper, want)...)
		}
	}

	if want[protocol.SourceOrganizeImports] {
		result = aggregateOrganizeImportsActions(result)
	}
	return result, nil
}

func (c *Cache) toProtocolCodeActions(rawCodeActions []CodeAction, associatedDiagnostic *protocol.Diagnostic, want map[protocol.CodeActionKind]bool) []protocol.CodeAction {
	if len(rawCodeActions) == 0 {
		return []protocol.CodeAction{}
	}
	var codeActions []protocol.CodeAction
	for _, rawCodeAction := range rawCodeActions {
		uri, err := c.resolver.PathToURI(string(rawCodeAction.Path))
		if err != nil {
			slog.With(
				"error", err,
				"path", string(rawCodeAction.Path),
			).Error("failed to resolve path to uri")
			continue
		}
		if want[rawCodeAction.Kind] {
			codeActions = append(codeActions, protocol.CodeAction{
				Title:       rawCodeAction.Title,
				Kind:        rawCodeAction.Kind,
				Diagnostics: []protocol.Diagnostic{*associatedDiagnostic},
				IsPreferred: rawCodeAction.IsPreferred,
				Edit: &protocol.WorkspaceEdit{
					Changes: map[protocol.DocumentURI][]protocol.TextEdit{
						uri: rawCodeAction.Edits,
					},
				},
				Command: rawCodeAction.Command,
			})
		}
	}
	return codeActions
}

func aggregateOrganizeImportsActions(actions []protocol.CodeAction) []protocol.CodeAction {
	// Group all the organize imports actions together into a single code action.
	// This is necessary because the client will apply the actions in sequence,
	// causing edit ranges to become invalid.
	if len(actions) == 0 {
		return actions
	}
	filtered := make([]protocol.CodeAction, 0, len(actions))
	changes := map[protocol.DocumentURI][]protocol.TextEdit{}
	seen := map[string]bool{}
	for _, action := range actions {
		if action.Kind == protocol.SourceOrganizeImports {
			for uri, edits := range action.Edit.Changes {
				for _, edit := range edits {
					if len(edit.NewText) > 0 {
						if seen[edit.NewText] {
							continue
						}
						seen[edit.NewText] = true
					}
					changes[uri] = append(changes[uri], edit)
				}
			}
		} else {
			filtered = append(filtered, action)
		}
	}
	if len(changes) > 0 {
		filtered = append(filtered, protocol.CodeAction{
			Title: "Organize Imports",
			Kind:  protocol.SourceOrganizeImports,
			Edit: &protocol.WorkspaceEdit{
				Changes: changes,
			},
		})
	}
	return filtered
}

var supportedCodeActions = map[protocol.CodeActionKind]bool{
	protocol.SourceFixAll:          true,
	protocol.SourceOrganizeImports: true,
	protocol.QuickFix:              true,
	protocol.RefactorRewrite:       true,
	protocol.RefactorInline:        true,
	protocol.RefactorExtract:       true,
}

// Logic here copied from gopls/internal/server/code_action.go
func requestedActionKinds(only []protocol.CodeActionKind) (map[protocol.CodeActionKind]bool, error) {
	// The Only field of the context specifies which code actions the client wants.
	// If Only is empty, assume that the client wants all of the non-explicit code actions.
	var want map[protocol.CodeActionKind]bool

	// Explicit Code Actions are opt-in and shouldn't be returned to the client unless
	// requested using Only.
	explicit := map[protocol.CodeActionKind]bool{
		protocol.GoTest: true,
	}

	if len(only) == 0 {
		want = supportedCodeActions
	} else {
		want = make(map[protocol.CodeActionKind]bool)
		for _, only := range only {
			for k, v := range supportedCodeActions {
				if only == k || strings.HasPrefix(string(k), string(only)+".") {
					want[k] = want[k] || v
				}
			}
			want[only] = want[only] || explicit[only]
		}
	}

	if len(want) == 0 {
		return nil, fmt.Errorf("no supported code action to execute, wanted %v", only)
	}
	return want, nil
}

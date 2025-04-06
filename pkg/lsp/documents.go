package lsp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/kralicky/protocompile/parser"
	"github.com/kralicky/tools-lite/gopls/pkg/file"
	"github.com/kralicky/tools-lite/gopls/pkg/protocol"
	"github.com/kralicky/tools-lite/pkg/diff"
	"github.com/kralicky/tools-lite/pkg/jsonrpc2"
)

func (s *Cache) ChangedText(ctx context.Context, uri protocol.VersionedTextDocumentIdentifier, changes []protocol.TextDocumentContentChangeEvent) ([]byte, error) {
	if len(changes) == 0 {
		return nil, fmt.Errorf("%w: no content changes provided", jsonrpc2.ErrInternal)
	}

	// Check if the client sent the full content of the file.
	// We accept a full content change even if the server expected incremental changes.
	if len(changes) == 1 && changes[0].Range == nil && changes[0].RangeLength == 0 {
		return []byte(changes[0].Text), nil
	}

	m, err := s.GetMapper(uri.URI)
	if err != nil {
		return nil, err
	}
	diffs, err := contentChangeEventsToDiffEdits(m, changes)
	if err != nil {
		return nil, err
	}
	return diff.ApplyBytes(m.Content, diffs)
}

func contentChangeEventsToDiffEdits(mapper *protocol.Mapper, changes []protocol.TextDocumentContentChangeEvent) ([]diff.Edit, error) {
	var edits []protocol.TextEdit
	for _, change := range changes {
		edits = append(edits, protocol.TextEdit{
			Range:   *change.Range,
			NewText: change.Text,
		})
	}

	return protocol.EditsToDiffEdits(mapper, edits)
}

func (c *Cache) DidModifyFiles(ctx context.Context, modifications []file.Modification) {
	slog.Debug("DidModifyFiles", "modifications", modifications)
	var toRecompile []string
	for _, m := range modifications {
		if m.Action == file.Delete {
			path, err := c.resolver.URIToPath(m.URI)
			if err == nil {
				toRecompile = append(toRecompile, path)
			}
		}
	}
	c.resolver.UpdateURIPathMappings(modifications)

	toDelete := []int{}
	for i, m := range modifications {
		if m.Action == file.Open && m.LanguageID != "protobuf" {
			continue
		}
		path, err := c.resolver.URIToPath(m.URI)
		if err != nil {
			slog.With(
				"error", err,
				"uri", m.URI.Path(),
			).Error("failed to resolve uri to path")
			toDelete = append(toDelete, i)
			continue
		}
		switch m.Action {
		case file.Close:
		case file.Open, file.Save:
			fh, err := c.compiler.fs.ReadFile(ctx, m.URI)
			if err == nil || fh.Version() != m.Version {
				toRecompile = append(toRecompile, path)
			}
		case file.Change, file.Create, file.Delete:
			toRecompile = append(toRecompile, path)
		}
	}
	for _, idx := range toDelete {
		modifications = slices.Delete(modifications, idx, idx+1)
	}
	if err := c.compiler.fs.UpdateOverlays(ctx, modifications); err != nil {
		panic(fmt.Errorf("internal protocol error: %w", err))
	}
	if len(toRecompile) > 0 {
		c.Compile(toRecompile,
			func() {
				c.documentVersions.Update(modifications...)
			},
			c.diagHandler.Flush,
		)
	}
}

// Checks if the most recently parsed version of the given document has any
// syntax errors, as reported by the diagnostic handler.
func (c *Cache) LatestDocumentContentsWellFormed(uri protocol.DocumentURI, strict bool) (bool, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.latestDocumentContentsWellFormedLocked(uri, strict)
}

func (c *Cache) latestDocumentContentsWellFormedLocked(uri protocol.DocumentURI, strict bool) (bool, error) {
	path, err := c.resolver.URIToPath(uri)
	if err != nil {
		return false, err
	}
	diagnostics, _, _ := c.diagHandler.GetDiagnosticsForPath(path)
	for _, diag := range diagnostics {
		var parseErr parser.ParseError
		if errors.As(diag.Error, &parseErr) {
			return false, nil
		}
		if strict {
			var ext parser.ExtendedSyntaxError
			if errors.As(diag.Error, &ext) && !ext.CanFormat() {
				return false, nil
			}
		}
	}
	return true, nil
}

func (c *Cache) WaitDocumentVersion(ctx context.Context, uri protocol.DocumentURI, version int32) error {
	ctx, ca := context.WithTimeout(ctx, 2*time.Second)
	defer ca()
	return c.documentVersions.Wait(ctx, uri, version)
}

func (r *Cache) GetMapper(uri protocol.DocumentURI) (*protocol.Mapper, error) {
	if !uri.IsFile() {
		data, err := r.resolver.SyntheticFileContents(uri)
		if err != nil {
			return nil, err
		}
		return protocol.NewMapper(uri, []byte(data)), nil
	}
	fh, err := r.resolver.ReadFile(context.TODO(), uri)
	if err != nil {
		return nil, err
	}
	content, err := fh.Content()
	if err != nil {
		return nil, err
	}
	return protocol.NewMapper(uri, content), nil
}

func (c *Cache) GetSyntheticFileContents(ctx context.Context, uri protocol.DocumentURI) (string, error) {
	return c.resolver.SyntheticFileContents(uri)
}

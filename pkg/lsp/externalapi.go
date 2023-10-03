package lsp

import (
	"github.com/bufbuild/protocompile/linker"
	"golang.org/x/exp/maps"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
	"golang.org/x/tools/gopls/pkg/span"
)

// Intended for use with external tools only, not part of LSP implementation.
func (c *Cache) XGetAllDiagnostics() (map[span.URI][]protocol.Diagnostic, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	diagnostics := c.diagHandler.FullDiagnosticSnapshot()
	res := make(map[span.URI][]protocol.Diagnostic, len(diagnostics))
	for path, diags := range diagnostics {
		uri, err := c.resolver.PathToURI(path)
		if err != nil {
			return nil, err
		}
		res[uri] = c.toProtocolDiagnostics(diags)
	}
	return res, nil
}

// Intended for use with external tools only, not part of LSP implementation.
func (c *Cache) XGetLinkerResults() []linker.Result {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	results := make([]linker.Result, len(c.results))
	for i, result := range c.results {
		results[i] = result.(linker.Result)
	}
	return results
}

func (c *Cache) XGetMapper(uri span.URI) (*protocol.Mapper, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.GetMapper(uri)
}

func (c *Cache) XGetURIPathMappings() (map[string]span.URI, map[span.URI]string) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return maps.Clone(c.resolver.fileURIsByPath), maps.Clone(c.resolver.filePathsByURI)
}

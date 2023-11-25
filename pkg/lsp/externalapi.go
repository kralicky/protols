package lsp

import (
	"maps"

	"github.com/bufbuild/protocompile/linker"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
)

// Intended for use with external tools only, not part of LSP implementation.
func (c *Cache) XGetAllDiagnostics() (map[protocol.DocumentURI][]protocol.Diagnostic, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	diagnostics := c.diagHandler.FullDiagnosticSnapshot()
	res := make(map[protocol.DocumentURI][]protocol.Diagnostic, len(diagnostics))
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

func (c *Cache) XGetResolver() linker.Resolver {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.results.AsResolver()
}

func (c *Cache) XGetMapper(uri protocol.DocumentURI) (*protocol.Mapper, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.GetMapper(uri)
}

type PathMappings struct {
	FileURIsByPath                  map[string]protocol.DocumentURI
	FilePathsByURI                  map[protocol.DocumentURI]string
	SyntheticFileOriginalNamesByURI map[protocol.DocumentURI]string
}

func (c *Cache) XGetURIPathMappings() PathMappings {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return PathMappings{
		FileURIsByPath:                  maps.Clone(c.resolver.fileURIsByPath),
		FilePathsByURI:                  maps.Clone(c.resolver.filePathsByURI),
		SyntheticFileOriginalNamesByURI: maps.Clone(c.resolver.syntheticFileOriginalNames),
	}
}

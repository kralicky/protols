package lsp

import (
	"maps"
	"slices"

	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/tools-lite/gopls/pkg/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
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
	results := make([]linker.Result, 0, len(c.results))
	for _, result := range c.results {
		if result.IsPlaceholder() {
			continue
		}
		if result.Syntax() == protoreflect.Editions {
			continue
		}
		results = append(results, result.(linker.Result))
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

func (c *Cache) XListWorkspaceLocalURIs() []protocol.DocumentURI {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	uris := make([]protocol.DocumentURI, 0, len(c.resolver.filePathsByURI))
	for uri := range c.resolver.filePathsByURI {
		if c.resolver.IsRealWorkspaceLocalFile(uri) {
			uris = append(uris, uri)
		}
	}
	return uris
}

func (c *Cache) XGetAllMessages() []protoreflect.MessageDescriptor {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	var all []protoreflect.MessageDescriptor
	for _, res := range c.results {
		msgs := res.Messages()
		all = slices.Grow(all, msgs.Len())
		for i, l := 0, msgs.Len(); i < l; i++ {
			all = append(all, msgs.Get(i))
		}
	}
	return all
}

package lsp

import (
	"fmt"

	"github.com/kralicky/protocompile"
	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
)

func (c *Cache) ComputeDocumentLinks(doc protocol.TextDocumentIdentifier) ([]protocol.DocumentLink, error) {
	// link valid imports
	var links []protocol.DocumentLink
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()

	res, err := c.FindParseResultByURI(doc.URI)
	if err != nil {
		return nil, err
	}
	resAst := res.AST()
	if resAst == nil {
		return nil, fmt.Errorf("no AST available for %s", doc.URI)
	}
	var imports []*ast.ImportNode
	// get the source positions of the import statements
	for _, decl := range resAst.Decls {
		if imp := decl.GetImport(); imp != nil {
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
		path := dependencyPaths[i]
		nameInfo := resAst.NodeInfo(imp.Name)
		if sr, err := c.resolver.FindFileByPath(protocompile.UnresolvedPath(path), res); err == nil {
			targetUri, err := c.resolver.PathToURI(string(sr.ResolvedPath))
			if err == nil {
				links = append(links, protocol.DocumentLink{
					Range:  toRange(nameInfo),
					Target: (*string)(&targetUri),
				})
			}
		}
	}

	return links, nil
}

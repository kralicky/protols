package lsp

import (
	"encoding/json"

	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/tools-lite/gopls/pkg/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (c *Cache) ComputeCodeLens(uri protocol.DocumentURI) ([]protocol.CodeLens, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()

	if !c.resolver.IsRealWorkspaceLocalFile(uri) {
		return nil, nil
	}

	parseRes, err := c.FindParseResultByURI(uri)
	if err != nil {
		return nil, err
	}
	if _, ok := parseRes.AST().Pragma(PragmaNoGenerate); ok {
		return nil, nil
	}

	var codeLenses []protocol.CodeLens
	req, _ := json.Marshal(GenerateCodeRequest{
		URIs: []protocol.DocumentURI{uri},
	})

	codeLenses = append(codeLenses,
		protocol.CodeLens{
			Command: &protocol.Command{
				Title:     "Generate File",
				Command:   "protols.generate",
				Arguments: []json.RawMessage{json.RawMessage(req)},
			},
		},
	)

	if pkg := protoreflect.FullName(parseRes.FileDescriptorProto().GetPackage()); pkg != "" {
		var uris []protocol.DocumentURI
		c.results.RangeFilesByPackage(pkg, func(f linker.File) bool {
			uri, err := c.resolver.PathToURI(f.Path())
			if err != nil {
				return true
			}
			uris = append(uris, uri)
			return true
		})
		req, _ := json.Marshal(GenerateCodeRequest{
			URIs: uris,
		})
		codeLenses = append(codeLenses,
			protocol.CodeLens{
				Command: &protocol.Command{
					Title:     "Generate Package",
					Command:   "protols.generate",
					Arguments: []json.RawMessage{json.RawMessage(req)},
				},
			},
		)
	}

	req, _ = json.Marshal(GenerateWorkspaceRequest{
		Workspace: c.workspace,
	})
	codeLenses = append(codeLenses,
		protocol.CodeLens{
			Command: &protocol.Command{
				Title:     "Generate Workspace",
				Command:   "protols.generateWorkspace",
				Arguments: []json.RawMessage{json.RawMessage(req)},
			},
		},
	)

	return codeLenses, nil
}

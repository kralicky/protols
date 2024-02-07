package sdkutil

import (
	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protols/pkg/lsp"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TopologicalSort(results []linker.Result) []linker.Result {
	sorted := make([]linker.Result, 0, len(results))
	index := make(map[string]linker.Result, len(results))
	for _, res := range results {
		index[res.Path()] = res
	}
	topologicalSort(results, &sorted, index, make(map[string]struct{}))
	return sorted
}

func topologicalSort(results []linker.Result, sorted *[]linker.Result, index map[string]linker.Result, seen map[string]struct{}) {
	for _, res := range results {
		if _, ok := seen[res.Path()]; ok {
			continue
		}
		seen[res.Path()] = struct{}{}
		deps := []linker.Result{}
		imports := res.Imports()
		for i := 0; i < imports.Len(); i++ {
			im := imports.Get(i)

			deps = append(deps, index[im.Path()])
		}
		topologicalSort(deps, sorted, index, seen)
		*sorted = append(*sorted, res)
	}
}

func RestoreDescriptorNames(protos []*descriptorpb.FileDescriptorProto, pathMapping lsp.PathMappings) {
	for _, fpb := range protos {
		uri, ok := pathMapping.FileURIsByPath[fpb.GetName()]
		if !ok {
			continue
		}
		if orig, hasOriginalName := pathMapping.SyntheticFileOriginalNamesByURI[uri]; hasOriginalName {
			if orig == fpb.GetName() {
				continue
			}
			*fpb.Name = orig
		}
		for i, dep := range fpb.Dependency {
			uri, ok := pathMapping.FileURIsByPath[dep]
			if !ok {
				continue
			}
			if orig, hasOriginalName := pathMapping.SyntheticFileOriginalNamesByURI[uri]; hasOriginalName {
				if orig == dep {
					continue
				}
				fpb.Dependency[i] = orig
			}
		}
	}
}

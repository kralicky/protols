package codegen

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bufbuild/protocompile/linker"
	"github.com/kralicky/protols/pkg/lsp"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

type RenameStrategy int

const (
	KeepLSPResolvedNames RenameStrategy = iota

	// If set, this will restore the original names of the files that were
	// synthesized from external (previously generated) Go module sources.
	// This can be necessary if those dependencies are intended to be linked
	// into a binary alongside newly generated code containing references to
	// the LSP-resolved names for those files. Because the linked dependencies
	// will register their own file descriptors to the global file descriptor
	// cache using the original names at runtime, any workspace-local descriptor
	// protos must also refer to those dependencies with the expected runtime
	// names during code generation.
	// This only modifies the contents of AllDescriptorProtos and
	// WorkspaceLocalDescriptorProtos.
	RestoreExternalGoModuleDescriptorNames
)

type DriverOptions struct {
	renameStrategy RenameStrategy
}

type DriverOption func(*DriverOptions)

func (o *DriverOptions) apply(opts ...DriverOption) {
	for _, op := range opts {
		op(o)
	}
}

func WithRenameStrategy(renameStrategy RenameStrategy) DriverOption {
	return func(o *DriverOptions) {
		o.renameStrategy = renameStrategy
	}
}

type Driver struct {
	DriverOptions
	workspace protocol.WorkspaceFolder
}

func NewDriver(workspaceFolder string, opts ...DriverOption) *Driver {
	options := DriverOptions{}
	options.apply(opts...)

	return &Driver{
		DriverOptions: options,
		workspace: protocol.WorkspaceFolder{
			URI: string(protocol.URIFromPath(workspaceFolder)),
		},
	}
}

type Results struct {
	Error                          bool
	Messages                       []string
	AllDescriptors                 []protoreflect.FileDescriptor
	AllDescriptorProtos            []*descriptorpb.FileDescriptorProto
	WorkspaceLocalDescriptors      []protoreflect.FileDescriptor
	WorkspaceLocalDescriptorProtos []*descriptorpb.FileDescriptorProto
	FileURIsByPath                 map[string]protocol.DocumentURI
	FilePathsByURI                 map[protocol.DocumentURI]string
}

var severityToColor = map[protocol.DiagnosticSeverity]string{
	protocol.SeverityError:   "\x1b[31m",
	protocol.SeverityWarning: "\x1b[33m",
}
var severityMsg = map[protocol.DiagnosticSeverity]string{
	protocol.SeverityError:   "\x1b[31merror\x1b[0m",
	protocol.SeverityWarning: "\x1b[33mwarning\x1b[0m",
}

func (d *Driver) Compile(protos []string) (*Results, error) {
	cache := lsp.NewCache(d.workspace)
	cache.LoadFiles(protos)

	diagnostics, err := cache.XGetAllDiagnostics()
	if err != nil {
		return nil, err
	}
	var results Results
	for uri, diags := range diagnostics {
		mapper, err := cache.XGetMapper(uri)
		if err != nil {
			return nil, err
		}
		for _, diag := range diags {
			if diag.Severity == protocol.SeverityError {
				results.Error = true
			}
			// obtain the whole line as context
			if diag.Range.Start.Line == diag.Range.End.Line {
				start := protocol.Position{
					Line:      diag.Range.Start.Line,
					Character: 0,
				}
				end := protocol.Position{
					Line:      diag.Range.End.Line + 1,
					Character: 0,
				}
				startPos, endPos, err := mapper.RangeOffsets(protocol.Range{
					Start: start,
					End:   end,
				})
				if err != nil {
					return nil, err
				}
				showSourceContext := true
				for _, tag := range diag.Tags {
					if tag == protocol.Unnecessary {
						showSourceContext = false
						break
					}
				}
				// show the source line with dimmed text before and after the error range,
				// and highlight the error range
				var uriFilename string
				if uri.IsFile() {
					uriFilename = uri.Path()
				} else {
					uriFilename = string(uri)
				}
				relativePath, _ := filepath.Rel(protocol.URIFromURI(d.workspace.URI).Path(), uriFilename)
				color := severityToColor[diag.Severity]
				if !showSourceContext {
					results.Messages = append(results.Messages,
						fmt.Sprintf("%s: %s:%d: %s",
							severityMsg[diag.Severity],
							relativePath, diag.Range.Start.Line+1,
							diag.Message,
						),
					)
					continue
				}

				fullLine := string(mapper.Content[startPos : endPos-1])
				if diag.Range.Start.Character > uint32(len(fullLine)) ||
					diag.Range.End.Character > uint32(len(fullLine)) ||
					diag.Range.Start.Character > diag.Range.End.Character {
					diag.Range.Start.Character = 0
					diag.Range.End.Character = uint32(len(fullLine))
				}
				highlightedLine := fmt.Sprintf("%s%s%s",
					"\x1b[2m"+fullLine[:diag.Range.Start.Character]+"\x1b[0m",
					color+fullLine[diag.Range.Start.Character:diag.Range.End.Character]+"\x1b[0m",
					"\x1b[2m"+fullLine[diag.Range.End.Character:]+"\x1b[0m",
				)

				results.Messages = append(results.Messages,
					fmt.Sprintf("%s: %s:%d: %s\n\t%s", severityMsg[diag.Severity],
						relativePath, diag.Range.Start.Line+1,
						diag.Message,
						highlightedLine,
					),
				)
			}
		}
	}
	if !results.Error {
		unsorted := cache.XGetLinkerResults()
		pathMappings := cache.XGetURIPathMappings()
		results.FileURIsByPath = pathMappings.FileURIsByPath
		results.FilePathsByURI = pathMappings.FilePathsByURI
		results.AllDescriptors, results.WorkspaceLocalDescriptors = d.sortAndFilterResults(unsorted, results.FileURIsByPath)
		results.AllDescriptorProtos = make([]*descriptorpb.FileDescriptorProto, len(results.AllDescriptors))
		results.WorkspaceLocalDescriptorProtos = make([]*descriptorpb.FileDescriptorProto, len(results.WorkspaceLocalDescriptors))

		for i, desc := range results.AllDescriptors {
			results.AllDescriptorProtos[i] = desc.(linker.Result).FileDescriptorProto()
		}
		for i, desc := range results.WorkspaceLocalDescriptors {
			results.WorkspaceLocalDescriptorProtos[i] = desc.(linker.Result).FileDescriptorProto()
		}
		switch d.renameStrategy {
		case RestoreExternalGoModuleDescriptorNames:
			restoreDescriptorNames(&results, pathMappings)
		}
	}
	return &results, nil
}

// Given a list of linker results, sorts them topologically and returns two lists:
//  1. The sorted list of all descriptors
//  2. A subset of the first list, containing only files that exist on disk
//     and are local to the workspace.
func (d *Driver) sortAndFilterResults(results []linker.Result, pathMapping map[string]protocol.DocumentURI) ([]protoreflect.FileDescriptor, []protoreflect.FileDescriptor) {
	sorted := make([]protoreflect.FileDescriptor, 0, len(results))
	localToWorkspace := []protoreflect.FileDescriptor{}
	topologicalSort(results, &sorted, make(map[string]struct{}))
	for _, res := range sorted {
		uri := pathMapping[res.Path()]
		if !uri.IsFile() {
			continue
		}
		path, err := filepath.Rel(protocol.URIFromURI(d.workspace.URI).Path(), uri.Path())
		if err != nil {
			continue
		}
		if _, err := os.Stat(path); err == nil && filepath.IsLocal(path) {
			localToWorkspace = append(localToWorkspace, res)
		}
	}
	return sorted, localToWorkspace
}

func topologicalSort[F linker.File, S ~[]F](results S, sorted *[]protoreflect.FileDescriptor, seen map[string]struct{}) {
	for _, res := range results {
		if _, ok := seen[res.Path()]; ok {
			continue
		}
		seen[res.Path()] = struct{}{}
		deps := []linker.File{}
		imports := res.Imports()
		for i := 0; i < imports.Len(); i++ {
			im := imports.Get(i)

			deps = append(deps, res.FindImportByPath(im.Path()))
		}
		topologicalSort(deps, sorted, seen)
		*sorted = append(*sorted, res)
	}
}

func restoreDescriptorNames(results *Results, pathMapping lsp.PathMappings) {
	for _, fpb := range results.AllDescriptorProtos {
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

	for _, fpb := range results.WorkspaceLocalDescriptorProtos {
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

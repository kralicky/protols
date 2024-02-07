package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protols/pkg/lsp"
	sdkutil "github.com/kralicky/protols/sdk/util"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
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
	protocol.SeverityHint:        "\x1b[34m", // blue
	protocol.SeverityInformation: "\x1b[32m", // green
	protocol.SeverityWarning:     "\x1b[33m", // yellow
	protocol.SeverityError:       "\x1b[31m", // red
}

var severityMsg = map[protocol.DiagnosticSeverity]string{
	protocol.SeverityHint:        "\x1b[34mhint\x1b[0m",
	protocol.SeverityInformation: "\x1b[32minfo\x1b[0m",
	protocol.SeverityWarning:     "\x1b[33mwarning\x1b[0m",
	protocol.SeverityError:       "\x1b[31merror\x1b[0m",
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
				showSourceContext := diag.Severity <= protocol.SeverityWarning
				for _, tag := range diag.Tags {
					if tag == protocol.Unnecessary {
						showSourceContext = false
						break
					}
				}
				// show the source line with dimmed text before and after the error range,
				// and highlight the error range
				var uriFilename, relativePath string
				if uri.IsFile() {
					uriFilename = uri.Path()
				} else {
					uriFilename = strings.TrimPrefix(string(uri), "proto://")
				}
				if p, err := filepath.Rel(protocol.DocumentURI(d.workspace.URI).Path(), uriFilename); err == nil {
					relativePath = p
				} else {
					relativePath = uriFilename
				}
				// dim path and line number
				source := fmt.Sprintf("\x1b[2m%s:%d\x1b[0m", relativePath, diag.Range.Start.Line+1)

				color := severityToColor[diag.Severity]
				if !showSourceContext {
					results.Messages = append(results.Messages,
						fmt.Sprintf("%s: %s %s",
							severityMsg[diag.Severity],
							source,
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
					fmt.Sprintf("%s: %s %s\n\t%s", severityMsg[diag.Severity],
						source,
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
			sdkutil.RestoreDescriptorNames(results.AllDescriptorProtos, pathMappings)
			sdkutil.RestoreDescriptorNames(results.WorkspaceLocalDescriptorProtos, pathMappings)
		}
	}
	return &results, nil
}

// Given a list of linker results, sorts them topologically and returns two lists:
//  1. The sorted list of all descriptors
//  2. A subset of the first list, containing only files that exist on disk
//     and are local to the workspace.
func (d *Driver) sortAndFilterResults(results []linker.Result, pathMapping map[string]protocol.DocumentURI) ([]protoreflect.FileDescriptor, []protoreflect.FileDescriptor) {
	sorted := sdkutil.TopologicalSort(results)
	sortedFds := make([]protoreflect.FileDescriptor, len(sorted))
	for i, res := range sorted {
		sortedFds[i] = res
	}
	localToWorkspace := []protoreflect.FileDescriptor{}
	for _, fd := range sortedFds {
		uri := pathMapping[fd.Path()]
		if !uri.IsFile() {
			continue
		}
		path, err := filepath.Rel(protocol.DocumentURI(d.workspace.URI).Path(), uri.Path())
		if err != nil {
			continue
		}
		if _, err := os.Stat(path); err == nil && filepath.IsLocal(path) {
			localToWorkspace = append(localToWorkspace, fd)
		}
	}
	return sortedFds, localToWorkspace
}

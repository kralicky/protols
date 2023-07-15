package lsp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bmatcuk/doublestar"
	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/protoutil"
	"github.com/bufbuild/protocompile/reporter"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/desc/protoprint"
	gsync "github.com/kralicky/gpkg/sync"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"golang.org/x/tools/gopls/pkg/lsp/cache"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
	"golang.org/x/tools/gopls/pkg/lsp/source"
	"golang.org/x/tools/gopls/pkg/span"
	"golang.org/x/tools/pkg/diff"
	"golang.org/x/tools/pkg/jsonrpc2"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Cache is responsible for keeping track of all the known proto source files
// and definitions.
type Cache struct {
	workspace      protocol.WorkspaceFolder
	lg             *zap.Logger
	compiler       *Compiler
	resolver       *Resolver
	diagHandler    *DiagnosticHandler
	resultsMu      sync.RWMutex
	results        linker.Files
	partialResults map[string]parser.Result
	indexMu        sync.RWMutex
	// indexedDirsByGoPkg map[string]string   // go package name -> directory
	// indexedGoPkgsByDir map[string]string   // directory -> go package name
	// filePathsByURI map[span.URI]string // URI -> canonical file path (go package + file name)
	// fileURIsByPath map[string]span.URI // canonical file path (go package + file name) -> URI

	todoModLock sync.Mutex

	compileLock sync.Mutex

	inflightTasksInvalidate gsync.Map[string, time.Time]
	inflightTasksCompile    gsync.Map[string, time.Time]
}

// FindDescriptorByName implements linker.Resolver.
func (c *Cache) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.results.AsResolver().FindDescriptorByName(name)
}

// FindExtensionByName implements linker.Resolver.
func (c *Cache) FindExtensionByName(field protoreflect.FullName) (protoreflect.ExtensionType, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.results.AsResolver().FindExtensionByName(field)
}

// FindExtensionByNumber implements linker.Resolver.
func (c *Cache) FindExtensionByNumber(message protoreflect.FullName, field protowire.Number) (protoreflect.ExtensionType, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.results.AsResolver().FindExtensionByNumber(message, field)
}

func (c *Cache) FindResultByPath(path string) (linker.Result, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	if c.results == nil {
		return nil, fmt.Errorf("no results exist")
	}
	f := c.results.FindFileByPath(path)
	if f == nil {
		return nil, fmt.Errorf("FindResultByPath: package not found: %q", path)
	}
	return f.(linker.Result), nil
}

func (c *Cache) FindResultByURI(uri span.URI) (linker.Result, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	if c.results == nil {
		return nil, fmt.Errorf("no results exist")
	}
	path, err := c.resolver.URIToPath(uri)
	if err != nil {
		return nil, err
	}
	f := c.results.FindFileByPath(path)
	if f == nil {
		return nil, fmt.Errorf("FindResultByURI: package not found: %q", path)
	}
	return f.(linker.Result), nil
}

func (c *Cache) FindParseResultByURI(uri span.URI) (parser.Result, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	if c.results == nil && len(c.partialResults) == 0 {
		return nil, fmt.Errorf("no results or partial results exist")
	}
	path, err := c.resolver.URIToPath(uri)
	if err != nil {
		return nil, err
	}
	if pr, ok := c.partialResults[path]; ok {
		return pr, nil
	}
	return c.FindResultByURI(uri)
}

// FindFileByPath implements linker.Resolver.
func (c *Cache) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.results.AsResolver().FindFileByPath(path)
}

func (c *Cache) FindFileByURI(uri span.URI) (protoreflect.FileDescriptor, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	path, err := c.resolver.URIToPath(uri)
	if err != nil {
		return nil, err
	}
	return c.results.AsResolver().FindFileByPath(path)
}

// FindMessageByName implements linker.Resolver.
func (c *Cache) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.results.AsResolver().FindMessageByName(name)
}

// FindMessageByURL implements linker.Resolver.
func (c *Cache) FindMessageByURL(url string) (protoreflect.MessageType, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.results.AsResolver().FindMessageByURL(url)
}

var _ linker.Resolver = (*Cache)(nil)

type Compiler struct {
	*protocompile.Compiler
	fs      *cache.OverlayFS
	workdir string
}

var requiredGoEnvVars = []string{"GO111MODULE", "GOFLAGS", "GOINSECURE", "GOMOD", "GOMODCACHE", "GONOPROXY", "GONOSUMDB", "GOPATH", "GOPROXY", "GOROOT", "GOSUMDB", "GOWORK"}

func NewCache(workspace protocol.WorkspaceFolder, lg *zap.Logger) *Cache {
	workdir := span.URIFromURI(workspace.URI).Filename()
	// NewCache creates a new cache.
	diagHandler := NewDiagnosticHandler()
	reporter := reporter.NewReporter(diagHandler.HandleError, diagHandler.HandleWarning)
	resolver := NewResolver(workspace, lg)

	compiler := &Compiler{
		fs: resolver.OverlayFS,
		Compiler: &protocompile.Compiler{
			Resolver:       resolver,
			MaxParallelism: runtime.NumCPU() * 4,
			Reporter:       reporter,
			SourceInfoMode: protocompile.SourceInfoExtraComments | protocompile.SourceInfoExtraOptionLocations,
			RetainResults:  true,
			RetainASTs:     true,
		},
		workdir: workdir,
	}
	cache := &Cache{
		workspace:   workspace,
		lg:          lg,
		compiler:    compiler,
		resolver:    resolver,
		diagHandler: diagHandler,
		// indexedDirsByGoPkg: map[string]string{},
		// indexedGoPkgsByDir: map[string]string{},
		// filePathsByURI: map[span.URI]string{},
		// fileURIsByPath: map[string]span.URI{},
		partialResults: map[string]parser.Result{},
	}
	compiler.Hooks = protocompile.CompilerHooks{
		PreInvalidate:  cache.preInvalidateHook,
		PostInvalidate: cache.postInvalidateHook,
		PreCompile:     cache.preCompile,
		PostCompile:    cache.postCompile,
	}
	cache.Reindex()
	return cache
}

func (r *Cache) GetMapper(uri span.URI) (*protocol.Mapper, error) {
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

func (s *Cache) ChangedText(ctx context.Context, uri span.URI, changes []protocol.TextDocumentContentChangeEvent) ([]byte, error) {
	if len(changes) == 0 {
		return nil, fmt.Errorf("%w: no content changes provided", jsonrpc2.ErrInternal)
	}

	// Check if the client sent the full content of the file.
	// We accept a full content change even if the server expected incremental changes.
	if len(changes) == 1 && changes[0].Range == nil && changes[0].RangeLength == 0 {
		return []byte(changes[0].Text), nil
	}

	m, err := s.GetMapper(uri)
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

	return source.FromProtocolEdits(mapper, edits)
}

func (c *Cache) preInvalidateHook(path string, reason string) {
	fmt.Printf("invalidating %s (%s)\n", path, reason)
	c.inflightTasksInvalidate.Store(path, time.Now())
	c.diagHandler.ClearDiagnosticsForPath(path)
}

func (c *Cache) postInvalidateHook(path string) {
	startTime, ok := c.inflightTasksInvalidate.LoadAndDelete(path)
	if ok {
		fmt.Printf("invalidated %s (took %s)\n", path, time.Since(startTime))
	} else {
		fmt.Printf("invalidated %s\n", path)
	}
}

func (c *Cache) preCompile(path string) {
	fmt.Printf("compiling %s\n", path)
	c.inflightTasksCompile.Store(path, time.Now())
	c.resultsMu.Lock()
	delete(c.partialResults, path)
	c.resultsMu.Unlock()
}

func (c *Cache) postCompile(path string) {
	startTime, ok := c.inflightTasksCompile.LoadAndDelete(path)
	if ok {
		fmt.Printf("compiled %s (took %s)\n", path, time.Since(startTime))
	} else {
		fmt.Printf("compiled %s\n", path)
	}
}

func (c *Cache) Reindex() {
	c.lg.Debug("reindexing")

	c.resolver.ResetPathMappings()

	allProtos, _ := doublestar.Glob(path.Join(c.compiler.workdir, "**/*.proto"))

	if len(allProtos) == 0 {
		c.lg.Debug("no protos found")
		return
	}
	c.lg.Debug("found protos", zap.Strings("protos", allProtos))
	created := make([]source.FileModification, len(allProtos))
	for i, proto := range allProtos {
		created[i] = source.FileModification{
			Action: source.Create,
			OnDisk: true,
			URI:    span.URIFromPath(proto),
		}
	}

	if err := c.DidModifyFiles(context.TODO(), created); err != nil {
		c.lg.Error("failed to index files", zap.Error(err))
	}
	c.resultsMu.Lock()
	defer c.resultsMu.Unlock()
	implicit := c.compiler.GetImplicitResults()
	c.lg.Debug("indexing implicit results", zap.Int("results", len(implicit)))
	c.results = append(c.results, implicit...)
}

func (c *Cache) Compile(protos ...string) {
	c.compileLock.Lock()
	defer c.compileLock.Unlock()
	c.lg.Info("compiling", zap.Int("protos", len(protos)))
	res, err := c.compiler.Compile(context.TODO(), protos...)
	if err != nil {
		if !errors.Is(err, reporter.ErrInvalidSource) {
			c.lg.With(zap.Error(err)).Error("failed to compile")
			return
		}
	}
	// important to lock resultsMu here so that it can be modified in compile hooks
	c.resultsMu.Lock()
	defer c.resultsMu.Unlock()
	c.lg.Info("done compiling", zap.Int("protos", len(protos)))
	for _, r := range res.Files {
		path := r.Path()
		found := false
		// delete(c.partialResults, path)
		for i, f := range c.results {
			// todo: this is big slow
			if f.Path() == path {
				found = true
				c.lg.With(zap.String("path", path)).Debug("updating existing linker result")
				c.results[i] = r
				break
			}
		}
		if !found {
			c.lg.With(zap.String("path", path)).Debug("adding new linker result")
			c.results = append(c.results, r)
		}
	}
	for path, partial := range res.UnlinkedParserResults {
		partial := partial
		c.lg.With(zap.String("path", path)).Debug("adding new partial linker result")
		c.partialResults[path] = partial
	}
}

// func (s *Cache) OnFileOpened(doc protocol.TextDocumentItem) {
// 	s.lg.With(
// 		zap.String("file", string(doc.URI)),
// 		zap.String("path", s.filePathsByURI[doc.URI.SpanURI()]),
// 	).Debug("file opened")
// 	s.compiler.overlay.Create(doc.URI.SpanURI(), s.filePathsByURI[doc.URI.SpanURI()], []byte(doc.Text))
// }

// func (s *Cache) OnFileClosed(doc protocol.TextDocumentIdentifier) {
// 	s.lg.With(
// 		zap.String("file", string(doc.URI)),
// 	).Debug("file closed")
// 	s.compiler.overlay.Delete(s.filePathsByURI[doc.URI.SpanURI()])
// }

// func (s *Cache) OnFileModified(f protocol.VersionedTextDocumentIdentifier, contentChanges []protocol.TextDocumentContentChangeEvent) error {
// 	s.todoModLock.Lock()
// 	defer s.todoModLock.Unlock()
// 	s.lg.With(
// 		zap.String("file", string(f.URI)),
// 	).Debug("file modified")

// 	if err := s.compiler.overlay.Update(f.URI.SpanURI(), contentChanges); err != nil {
// 		return err
// 	}
// 	s.Compile(s.filePathsByURI[f.URI.SpanURI()])
// 	return nil
// }

// func (c *Cache) OnFilesDeleted(f []protocol.FileDelete) error {
// 	c.indexMu.Lock()
// 	defer c.indexMu.Unlock()
// 	// remove from cache
// 	paths := make([]string, len(f))
// 	for i, file := range f {
// 		paths[i] = c.filePathsByURI[span.URIFromURI(file.URI)]
// 		c.compiler.overlay.Delete(paths[i])
// 	}
// 	c.lg.With(
// 		zap.Strings("files", paths),
// 	).Debug("files deleted")
// 	c.Compile(paths...)

// 	for _, path := range paths {
// 		uri := c.fileURIsByPath[path]
// 		delete(c.filePathsByURI, uri)
// 		delete(c.fileURIsByPath, path)
// 	}
// 	return nil
// }

// func (c *Cache) OnFilesCreated(files []protocol.FileCreate) error {
// 	c.indexMu.Lock()
// 	resolved := make([]string, 0, len(files))
// 	for _, f := range files {
// 		uri := span.URIFromURI(f.URI)
// 		filename := uri.Filename()
// 		goPkg, err := FastLookupGoModule(filename)
// 		if err != nil {
// 			c.lg.With(
// 				zap.String("filename", filename),
// 				zap.Error(err),
// 			).Debug("failed to lookup go module")
// 			continue
// 		}
// 		canonicalName := filepath.Join(goPkg, filepath.Base(filename))
// 		c.filePathsByURI[uri] = canonicalName
// 		c.fileURIsByPath[canonicalName] = uri
// 		resolved = append(resolved, canonicalName)
// 	}
// 	c.indexMu.Unlock()
// 	c.lg.With(
// 		zap.Int("files", len(resolved)),
// 	).Debug("files created")
// 	c.Compile(resolved...)

// 	return nil
// }

// func (c *Cache) OnFilesRenamed(f []protocol.FileRename) error {
// 	c.lg.With(
// 		zap.Any("files", f),
// 	).Debug("files renamed")

// 	c.indexMu.Lock()
// 	defer c.indexMu.Unlock()

// 	paths := make([]string, len(f))
// 	for _, file := range f {
// 		oldURI := span.URIFromURI(file.OldURI)
// 		newURI := span.URIFromURI(file.NewURI)
// 		path := c.filePathsByURI[oldURI]
// 		delete(c.filePathsByURI, oldURI)
// 		c.filePathsByURI[newURI] = path
// 		c.fileURIsByPath[path] = newURI
// 		paths = append(paths, path)
// 	}

// 	c.Compile(paths...)
// 	return nil
// }

func (c *Cache) DidModifyFiles(ctx context.Context, modifications []source.FileModification) error {
	c.resolver.UpdateURIPathMappings(modifications)

	var toRecompile []string
	for _, m := range modifications {
		path, err := c.resolver.URIToPath(m.URI)
		if err != nil {
			c.lg.With(
				zap.Error(err),
				zap.String("uri", m.URI.Filename()),
			).Error("failed to resolve uri to path")
			continue
		}
		switch m.Action {
		case source.Open:
		case source.Close:
		case source.Save:
			toRecompile = append(toRecompile, path)
		case source.Change:
			toRecompile = append(toRecompile, path)
		case source.Create:
			toRecompile = append(toRecompile, path)
		case source.Delete:
			toRecompile = append(toRecompile, path)
		}
	}
	if err := c.compiler.fs.UpdateOverlays(ctx, modifications); err != nil {
		return err
	}
	c.Compile(toRecompile...)
	return nil
}

// func (s *Cache) OnFileSaved(f *protocol.DidSaveTextDocumentParams) error {
// 	s.lg.With(
// 		zap.String("file", string(f.TextDocument.URI)),
// 	).Debug("file modified")
// 	s.compiler.overlay.ReloadFromDisk(f.TextDocument.URI.SpanURI())
// 	s.Compile(s.filePathsByURI[f.TextDocument.URI.SpanURI()])
// 	return nil
// }

func (c *Cache) ComputeSemanticTokens(doc protocol.TextDocumentIdentifier) ([]uint32, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	result, err := semanticTokensFull(c, doc)
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (c *Cache) ComputeSemanticTokensRange(doc protocol.TextDocumentIdentifier, rng protocol.Range) ([]uint32, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	result, err := semanticTokensRange(c, doc, rng)
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (c *Cache) ComputeDiagnosticReports(uri span.URI, prevResultId string) ([]protocol.Diagnostic, protocol.DocumentDiagnosticReportKind, string, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	var maybePrevResultId []string
	if prevResultId != "" {
		maybePrevResultId = append(maybePrevResultId, prevResultId)
	}
	path, err := c.resolver.URIToPath(uri)
	if err != nil {
		c.lg.With(
			zap.Error(err),
			zap.String("uri", string(uri)),
		).Error("failed to resolve uri to path")
		return nil, protocol.DiagnosticUnchanged, "", nil
	}
	rawReports, resultId, unchanged := c.diagHandler.GetDiagnosticsForPath(path, maybePrevResultId...)
	if unchanged {
		return nil, protocol.DiagnosticUnchanged, resultId, nil
	}
	protocolReports := c.toProtocolDiagnostics(rawReports)
	if protocolReports == nil {
		protocolReports = []protocol.Diagnostic{}
	}

	return protocolReports, protocol.DiagnosticFull, resultId, nil
}

func (c *Cache) toProtocolDiagnostics(rawReports []*ProtoDiagnostic) []protocol.Diagnostic {
	var reports []protocol.Diagnostic
	for _, rawReport := range rawReports {
		reports = append(reports, protocol.Diagnostic{
			Range:    toRange(rawReport.Pos),
			Severity: rawReport.Severity,
			Message:  rawReport.Error.Error(),
			Tags:     rawReport.Tags,
			Source:   "protols",
		})
	}
	return reports
}

type workspaceDiagnosticCallbackFunc = func(uri span.URI, reports []protocol.Diagnostic, kind protocol.DocumentDiagnosticReportKind, resultId string)

func (c *Cache) StreamWorkspaceDiagnostics(ctx context.Context, ch chan<- protocol.WorkspaceDiagnosticReportPartialResult) {
	currentDiagnostics := make(map[span.URI][]protocol.Diagnostic)
	diagnosticVersions := make(map[span.URI]int32)
	c.diagHandler.Stream(ctx, func(event DiagnosticEvent, path string, diagnostics ...*ProtoDiagnostic) {
		uri, err := c.resolver.PathToURI(path)
		if err != nil {
			return
		}

		version := diagnosticVersions[uri]
		version++
		diagnosticVersions[uri] = version

		switch event {
		case DiagnosticEventAdd:
			protocolDiagnostics := c.toProtocolDiagnostics(diagnostics)
			currentDiagnostics[uri] = append(currentDiagnostics[uri], protocolDiagnostics...)
			ch <- protocol.WorkspaceDiagnosticReportPartialResult{
				Items: []protocol.Or_WorkspaceDocumentDiagnosticReport{
					{
						Value: protocol.WorkspaceFullDocumentDiagnosticReport{
							Version: version,
							URI:     protocol.URIFromSpanURI(uri),
							FullDocumentDiagnosticReport: protocol.FullDocumentDiagnosticReport{
								Kind:  string(protocol.DiagnosticFull),
								Items: currentDiagnostics[uri],
							},
						},
					},
				},
			}
		case DiagnosticEventClear:
			delete(currentDiagnostics, uri)
			ch <- protocol.WorkspaceDiagnosticReportPartialResult{
				Items: []protocol.Or_WorkspaceDocumentDiagnosticReport{
					{
						Value: protocol.WorkspaceFullDocumentDiagnosticReport{
							Version: version,
							URI:     protocol.URIFromSpanURI(uri),
							FullDocumentDiagnosticReport: protocol.FullDocumentDiagnosticReport{
								Kind:  string(protocol.DiagnosticFull),
								Items: []protocol.Diagnostic{},
							},
						},
					},
				},
			}
		}
	})
}

func (c *Cache) ComputeDocumentLinks(doc protocol.TextDocumentIdentifier) ([]protocol.DocumentLink, error) {
	// link valid imports
	var links []protocol.DocumentLink
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()

	res, err := c.FindResultByURI(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	resAst := res.AST()
	var imports []*ast.ImportNode
	// get the source positions of the import statements
	for _, decl := range resAst.Decls {
		if imp, ok := decl.(*ast.ImportNode); ok {
			imports = append(imports, imp)
		}
	}

	for _, imp := range imports {
		path := imp.Name.AsString()
		nameInfo := resAst.NodeInfo(imp.Name)
		if uri, err := c.resolver.PathToURI(path); err == nil {
			links = append(links, protocol.DocumentLink{
				Range:  toRange(nameInfo),
				Target: string(uri),
			})
		}
	}

	return links, nil
}

func (c *Cache) ComputeInlayHints(doc protocol.TextDocumentIdentifier, rng protocol.Range) ([]protocol.InlayHint, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	hints := []protocol.InlayHint{}
	hints = append(hints, c.computeMessageLiteralHints(doc, rng)...)
	return hints, nil
}

type optionGetter[T proto.Message] interface {
	GetOptions() T
}

func collectOptions[V proto.Message, T ast.OptionDeclNode, U optionGetter[V]](t T, getter U, optionsByNode map[*ast.OptionNode][]protoreflect.ExtensionType) {
	opt, ok := any(t).(*ast.OptionNode)
	if !ok {
		return
	}
	proto.RangeExtensions(getter.GetOptions(), func(et protoreflect.ExtensionType, i interface{}) bool {
		if et.TypeDescriptor().IsExtension() {
			optionsByNode[opt] = append(optionsByNode[opt], et)
		}
		return true
	})
}

var (
	wellKnownFileOptions = map[string]string{
		"java_package":                  "string",
		"java_outer_classname":          "string",
		"java_multiple_files":           "bool",
		"java_generate_equals_and_hash": "bool",
		"java_string_check_utf8":        "bool",
		"optimize_for":                  "google.protobuf.FileOptions.OptimizeMode",
		"go_package":                    "string",
		"cc_generic_services":           "bool",
		"java_generic_services":         "bool",
		"py_generic_services":           "bool",
		"php_generic_services":          "bool",
		"deprecated":                    "bool",
		"cc_enable_arenas":              "bool",
		"objc_class_prefix":             "string",
		"csharp_namespace":              "string",
		"swift_prefix":                  "string",
		"php_class_prefix":              "string",
		"php_namespace":                 "string",
		"php_metadata_namespace":        "string",
		"ruby_package":                  "string",
	}
)

func (c *Cache) computeMessageLiteralHints(doc protocol.TextDocumentIdentifier, rng protocol.Range) []protocol.InlayHint {
	var hints []protocol.InlayHint
	res, err := c.FindResultByURI(doc.URI.SpanURI())
	if err != nil {
		return nil
	}
	fdp := res.FileDescriptorProto()
	mapper, err := c.GetMapper(doc.URI.SpanURI())
	if err != nil {
		return nil
	}
	a := res.AST()

	startOff, endOff, _ := mapper.RangeOffsets(rng)

	optionsByNode := make(map[*ast.OptionNode][]protoreflect.ExtensionType)

	for _, decl := range a.Decls {
		if opt, ok := decl.(*ast.OptionNode); ok {
			opt := opt
			if len(opt.Name.Parts) == 1 {
				info := a.NodeInfo(opt.Name)
				if info.End().Offset <= startOff {
					continue
				} else if info.Start().Offset >= endOff {
					break
				}
				part := opt.Name.Parts[0]
				if wellKnownType, ok := wellKnownFileOptions[part.Value()]; ok {
					hints = append(hints, protocol.InlayHint{
						Kind: protocol.Type,
						Position: protocol.Position{
							Line:      uint32(info.End().Line - 1),
							Character: uint32(info.End().Col - 1),
						},
						Label: []protocol.InlayHintLabelPart{
							{
								Value: wellKnownType,
							},
						},
						PaddingLeft: true,
					})
					continue
				}
			}
			// todo(bug): if more than one FileOption is declared in the same file, each option will show up in all usages of the options in the file
			collectOptions[*descriptorpb.FileOptions](opt, fdp, optionsByNode)
		}
	}
	// collect all options
	for _, svc := range fdp.GetService() {
		for _, decl := range res.ServiceNode(svc).(*ast.ServiceNode).Decls {
			info := a.NodeInfo(decl)
			if info.End().Offset <= startOff {
				continue
			} else if info.Start().Offset >= endOff {
				break
			}
			if opt, ok := decl.(*ast.OptionNode); ok {
				collectOptions[*descriptorpb.ServiceOptions](opt, svc, optionsByNode)
			}
		}
		for _, method := range svc.GetMethod() {
			for _, decl := range res.MethodNode(method).(*ast.RPCNode).Decls {
				info := a.NodeInfo(decl)
				if info.End().Offset <= startOff {
					continue
				} else if info.Start().Offset >= endOff {
					break
				}
				if opt, ok := decl.(*ast.OptionNode); ok {
					collectOptions[*descriptorpb.MethodOptions](opt, method, optionsByNode)
				}
			}
		}
	}
	for _, msg := range fdp.GetMessageType() {
		for _, decl := range res.MessageNode(msg).(*ast.MessageNode).Decls {
			info := a.NodeInfo(decl)
			if info.End().Offset <= startOff {
				continue
			} else if info.Start().Offset >= endOff {
				break
			}
			if opt, ok := decl.(*ast.OptionNode); ok {
				collectOptions[*descriptorpb.MessageOptions](opt, msg, optionsByNode)
			}
		}
		for _, field := range msg.GetField() {
			fieldNode := res.FieldNode(field)
			info := a.NodeInfo(fieldNode)
			if info.End().Offset <= startOff {
				continue
			} else if info.Start().Offset >= endOff {
				break
			}
			switch fieldNode := fieldNode.(type) {
			case *ast.FieldNode:
				for _, opt := range fieldNode.GetOptions().GetElements() {
					collectOptions[*descriptorpb.FieldOptions](opt, field, optionsByNode)
				}
			case *ast.MapFieldNode:
				for _, opt := range fieldNode.GetOptions().GetElements() {
					collectOptions[*descriptorpb.FieldOptions](opt, field, optionsByNode)
				}
			}
		}
	}
	for _, enum := range fdp.GetEnumType() {
		for _, decl := range res.EnumNode(enum).(*ast.EnumNode).Decls {
			info := a.NodeInfo(decl)
			if info.End().Offset <= startOff {
				continue
			} else if info.Start().Offset >= endOff {
				break
			}
			if opt, ok := decl.(*ast.OptionNode); ok {
				collectOptions[*descriptorpb.EnumOptions](opt, enum, optionsByNode)
			}
		}
		for _, val := range enum.GetValue() {
			for _, opt := range res.EnumValueNode(val).(*ast.EnumValueNode).Options.GetElements() {
				collectOptions[*descriptorpb.EnumValueOptions](opt, val, optionsByNode)
			}
		}
	}
	// for _, ext := range fdp.GetExtension() {
	// 	for _, opt := range res.FieldNode(ext).(*ast.FieldNode).GetOptions().GetElements() {
	// 		collectOptions[*descriptorpb.FieldOptions](opt, ext, optionsByNode)
	// 	}
	// }

	allNodes := a.Children()
	for _, node := range allNodes {
		// only look at the decls that overlap the range
		info := a.NodeInfo(node)
		if info.End().Offset <= startOff {
			continue
		} else if info.Start().Offset >= endOff {
			break
		}
		ast.Walk(node, &ast.SimpleVisitor{
			DoVisitOptionNode: func(n *ast.OptionNode) error {
				opts, ok := optionsByNode[n]
				if !ok {
					return nil
				}
				for _, opt := range opts {
					msg := opt.TypeDescriptor().Message()
					if msg != nil {
						fullName := msg.FullName()

						info := a.NodeInfo(n.Val)
						hints = append(hints, protocol.InlayHint{
							Position: protocol.Position{
								Line:      uint32(info.Start().Line) - 1,
								Character: uint32(info.Start().Col) - 1,
							},
							Label: []protocol.InlayHintLabelPart{
								{
									Value:   string(fullName),
									Tooltip: makeTooltip(msg),
								},
							},
							Kind:         protocol.Type,
							PaddingLeft:  true,
							PaddingRight: true,
						})
						if lit, ok := n.Val.(*ast.MessageLiteralNode); ok {
							hints = append(hints, buildMessageLiteralHints(lit, msg, a)...)
						}
					}
				}
				return nil
			},
		})
	}

	return hints
}

func (c *Cache) DocumentSymbolsForFile(doc protocol.TextDocumentIdentifier) ([]protocol.DocumentSymbol, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()

	f, err := c.FindResultByURI(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}

	var symbols []protocol.DocumentSymbol

	fn := f.AST()
	ast.Walk(fn, &ast.SimpleVisitor{
		// DoVisitImportNode: func(node *ast.ImportNode) error {
		// 	s.lg.Debug("found import", zap.String("name", string(node.Name.AsString())))
		// 	symbols = append(symbols, protocol.DocumentSymbol{
		// 		Name:           string(node.Name.AsString()),
		// 		Kind:           protocol.SymbolKindNamespace,
		// 		Range:          posToRange(fn.NodeInfo(node)),
		// 		SelectionRange: posToRange(fn.NodeInfo(node.Name)),
		// 	})
		// 	return nil
		// },
		DoVisitServiceNode: func(node *ast.ServiceNode) error {
			service := protocol.DocumentSymbol{
				Name:           string(node.Name.AsIdentifier()),
				Kind:           protocol.Interface,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Name)),
			}

			ast.Walk(node, &ast.SimpleVisitor{
				DoVisitRPCNode: func(node *ast.RPCNode) error {
					// s.lg.Debug("found rpc", zap.String("name", string(node.Name.AsIdentifier())), zap.String("service", string(node.Name.AsIdentifier())))
					var detail string
					switch {
					case node.Input.Stream != nil && node.Output.Stream != nil:
						detail = "stream (bidirectional)"
					case node.Input.Stream != nil:
						detail = "stream (client)"
					case node.Output.Stream != nil:
						detail = "stream (server)"
					default:
						detail = "unary"
					}
					rpc := protocol.DocumentSymbol{
						Name:           string(node.Name.AsIdentifier()),
						Detail:         detail,
						Kind:           protocol.Method,
						Range:          toRange(fn.NodeInfo(node)),
						SelectionRange: toRange(fn.NodeInfo(node.Name)),
					}

					ast.Walk(node, &ast.SimpleVisitor{
						DoVisitRPCTypeNode: func(node *ast.RPCTypeNode) error {
							rpcType := protocol.DocumentSymbol{
								Name:           string(node.MessageType.AsIdentifier()),
								Kind:           protocol.Class,
								Range:          toRange(fn.NodeInfo(node)),
								SelectionRange: toRange(fn.NodeInfo(node.MessageType)),
							}
							rpc.Children = append(rpc.Children, rpcType)
							return nil
						},
					})
					service.Children = append(service.Children, rpc)
					return nil
				},
			})
			symbols = append(symbols, service)
			return nil
		},
		DoVisitMessageNode: func(node *ast.MessageNode) error {
			sym := protocol.DocumentSymbol{
				Name:           string(node.Name.AsIdentifier()),
				Kind:           protocol.Class,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Name)),
			}
			ast.Walk(node, &ast.SimpleVisitor{
				DoVisitFieldNode: func(node *ast.FieldNode) error {
					sym.Children = append(sym.Children, protocol.DocumentSymbol{
						Name:           string(node.Name.AsIdentifier()),
						Detail:         string(node.FldType.AsIdentifier()),
						Kind:           protocol.Field,
						Range:          toRange(fn.NodeInfo(node)),
						SelectionRange: toRange(fn.NodeInfo(node.Name)),
					})
					return nil
				},
				DoVisitMapFieldNode: func(node *ast.MapFieldNode) error {
					sym.Children = append(sym.Children, protocol.DocumentSymbol{
						Name:           string(node.Name.AsIdentifier()),
						Detail:         fmt.Sprintf("map<%s, %s>", string(node.KeyField().Ident.AsIdentifier()), string(node.ValueField().Ident.AsIdentifier())),
						Kind:           protocol.Field,
						Range:          toRange(fn.NodeInfo(node)),
						SelectionRange: toRange(fn.NodeInfo(node.Name)),
					})
					return nil
				},
			})
			symbols = append(symbols, sym)
			return nil
		},
		DoVisitEnumNode: func(node *ast.EnumNode) error {
			sym := protocol.DocumentSymbol{
				Name:           string(node.Name.AsIdentifier()),
				Kind:           protocol.Enum,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Name)),
			}
			ast.Walk(node, &ast.SimpleVisitor{
				DoVisitEnumValueNode: func(node *ast.EnumValueNode) error {
					sym.Children = append(sym.Children, protocol.DocumentSymbol{
						Name:           string(node.Name.AsIdentifier()),
						Kind:           protocol.EnumMember,
						Range:          toRange(fn.NodeInfo(node)),
						SelectionRange: toRange(fn.NodeInfo(node.Name)),
					})
					return nil
				},
			})
			symbols = append(symbols, sym)
			return nil
		},
		DoVisitExtendNode: func(node *ast.ExtendNode) error {
			sym := protocol.DocumentSymbol{
				Name:           string(node.Extendee.AsIdentifier()),
				Kind:           protocol.Class,
				Range:          toRange(fn.NodeInfo(node)),
				SelectionRange: toRange(fn.NodeInfo(node.Extendee)),
			}
			ast.Walk(node, &ast.SimpleVisitor{
				DoVisitFieldNode: func(node *ast.FieldNode) error {
					sym.Children = append(sym.Children, protocol.DocumentSymbol{
						Name:           string(node.Name.AsIdentifier()),
						Kind:           protocol.Field,
						Range:          toRange(fn.NodeInfo(node)),
						SelectionRange: toRange(fn.NodeInfo(node.Name)),
					})
					return nil
				},
			})
			symbols = append(symbols, sym)
			return nil
		},
	})
	return symbols, nil
}

func (c *Cache) GetSyntheticFileContents(ctx context.Context, uri string) (string, error) {
	uri = strings.TrimSuffix(uri, "#"+c.workspace.Name)
	uri = strings.TrimPrefix(strings.TrimPrefix(uri, "proto:///"), "proto://")
	fh, err := c.resolver.FindFileByPath(uri)
	if err != nil {
		return "", err
	}

	switch {
	case fh.Source != nil:
		b, err := io.ReadAll(fh.Source)
		return string(b), err
	case fh.Desc != nil:
		return printDescriptor(fh.Desc)

	case fh.Proto != nil:
		f, err := protodesc.NewFile(fh.Proto, c.results.AsResolver())
		if err != nil {
			return "", err
		}
		return printDescriptor(f)
	default:
		return "", fmt.Errorf("unimplemented synthetic file type for %s", uri)
	}
}

func (c *Cache) FindTypeDescriptorAtLocation(params protocol.TextDocumentPositionParams) (protoreflect.Descriptor, protocol.Range, error) {
	enc, err := computeSemanticTokens(c, params.TextDocument, &protocol.Range{
		Start: params.Position,
		End:   params.Position,
	})
	if err != nil {
		return nil, protocol.Range{}, err
	}

	item, found := findNarrowestSemanticToken(enc.items, params.Position)
	if !found {
		return nil, protocol.Range{}, nil
	}
	parseRes, err := c.FindParseResultByURI(params.TextDocument.URI.SpanURI())
	if err != nil {
		return nil, protocol.Range{}, err
	}

	switch node := item.node.(type) {
	case ast.IdentValueNode:
		rng := toRange(parseRes.AST().NodeInfo(node))
		name := string(node.AsIdentifier())
		var desc protoreflect.Descriptor
		var unqualifiedName protoreflect.Name
		var pkg protoreflect.FullName
		if strings.Contains(name, ".") {
			// treat as fully qualified name
			fn := protoreflect.FullName(name)
			unqualifiedName = fn.Name()
			pkg = fn.Parent()
		} else {
			unqualifiedName = protoreflect.Name(name)
			pkg = protoreflect.FullName(parseRes.FileDescriptorProto().GetPackage())
		}
		if desc == nil {
			descs := c.FindAllDescriptorsByPrefix(context.TODO(), string(unqualifiedName), pkg)
			if len(descs) > 0 {
				desc = descs[0]
			}
		}
		if desc == nil {
			return nil, protocol.Range{}, nil
		}
		return desc, rng, nil
	default:
		return nil, protocol.Range{}, nil
	}
}

func (c *Cache) FindDefinitionForTypeDescriptor(desc protoreflect.Descriptor) ([]protocol.Location, error) {
	parentFile := desc.ParentFile()
	if parentFile == nil {
		return nil, errors.New("no parent file found for descriptor")
	}
	containingFileResolver, err := c.FindResultByPath(parentFile.Path())
	if err != nil {
		return nil, fmt.Errorf("failed to find containing file for %q: %w", parentFile.Path(), err)
	}
	var node ast.Node
	switch desc := desc.(type) {
	case protoreflect.MessageDescriptor:
		node = containingFileResolver.MessageNode(protoutil.ProtoFromMessageDescriptor(desc))
	case protoreflect.EnumDescriptor:
		node = containingFileResolver.EnumNode(protoutil.ProtoFromEnumDescriptor(desc))
	case protoreflect.ServiceDescriptor:
		node = containingFileResolver.ServiceNode(protoutil.ProtoFromServiceDescriptor(desc))
	case protoreflect.MethodDescriptor:
		node = containingFileResolver.MethodNode(protoutil.ProtoFromMethodDescriptor(desc))
	case protoreflect.FieldDescriptor:
		node = containingFileResolver.FieldNode(protoutil.ProtoFromFieldDescriptor(desc))
	case protoreflect.EnumValueDescriptor:
		node = containingFileResolver.EnumValueNode(protoutil.ProtoFromEnumValueDescriptor(desc))
	case protoreflect.OneofDescriptor:
		node = containingFileResolver.OneofNode(protoutil.ProtoFromOneofDescriptor(desc))
	case protoreflect.FileDescriptor:
		node = containingFileResolver.FileNode()
		c.lg.Debug("definition is an import: ", zap.String("import", containingFileResolver.Path()))
	default:
		return nil, fmt.Errorf("unexpected descriptor type %T", desc)
	}

	info := containingFileResolver.AST().NodeInfo(node)
	uri, err := c.resolver.PathToURI(containingFileResolver.Path())
	if err != nil {
		return nil, err
	}
	return []protocol.Location{
		{
			URI:   protocol.URIFromSpanURI(uri),
			Range: toRange(info),
		},
	}, nil
}

func (c *Cache) FindReferencesForTypeDescriptor(desc protoreflect.Descriptor) ([]protocol.Location, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()

	var wg sync.WaitGroup
	referencePositions := make(chan ast.SourcePosInfo, len(c.results))
	for _, res := range c.results {
		res := res
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, ref := range res.(linker.Result).FindReferences(desc) {
				ref := ref
				referencePositions <- ref
			}
		}()
	}
	go func() {
		wg.Wait()
		close(referencePositions)
	}()

	var locations []protocol.Location
	for posInfo := range referencePositions {
		filename := posInfo.Start().Filename
		uri, err := c.resolver.PathToURI(filename)
		if err != nil {
			continue
		}
		locations = append(locations, protocol.Location{
			URI:   protocol.URIFromSpanURI(uri),
			Range: toRange(posInfo),
		})
	}

	return locations, nil
}

func (c *Cache) ComputeHover(params protocol.TextDocumentPositionParams) (*protocol.Hover, error) {
	desc, rng, err := c.FindTypeDescriptorAtLocation(params)
	if err != nil {
		return nil, err
	}
	tooltip := makeTooltip(desc)
	if tooltip == nil {
		return nil, nil
	}
	return &protocol.Hover{
		Contents: tooltip.Value.(protocol.MarkupContent),
		Range:    rng,
	}, nil
}

func (c *Cache) FindReferences(ctx context.Context, params protocol.TextDocumentPositionParams, refCtx protocol.ReferenceContext) ([]protocol.Location, error) {
	desc, _, err := c.FindTypeDescriptorAtLocation(params)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		return nil, nil
	}

	var locations []protocol.Location

	if refCtx.IncludeDeclaration {
		locations, err = c.FindDefinitionForTypeDescriptor(desc)
		if err != nil {
			return nil, err
		}
	}

	refs, err := c.FindReferencesForTypeDescriptor(desc)
	if err != nil {
		return nil, err
	}
	locations = append(locations, refs...)
	return locations, nil
}

func buildMessageLiteralHints(lit *ast.MessageLiteralNode, msg protoreflect.MessageDescriptor, a *ast.FileNode) []protocol.InlayHint {
	msgFields := msg.Fields()
	var hints []protocol.InlayHint
	for _, field := range lit.Elements {
		fieldDesc := msgFields.ByName(protoreflect.Name(field.Name.Value()))
		if fieldDesc == nil {
			continue
		}
		fieldHint := protocol.InlayHint{
			Kind:         protocol.Type,
			PaddingLeft:  true,
			PaddingRight: true,
		}
		kind := fieldDesc.Kind()
		if kind == protoreflect.MessageKind {
			info := a.NodeInfo(field.Val)
			fieldHint.Position = protocol.Position{
				Line:      uint32(info.Start().Line) - 1,
				Character: uint32(info.Start().Col) - 1,
			}
			fieldHint.Label = append(fieldHint.Label, protocol.InlayHintLabelPart{
				Value:   string(fieldDesc.Message().FullName()),
				Tooltip: makeTooltip(fieldDesc.Message()),
			})
			switch val := field.Val.(type) {
			case *ast.MessageLiteralNode:
				hints = append(hints, buildMessageLiteralHints(val, fieldDesc.Message(), a)...)
			case *ast.ArrayLiteralNode:
			default:
				// hints = append(hints, buildArrayLiteralHints(val, fieldDesc.Message(), a)...)
			}
			fieldHint.PaddingLeft = false
		} else {
			// 	info := a.NodeInfo(field.Sep)
			// 	fieldHint.Position = protocol.Position{
			// 		Line:      uint32(info.Start().Line) - 1,
			// 		Character: uint32(info.Start().Col) - 1,
			// 	}
			// 	fieldHint.Label = append(fieldHint.Label, protocol.InlayHintLabelPart{
			// 		Value: kind.String(),
			// 	})
			// 	fieldHint.PaddingRight = false
		}
		hints = append(hints, fieldHint)
	}
	return hints
}

func printDescriptor(d protoreflect.Descriptor) (string, error) {
	wrap, err := desc.WrapDescriptor(d)
	if err != nil {
		return "", err
	}
	printer := protoprint.Printer{
		CustomSortFunction: SortElements,
		Indent:             "  ",
		Compact:            protoprint.CompactDefault,
	}
	str, err := printer.PrintProtoToString(wrap)
	if err != nil {
		return "", err
	}
	return str, nil
}

func makeTooltip(d protoreflect.Descriptor) *protocol.OrPTooltipPLabel {
	str, err := printDescriptor(d)
	if err != nil {
		return nil
	}
	return &protocol.OrPTooltipPLabel{
		Value: protocol.MarkupContent{
			Kind:  protocol.Markdown,
			Value: fmt.Sprintf("```protobuf\n%s\n```", str),
		},
	}
}

func (c *Cache) FormatDocument(doc protocol.TextDocumentIdentifier, options protocol.FormattingOptions, maybeRange ...protocol.Range) ([]protocol.TextEdit, error) {
	// printer := protoprint.Printer{
	// 	CustomSortFunction: SortElements,
	// 	Indent:             "  ", // todo: tabs break semantic tokens
	// 	Compact:            protoprint.CompactDefault,
	// }

	mapper, err := c.GetMapper(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	res, err := c.FindParseResultByURI(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}

	// if len(maybeRange) == 1 {
	// 	rng := maybeRange[0]
	// 	// format range
	// 	start, end, err := mapper.RangeOffsets(rng)
	// 	if err != nil {
	// 		return nil, err
	// 	}

	// 	// Try to map the range to a single top-level element. If the range overlaps
	// 	// multiple top level elements, we'll just format the whole file.

	// 	targetDesc, err := findDescriptorWithinRangeOffsets(res, start, end)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	splicedBuffer := bytes.NewBuffer(bytes.Clone(mapper.Content[:start]))

	// 	wrap, err := desc.WrapDescriptor(targetDesc)
	// 	if err != nil {
	// 		return nil, err
	// 	}

	// 	err = printer.PrintProto(wrap, splicedBuffer)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	splicedBuffer.Write(mapper.Content[end:])
	// 	spliced := splicedBuffer.Bytes()
	// 	// fmt.Printf("old:\n%s\nnew:\n%s\n", string(mapper.Content), string(spliced))

	// 	edits := diff.Bytes(mapper.Content, spliced)
	// 	return source.ToProtocolEdits(mapper, edits)
	// }

	// wrap, err := desc.WrapFile(res.Descriptor(res.FileNode()).(protoreflect.FileDescriptor))
	// if err != nil {
	// 	return nil, err
	// }
	// format whole file
	buf := bytes.NewBuffer(make([]byte, 0, len(mapper.Content)))
	format := newFormatter(buf, res.AST())
	if err := format.Run(); err != nil {
		return nil, err
	}

	edits := diff.Bytes(mapper.Content, buf.Bytes())
	return source.ToProtocolEdits(mapper, edits)
}

func (c *Cache) FindAllDescriptorsByPrefix(ctx context.Context, prefix string, localPackage protoreflect.FullName) []protoreflect.Descriptor {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	eg, ctx := errgroup.WithContext(ctx)
	resultsByPackage := make([][]protoreflect.Descriptor, len(c.results))
	for i, res := range c.results {
		i, res := i, res
		eg.Go(func() (err error) {
			p := prefix
			if res.Package() == localPackage {
				p = string(localPackage) + "." + p
			}
			resultsByPackage[i], err = res.(linker.Result).FindDescriptorsByPrefix(ctx, p)
			return
		})
	}
	eg.Wait()
	combined := make([]protoreflect.Descriptor, 0, len(c.results))
	for _, results := range resultsByPackage {
		combined = append(combined, results...)
	}
	return combined
}

func (c *Cache) GetCompletions(params *protocol.CompletionParams) (result *protocol.CompletionList, err error) {
	doc := params.TextDocument
	parseRes, err := c.FindParseResultByURI(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	mapper, err := c.GetMapper(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	posOffset, err := mapper.PositionOffset(params.Position)
	if err != nil {
		return nil, err
	}

	fileNode := parseRes.AST()

	completions := []protocol.CompletionItem{}
	thisPackage := parseRes.FileDescriptorProto().GetPackage()

	ast.Walk(fileNode, &ast.SimpleVisitor{
		DoVisitMessageNode: func(node *ast.MessageNode) error {
			scopeBegin := fileNode.NodeInfo(node.OpenBrace).Start().Offset
			scopeEnd := fileNode.NodeInfo(node.CloseBrace).End().Offset

			if posOffset < scopeBegin || posOffset > scopeEnd {
				return nil
			}

			// find the text from the start of the line to the cursor
			startOffset, err := mapper.PositionOffset(protocol.Position{
				Line:      params.Position.Line,
				Character: 0,
			})
			if err != nil {
				return err
			}

			ctx, ca := context.WithTimeout(context.Background(), 1*time.Second)
			defer ca()
			text := strings.TrimLeftFunc(string(mapper.Content[startOffset:posOffset]), unicode.IsSpace)
			fmt.Println("finding completions for", text)
			matches := c.FindAllDescriptorsByPrefix(ctx, text, protoreflect.FullName(thisPackage))
			for _, match := range matches {
				var compl protocol.CompletionItem
				parent := match.ParentFile()
				pkg := parent.Package()
				if pkg != protoreflect.FullName(thisPackage) {
					compl.Label = string(pkg) + "." + string(match.Name())
					compl.AdditionalTextEdits = []protocol.TextEdit{editAddImport(parseRes, parent.Path())}
				} else {
					compl.Label = string(match.Name())
				}
				switch match.(type) {
				case protoreflect.MessageDescriptor:
					compl.Kind = protocol.ClassCompletion
				case protoreflect.EnumDescriptor:
					compl.Kind = protocol.EnumCompletion
				default:
					continue
				}
				completions = append(completions, compl)
			}
			return nil
		},
	})

	return &protocol.CompletionList{
		Items: completions,
	}, nil
}

func editAddImport(parseRes parser.Result, path string) protocol.TextEdit {
	insertionPoint := parseRes.ImportInsertionPoint()
	text := fmt.Sprintf("import \"%s\";\n", path)
	return protocol.TextEdit{
		Range: protocol.Range{
			Start: protocol.Position{
				Line:      uint32(insertionPoint.Line - 1),
				Character: uint32(insertionPoint.Col - 1),
			},
			End: protocol.Position{
				Line:      uint32(insertionPoint.Line - 1),
				Character: uint32(insertionPoint.Col - 1),
			},
		},
		NewText: text,
	}
}

func FastLookupGoModule(filename string) (string, error) {
	// Search the .proto file for `option go_package = "...";`
	// We know this will be somewhere at the top of the file.
	f, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, "option") {
			continue
		}
		index := strings.Index(line, "go_package")
		if index == -1 {
			continue
		}
		for ; index < len(line); index++ {
			if line[index] == '=' {
				break
			}
		}
		for ; index < len(line); index++ {
			if line[index] == '"' {
				break
			}
		}
		if index == len(line) {
			continue
		}
		startIdx := index + 1
		endIdx := strings.LastIndexByte(line, '"')
		if endIdx <= startIdx {
			continue
		}
		return line[startIdx:endIdx], nil
	}
	return "", fmt.Errorf("no go_package option found")
}

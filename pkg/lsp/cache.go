package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"

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

func (c *Cache) TracksURI(uri protocol.DocumentURI) bool {
	_, err := c.resolver.URIToPath(span.URIFromURI(string(uri)))
	return err == nil
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
	cache.Initialize()
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

func (c *Cache) Initialize() {
	c.lg.Debug("initializing")
	defer c.lg.Debug("done initializing")

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
	c.compileLocked(protos...)
}

func (c *Cache) compileLocked(protos ...string) {
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
	c.resultsMu.Unlock()

	syntheticFiles := c.resolver.CheckIncompleteDescriptors(c.compiler.GetImplicitResults())
	if len(syntheticFiles) == 0 {
		return
	}
	c.lg.Debug("building new synthetic sources", zap.Int("sources", len(syntheticFiles)))
	c.compileLocked(syntheticFiles...)
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
		for i, info := range rawReport.RelatedInformation {
			u, err := c.resolver.PathToURI(string(info.Location.URI))
			if err == nil {
				rawReport.RelatedInformation[i].Location.URI = protocol.URIFromSpanURI(u)
			}
		}
		report := protocol.Diagnostic{
			Range:              toRange(rawReport.Pos),
			Severity:           rawReport.Severity,
			Message:            rawReport.Error.Error(),
			Tags:               rawReport.Tags,
			RelatedInformation: rawReport.RelatedInformation,
			Source:             "protols",
		}
		if len(rawReport.CodeActions) > 0 {
			jsonData, err := json.Marshal(CodeActions{Items: rawReport.CodeActions})
			if err != nil {
				c.lg.With(
					zap.Error(err),
					zap.String("sourcePos", rawReport.Pos.String()),
				).Error("failed to marshal suggested fixes")
				continue
			}
			rawMsg := json.RawMessage(jsonData)
			report.Data = &rawMsg
		}
		reports = append(reports, report)
	}
	return reports
}

func (c *Cache) ToProtocolCodeActions(rawCodeActions []CodeAction, associatedDiagnostic *protocol.Diagnostic) []protocol.CodeAction {
	if len(rawCodeActions) == 0 {
		return []protocol.CodeAction{}
	}
	var codeActions []protocol.CodeAction
	for _, rawCodeAction := range rawCodeActions {
		u, err := c.resolver.PathToURI(string(rawCodeAction.Path))
		if err != nil {
			c.lg.With(
				zap.Error(err),
				zap.String("path", string(rawCodeAction.Path)),
			).Error("failed to resolve path to uri")
			continue
		}
		uri := protocol.URIFromSpanURI(u)
		codeActions = append(codeActions, protocol.CodeAction{
			Title:       rawCodeAction.Title,
			Kind:        rawCodeAction.Kind,
			Diagnostics: []protocol.Diagnostic{*associatedDiagnostic},
			IsPreferred: rawCodeAction.IsPreferred,
			Edit: &protocol.WorkspaceEdit{
				Changes: map[protocol.DocumentURI][]protocol.TextEdit{
					uri: rawCodeAction.Edits,
				},
			},
			Command: rawCodeAction.Command,
		})
	}
	return codeActions
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

	res, err := c.FindParseResultByURI(doc.URI.SpanURI())
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
				Target: (*string)(&uri),
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
	// uri = strings.TrimSuffix(uri, "#"+c.workspace.Name)
	// uri = strings.TrimPrefix(strings.TrimPrefix(uri, "proto:///"), "proto://")

	return c.resolver.SyntheticFileContents(span.URIFromURI(uri))
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
		c.Compile(uri)
		f, err := protodesc.NewFile(fh.Proto, c.results.AsResolver())
		if err != nil {
			return "", err
		}
		return printDescriptor(f)
	default:
		return "", fmt.Errorf("unimplemented synthetic file type for %s", uri)
	}
}

type list[T protoreflect.Descriptor] interface {
	Len() int
	Get(i int) T
}

func findByName[T protoreflect.Descriptor](l list[T], name string) (entry T) {
	isFullName := strings.Contains(name, ".")
	for i := 0; i < l.Len(); i++ {
		if isFullName {
			if string(l.Get(i).FullName()) == name {
				entry = l.Get(i)
				break
			}
		} else {
			if string(l.Get(i).Name()) == name {
				entry = l.Get(i)
				break
			}
		}
	}
	return
}

type stackEntry struct {
	node ast.Node
	desc protoreflect.Descriptor
	prev *stackEntry
}

func (s *stackEntry) isResolved() bool {
	return s.desc != nil
}

func (s *stackEntry) nextResolved() *stackEntry {
	res := s
	for {
		if res == nil {
			panic("bug: stackEntry.nextResolved() called with no resolved entry")
		}
		if res.isResolved() {
			return res
		}
		res = res.prev
	}
}

type stack []*stackEntry

func (s *stack) push(node ast.Node, desc protoreflect.Descriptor) {
	e := &stackEntry{
		node: node,
		desc: desc,
	}
	if len(*s) > 0 {
		(*s)[len(*s)-1].prev = e
	}
	*s = append(*s, e)
}

func (c *Cache) FindTypeDescriptorAtLocation(params protocol.TextDocumentPositionParams) (protoreflect.Descriptor, protocol.Range, error) {
	parseRes, err := c.FindParseResultByURI(params.TextDocument.URI.SpanURI())
	if err != nil {
		return nil, protocol.Range{}, err
	}
	linkRes, err := c.FindResultByURI(params.TextDocument.URI.SpanURI())
	if err != nil {
		return nil, protocol.Range{}, err
	}

	mapper, err := c.GetMapper(params.TextDocument.URI.SpanURI())
	if err != nil {
		return nil, protocol.Range{}, err
	}

	enc := semanticItems{
		options: semanticItemsOptions{
			skipComments: true,
		},
		parseRes: parseRes,
		linkRes:  linkRes,
	}
	offset, err := mapper.PositionOffset(params.Position)
	if err != nil {
		return nil, protocol.Range{}, err
	}
	root := parseRes.AST()

	token := root.TokenAtOffset(offset)
	computeSemanticTokens(c, &enc, ast.WithIntersection(token))

	item, found := findNarrowestSemanticToken(parseRes, enc.items, params.Position)
	if !found {
		return nil, protocol.Range{}, nil
	}

	// traverse the path backwards to find the closest top-level mapped descriptor,
	// then traverse forwards to find the deeply nested descriptor for the original
	// ast node
	stack := stack{}
	// var haveDescriptor protoreflect.Descriptor

	for i := len(item.path) - 1; i >= 0; i-- {
		currentNode := item.path[i]
		nodeDescriptor := parseRes.Descriptor(currentNode)
		if nodeDescriptor == nil {
			// this node does not directly map to a descriptor. push it on the stack
			// and go up one level
			stack.push(currentNode, nil)
		} else {
			// this node does directly map to a descriptor.
			var desc protoreflect.Descriptor
			switch nodeDescriptor := nodeDescriptor.(type) {
			case *descriptorpb.FileDescriptorProto:
				desc = linkRes.ParentFile()
			case *descriptorpb.DescriptorProto:
				var typeName string
				// check if it's a synthetic map field
				if nodeDescriptor.GetOptions().GetMapEntry() {
					// if it is, we're looking for the value message
					typeName = strings.TrimPrefix(nodeDescriptor.Field[1].GetTypeName(), ".")
				} else {
					typeName = nodeDescriptor.GetName()
				}
				// check if we're looking for a nested message
				if i > 0 {
					if _, ok := item.path[i-1].(*ast.MessageNode); ok {
						// the immediate parent is another message, so this message is not
						// a top-level descriptor. push it on the stack and go up one level
						stack.push(currentNode, nil)
						continue
					}
				}
				// search top-level messages
				desc = findByName[protoreflect.MessageDescriptor](linkRes.Messages(), typeName)
			case *descriptorpb.EnumDescriptorProto:
				// check if we're looking for an enum nested in a message
				// (enums can't be nested in other enums)
				if i > 0 {
					if _, ok := item.path[i-1].(*ast.MessageNode); ok {
						// the immediate parent is a message, so this enum is not
						// a top-level descriptor. push it on the stack and go up one level
						stack.push(currentNode, nil)
						continue
					}
				}
				desc = findByName[protoreflect.EnumDescriptor](linkRes.Enums(), nodeDescriptor.GetName())
			case *descriptorpb.ServiceDescriptorProto:
				desc = linkRes.Services().ByName(protoreflect.Name(nodeDescriptor.GetName()))
			case *descriptorpb.UninterpretedOption_NamePart:
				desc = linkRes.FindOptionNameFieldDescriptor(nodeDescriptor)
			case *descriptorpb.UninterpretedOption:
				desc = linkRes.FindOptionMessageDescriptor(nodeDescriptor)
			default:
				// not a top-level descriptor. push it on the stack and go up one level
				stack.push(currentNode, nil)
				continue
			}
			if desc == nil {
				return nil, protocol.Range{}, fmt.Errorf("could not find descriptor for %T", nodeDescriptor)
			}
			stack.push(currentNode, desc)
			break
		}
	}

	// fmt.Printf("descriptor: [%T] %v\n", haveDescriptor, haveDescriptor.FullName())

	// fast path: the node is directly mapped to a resolved top-level descriptor
	if len(stack) == 1 && stack[0].desc != nil {
		return stack[0].desc, toRange(root.NodeInfo(stack[0].node)), nil
	}

	for i := len(stack) - 1; i >= 0; i-- {
		want := stack[i]
		if want.isResolved() {
			continue
		}
		have := want.nextResolved()
		switch haveDesc := have.desc.(type) {
		case protoreflect.FileDescriptor:
			switch wantNode := want.node.(type) {
			case ast.FileElement:
				switch wantNode := wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.FileOptions).ProtoReflect().Descriptor()
				case *ast.ImportNode:
					want.desc = findByName[protoreflect.FileImport](haveDesc.Imports(), wantNode.Name.AsString())
				case *ast.MessageNode:
					want.desc = findByName[protoreflect.MessageDescriptor](haveDesc.Messages(), string(wantNode.Name.AsIdentifier()))
				case *ast.EnumNode:
					want.desc = findByName[protoreflect.EnumDescriptor](haveDesc.Enums(), string(wantNode.Name.AsIdentifier()))
				case *ast.ExtendNode:
					want.desc = haveDesc
				case *ast.ServiceNode:
					want.desc = findByName[protoreflect.ServiceDescriptor](haveDesc.Services(), string(wantNode.Name.AsIdentifier()))
				}
			case *ast.FieldNode:
				want.desc = findByName[protoreflect.FieldDescriptor](haveDesc.Extensions(), string(wantNode.Name.AsIdentifier()))
			case *ast.CompoundIdentNode:
				switch prevNode := want.prev.node.(type) {
				case *ast.ExtendNode:
					// looking for the extendee in the "extend <extendee> {" statement
					if wantNode.AsIdentifier() == prevNode.Extendee.AsIdentifier() {
						want.desc = linkRes.FindExtendeeDescriptorByName(protoreflect.FullName(wantNode.AsIdentifier()))
					}
				}
			}
		case protoreflect.MessageDescriptor:
			switch wantNode := want.node.(type) {
			case ast.MessageElement:
				switch wantNode := wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.MessageOptions).ProtoReflect().Descriptor()
				case *ast.FieldNode:
					want.desc = findByName[protoreflect.FieldDescriptor](haveDesc.Fields(), string(wantNode.Name.AsIdentifier()))
				case *ast.MapFieldNode:
					want.desc = findByName[protoreflect.FieldDescriptor](haveDesc.Fields(), string(wantNode.Name.AsIdentifier()))
				case *ast.OneofNode:
					want.desc = findByName[protoreflect.OneofDescriptor](haveDesc.Oneofs(), string(wantNode.Name.AsIdentifier()))
				case *ast.GroupNode:
					want.desc = findByName[protoreflect.FieldDescriptor](haveDesc.Fields(), string(wantNode.Name.AsIdentifier()))
				case *ast.MessageNode:
					want.desc = findByName[protoreflect.MessageDescriptor](haveDesc.Messages(), string(wantNode.Name.AsIdentifier()))
				case *ast.EnumNode:
					want.desc = findByName[protoreflect.EnumDescriptor](haveDesc.Enums(), string(wantNode.Name.AsIdentifier()))
				case *ast.ExtendNode:
					want.desc = findByName[protoreflect.FieldDescriptor](haveDesc.Extensions(), string(wantNode.Extendee.AsIdentifier()))
				case *ast.ExtensionRangeNode:
				case *ast.ReservedNode:
				}
			case *ast.MapTypeNode:
				want.desc = haveDesc
			case *ast.FieldReferenceNode:
				if wantNode.IsAnyTypeReference() {
					want.desc = linkRes.FindMessageDescriptorByTypeReferenceURLNode(wantNode)
				} else {
					want.desc = findByName[protoreflect.FieldDescriptor](haveDesc.Fields(), string(wantNode.Name.AsIdentifier()))
				}
			case *ast.MessageLiteralNode:
				want.desc = haveDesc
			case *ast.MessageFieldNode:
				name := wantNode.Name
				if name.IsAnyTypeReference() {
					want.desc = linkRes.FindMessageDescriptorByTypeReferenceURLNode(name)
				} else {
					want.desc = findByName[protoreflect.FieldDescriptor](haveDesc.Fields(), string(wantNode.Name.Value()))
				}
			case ast.IdentValueNode:
				want.desc = haveDesc
			}
		case protoreflect.ExtensionTypeDescriptor:
			switch wantNode := want.node.(type) {
			case ast.IdentValueNode:
				switch containingField := want.prev.node.(type) {
				case *ast.FieldReferenceNode:
					want.desc = haveDesc
				case *ast.FieldNode:
					switch wantNode {
					case containingField.Name:
						want.desc = haveDesc.Descriptor()
					case containingField.FldType:
						want.desc = haveDesc.Message()
					}
				}
			}
		case protoreflect.FieldDescriptor:
			switch wantNode := want.node.(type) {
			case ast.FieldDeclNode:
				switch wantNode := wantNode.(type) {
				case *ast.FieldNode:
					want.desc = findByName[protoreflect.FieldDescriptor](haveDesc.Message().Fields(), string(wantNode.Name.AsIdentifier()))
				case *ast.GroupNode:
					want.desc = findByName[protoreflect.FieldDescriptor](haveDesc.Message().Fields(), string(wantNode.Name.AsIdentifier()))
				case *ast.MapFieldNode:
					want.desc = findByName[protoreflect.FieldDescriptor](haveDesc.Message().Fields(), string(wantNode.Name.AsIdentifier()))
				case *ast.SyntheticMapField:
					want.desc = findByName[protoreflect.FieldDescriptor](haveDesc.Message().Fields(), string(wantNode.Ident.AsIdentifier()))
				}
			case *ast.FieldReferenceNode:
				want.desc = haveDesc
				// if wantNode.IsAnyTypeReference() {
				// 	want.desc = linkRes.FindMessageDescriptorByTypeReferenceURLNode(wantNode)
				// } else {
				// 	want.desc = findByName[protoreflect.FieldDescriptor](haveDesc.Message().Fields(), string(wantNode.Name.AsIdentifier()))
				// }
			case *ast.MessageLiteralNode:
				want.desc = haveDesc
			case *ast.MessageFieldNode:
				name := wantNode.Name
				if name.IsAnyTypeReference() {
					want.desc = linkRes.FindMessageDescriptorByTypeReferenceURLNode(name)
				} else {
					want.desc = findByName[protoreflect.FieldDescriptor](haveDesc.Message().Fields(), string(wantNode.Name.Value()))
				}
			case *ast.CompactOptionsNode:
				want.desc = haveDesc.Options().(*descriptorpb.FieldOptions).ProtoReflect().Descriptor()
			case ast.IdentValueNode:
				// need to disambiguate
				switch haveNode := have.node.(type) {
				case *ast.FieldReferenceNode:
					want.desc = haveDesc
				case ast.FieldDeclNode:
					switch want.node {
					case haveNode.FieldType():
						switch {
						case haveDesc.IsExtension():
							// keep the field descriptor
						case haveDesc.IsMap():
							want.desc = haveDesc.MapValue()
						case haveDesc.Kind() == protoreflect.MessageKind:
							want.desc = haveDesc.Message()
						case haveDesc.Kind() == protoreflect.EnumKind:
							want.desc = haveDesc.Enum()
						}
					case haveNode.FieldName():
						// keep the field descriptor
						// this may be nil if we're in a regular message field, but set if
						// we are in a message literal
						if want.desc == nil {
							want.desc = have.desc
						}
					}
				}
			}
		case protoreflect.EnumDescriptor:
			switch wantNode := want.node.(type) {
			case ast.EnumElement:
				switch wantNode := wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.EnumOptions).ProtoReflect().Descriptor()
				case *ast.EnumValueNode:
					want.desc = findByName[protoreflect.EnumValueDescriptor](haveDesc.Values(), string(wantNode.Name.AsIdentifier()))
				case *ast.ReservedNode:
				}
				// default:
				// if enumNode, ok := parentNode.(*ast.EnumValueNode); ok {
				// 	if want == enumNode.Name {
				// 		want.resolved = have.Values().ByName(protoreflect.Name(enumNode.Name.AsIdentifier()))
				// 	}
				// }
			case ast.IdentValueNode:
				want.desc = haveDesc
			}
		case protoreflect.EnumValueDescriptor:
			switch wantNode := want.node.(type) {
			case ast.EnumValueDeclNode:
				switch wantNode.(type) {
				case *ast.EnumValueNode:
					want.desc = haveDesc // ??
				case ast.NoSourceNode:
				}
			case *ast.CompactOptionsNode:
				want.desc = haveDesc.Options().(*descriptorpb.EnumValueOptions).ProtoReflect().Descriptor()
			case ast.IdentValueNode:
				want.desc = haveDesc
			}
		case protoreflect.ServiceDescriptor:
			switch wantNode := want.node.(type) {
			case ast.ServiceElement:
				switch wantNode := wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.ServiceOptions).ProtoReflect().Descriptor()
				case *ast.RPCNode:
					want.desc = findByName[protoreflect.MethodDescriptor](haveDesc.Methods(), string(wantNode.Name.AsIdentifier()))
				}
			case ast.IdentValueNode:
				want.desc = haveDesc
			}
		case protoreflect.MethodDescriptor:
			switch wantNode := want.node.(type) {
			case ast.RPCElement:
				switch wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.MethodOptions).ProtoReflect().Descriptor()
				default:
				}
			case *ast.RPCTypeNode:
				if haveNode, ok := have.node.(*ast.RPCNode); ok {
					switch want.node {
					case haveNode.Input:
						want.desc = haveDesc.Input()
					case haveNode.Output:
						want.desc = haveDesc.Output()
					}
				}
			case *ast.CompactOptionsNode:
				want.desc = haveDesc.Options().(*descriptorpb.MethodOptions).ProtoReflect().Descriptor()
			case ast.IdentValueNode:
				want.desc = haveDesc
			}
		case protoreflect.OneofDescriptor:
			switch wantNode := want.node.(type) {
			case ast.OneofElement:
				switch wantNode := wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.OneofOptions).ProtoReflect().Descriptor()
				case *ast.FieldNode:
					want.desc = findByName[protoreflect.FieldDescriptor](haveDesc.Fields(), string(wantNode.Name.AsIdentifier()))
				}
			case ast.IdentValueNode:
				want.desc = haveDesc
			}
		default:
			return nil, protocol.Range{}, fmt.Errorf("unknown descriptor type %T", have.desc)
		}
		if want.desc == nil {
			return nil, protocol.Range{}, fmt.Errorf("failed to find descriptor for %T/%T", want.desc, want.node)
		}

	}

	if len(stack) == 0 {
		// nothing relevant found
		return nil, protocol.Range{}, nil
	}

	return stack[0].desc, toRange(parseRes.AST().NodeInfo(stack[0].node)), nil
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
		if !desc.IsExtension() {
			node = containingFileResolver.FieldNode(protoutil.ProtoFromFieldDescriptor(desc))
		} else {
			exts := desc.ParentFile().Extensions()
			for i := 0; i < exts.Len(); i++ {
				ext := exts.Get(i)
				if ext.FullName() == desc.FullName() {
					node = containingFileResolver.FieldNode(protoutil.ProtoFromFieldDescriptor(ext))
					break
				}
			}
		}
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
	if node == nil {
		return nil, fmt.Errorf("failed to find node for %q", desc.FullName())
	}

	if _, ok := node.(ast.NoSourceNode); ok {
		return nil, fmt.Errorf("no source available")
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
	wg.Add(len(c.results))
	for _, res := range c.results {
		res := res
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
	mapper, err := c.GetMapper(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	res, err := c.FindParseResultByURI(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}

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

package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/protoutil"
	"github.com/bufbuild/protocompile/reporter"
	gsync "github.com/kralicky/gpkg/sync"
	"github.com/kralicky/protols/pkg/format"
	"golang.org/x/exp/slices"
	"golang.org/x/sync/errgroup"
	"golang.org/x/tools/gopls/pkg/lsp/cache"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
	"golang.org/x/tools/gopls/pkg/lsp/source"
	"golang.org/x/tools/gopls/pkg/span"
	"golang.org/x/tools/pkg/diff"
	"golang.org/x/tools/pkg/jsonrpc2"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Cache is responsible for keeping track of all the known proto source files
// and definitions.
type Cache struct {
	workspace   protocol.WorkspaceFolder
	compiler    *Compiler
	resolver    *Resolver
	diagHandler *DiagnosticHandler
	resultsMu   sync.RWMutex
	results     linker.Files
	// unlinkedResultsMu has an invariant that resultsMu is write-locked; it expects
	// to be required only during compilation. This means that if resultsMu is
	// held (for reading or writing), unlinkedResultsMu does not need to be held.
	unlinkedResultsMu sync.Mutex
	unlinkedResults   map[protocompile.ResolvedPath]parser.Result
	indexMu           sync.RWMutex

	inflightTasksInvalidate gsync.Map[protocompile.ResolvedPath, time.Time]
	inflightTasksCompile    gsync.Map[protocompile.ResolvedPath, time.Time]
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
	if c.results == nil && len(c.unlinkedResults) == 0 {
		return nil, fmt.Errorf("no results or partial results exist")
	}
	path, err := c.resolver.URIToPath(uri)
	if err != nil {
		return nil, err
	}
	if pr, ok := c.unlinkedResults[protocompile.ResolvedPath(path)]; ok {
		return pr, nil
	}
	return c.FindResultByURI(uri)
}

// // FindFileByPath implements linker.Resolver.
// func (c *Cache) FindFileByPath(path protocompile.UnresolvedPath) (protoreflect.FileDescriptor, error) {
// 	c.resultsMu.RLock()
// 	defer c.resultsMu.RUnlock()
// 	c.resolver.f
// 	return c.results.AsResolver().FindFileByPath(path)
// }

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

// var _ linker.Resolver = (*Cache)(nil)

type Compiler struct {
	*protocompile.Compiler
	fs      *cache.OverlayFS
	workdir string
}

var requiredGoEnvVars = []string{"GO111MODULE", "GOFLAGS", "GOINSECURE", "GOMOD", "GOMODCACHE", "GONOPROXY", "GONOSUMDB", "GOPATH", "GOPROXY", "GOROOT", "GOSUMDB", "GOWORK"}

type CacheOptions struct {
	searchPattern string
}

type CacheOption func(*CacheOptions)

func (o *CacheOptions) apply(opts ...CacheOption) {
	for _, op := range opts {
		op(o)
	}
}

func NewCache(workspace protocol.WorkspaceFolder, opts ...CacheOption) *Cache {
	options := CacheOptions{}
	options.apply(opts...)
	workdir := span.URIFromURI(workspace.URI).Filename()
	// NewCache creates a new cache.
	diagHandler := NewDiagnosticHandler()
	reporter := reporter.NewReporter(diagHandler.HandleError, diagHandler.HandleWarning)
	resolver := NewResolver(workspace)

	compiler := &Compiler{
		fs: resolver.OverlayFS,
		Compiler: &protocompile.Compiler{
			Resolver:                     resolver,
			MaxParallelism:               runtime.NumCPU() * 4,
			Reporter:                     reporter,
			SourceInfoMode:               protocompile.SourceInfoExtraComments | protocompile.SourceInfoExtraOptionLocations,
			RetainResults:                true,
			RetainASTs:                   true,
			IncludeDependenciesInResults: true,
		},
		workdir: workdir,
	}
	cache := &Cache{
		workspace:       workspace,
		compiler:        compiler,
		resolver:        resolver,
		diagHandler:     diagHandler,
		unlinkedResults: make(map[protocompile.ResolvedPath]parser.Result),
	}
	compiler.Hooks = protocompile.CompilerHooks{
		PreInvalidate:  cache.preInvalidateHook,
		PostInvalidate: cache.postInvalidateHook,
		PreCompile:     cache.preCompile,
		PostCompile:    cache.postCompile,
	}
	return cache
}

func (c *Cache) LoadFiles(files []string) {
	slog.Debug("initializing")
	defer slog.Debug("done initializing")

	created := make([]source.FileModification, len(files))
	for i, f := range files {
		created[i] = source.FileModification{
			Action: source.Create,
			OnDisk: true,
			URI:    span.URIFromPath(f),
		}
	}

	if err := c.DidModifyFiles(context.TODO(), created); err != nil {
		slog.Error("failed to index files", "error", err)
	}
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

func (c *Cache) preInvalidateHook(path protocompile.ResolvedPath, reason string) {
	slog.Debug("invalidating file", "path", path, "reason", reason)
	c.inflightTasksInvalidate.Store(path, time.Now())
	c.diagHandler.ClearDiagnosticsForPath(string(path))
}

func (c *Cache) postInvalidateHook(path protocompile.ResolvedPath, prevResult linker.File, willRecompile bool) {
	startTime, _ := c.inflightTasksInvalidate.LoadAndDelete(path)
	slog.Debug("file invalidated", "path", path, "took", time.Since(startTime))
	// if !willRecompile {
	// 	c.resultsMu.Lock()
	// 	defer c.resultsMu.Unlock()
	// 	slog.Debug("file deleted, clearing linker result", "path", path)
	// 	for i, f := range c.results {
	// 		if f.Path() == path {
	// 			c.results = append(c.results[:i], c.results[i+1:]...)
	// 			break
	// 		}
	// 	}
	// }
}

func (c *Cache) preCompile(path protocompile.ResolvedPath) {
	slog.Debug(fmt.Sprintf("compiling %s\n", path))
	c.inflightTasksCompile.Store(path, time.Now())
	c.unlinkedResultsMu.Lock()
	defer c.unlinkedResultsMu.Unlock()
	delete(c.unlinkedResults, path)
}

func (c *Cache) postCompile(path protocompile.ResolvedPath) {
	startTime, ok := c.inflightTasksCompile.LoadAndDelete(path)
	if ok {
		slog.Debug(fmt.Sprintf("compiled %s (took %s)\n", path, time.Since(startTime)))
	} else {
		slog.Debug(fmt.Sprintf("compiled %s\n", path))
	}
}

func (c *Cache) Compile(protos ...string) {
	// c.compileLock.Lock()
	// defer c.compileLock.Unlock()
	c.resultsMu.Lock()
	defer c.resultsMu.Unlock()
	c.compileLocked(protos...)
}

func (c *Cache) compileLocked(protos ...string) {
	slog.Info("compiling", "protos", len(protos))

	resolved := make([]protocompile.ResolvedPath, 0, len(protos))
	for _, proto := range protos {
		resolved = append(resolved, protocompile.ResolvedPath(proto))
	}
	res, err := c.compiler.Compile(context.TODO(), resolved...)
	if err != nil {
		if !errors.Is(err, reporter.ErrInvalidSource) {
			slog.With("error", err).Error("failed to compile")
			return
		}
	}
	// important to lock resultsMu here so that it can be modified in compile hooks
	// c.resultsMu.Lock()
	slog.Info("done compiling", "protos", len(protos))
	for _, r := range res.Files {
		path := r.Path()
		found := false
		// delete(c.partialResults, path)
		for i, f := range c.results {
			// todo: this is big slow
			if f.Path() == path {
				found = true
				slog.With("path", path).Debug("updating existing linker result")
				c.results[i] = r
				break
			}
		}
		if !found {
			slog.With("path", path).Debug("adding new linker result")
			c.results = append(c.results, r)
		}
	}
	c.unlinkedResultsMu.Lock()
	for path, partial := range res.UnlinkedParserResults {
		partial := partial
		slog.With("path", path).Debug("adding new partial linker result")
		c.unlinkedResults[path] = partial
	}
	c.unlinkedResultsMu.Unlock()
	// c.resultsMu.Unlock()

	syntheticFiles := c.resolver.CheckIncompleteDescriptors(c.results)
	if len(syntheticFiles) == 0 {
		return
	}
	slog.Debug("building new synthetic sources", "sources", len(syntheticFiles))
	c.compileLocked(syntheticFiles...)
}

// func (s *Cache) OnFileOpened(doc protocol.TextDocumentItem) {
// 	slog.With(
// 		"file", string(doc.URI),
// 		"path", s.filePathsByURI[doc.URI.SpanURI()],
// 	).Debug("file opened")
// 	s.compiler.overlay.Create(doc.URI.SpanURI(), s.filePathsByURI[doc.URI.SpanURI()], []byte(doc.Text))
// }

// func (s *Cache) OnFileClosed(doc protocol.TextDocumentIdentifier) {
// 	slog.With(
// 		"file", string(doc.URI),
// 	).Debug("file closed")
// 	s.compiler.overlay.Delete(s.filePathsByURI[doc.URI.SpanURI()])
// }

// func (s *Cache) OnFileModified(f protocol.VersionedTextDocumentIdentifier, contentChanges []protocol.TextDocumentContentChangeEvent) error {
// 	s.todoModLock.Lock()
// 	defer s.todoModLock.Unlock()
// 	slog.With(
// 		"file", string(f.URI),
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
// 	slog.With(
// 		"files", paths,
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
// 			slog.With(
// 				"filename", filename,
// 				"error", err,
// 			).Debug("failed to lookup go module")
// 			continue
// 		}
// 		canonicalName := filepath.Join(goPkg, filepath.Base(filename))
// 		c.filePathsByURI[uri] = canonicalName
// 		c.fileURIsByPath[canonicalName] = uri
// 		resolved = append(resolved, canonicalName)
// 	}
// 	c.indexMu.Unlock()
// 	slog.With(
// 		"files", len(resolved),
// 	).Debug("files created")
// 	c.Compile(resolved...)

// 	return nil
// }

// func (c *Cache) OnFilesRenamed(f []protocol.FileRename) error {
// 	slog.With(
// 		"files", f,
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
			slog.With(
				"error", err,
				"uri", m.URI.Filename(),
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
	if len(toRecompile) > 0 {
		c.Compile(toRecompile...)
	}
	return nil
}

// func (s *Cache) OnFileSaved(f *protocol.DidSaveTextDocumentParams) error {
// 	slog.With(
// 		"file", string(f.TextDocument.URI),
// 	).Debug("file modified")
// 	s.compiler.overlay.ReloadFromDisk(f.TextDocument.URI.SpanURI())
// 	s.Compile(s.filePathsByURI[f.TextDocument.URI.SpanURI()])
// 	return nil
// }

func (c *Cache) ComputeSemanticTokens(doc protocol.TextDocumentIdentifier) ([]uint32, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	if ok, err := c.latestDocumentContentsWellFormedLocked(doc.URI.SpanURI()); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("document contents not well formed")
	}

	result, err := semanticTokensFull(c, doc)
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (c *Cache) ComputeSemanticTokensRange(doc protocol.TextDocumentIdentifier, rng protocol.Range) ([]uint32, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	if ok, err := c.latestDocumentContentsWellFormedLocked(doc.URI.SpanURI()); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("document contents not well formed")
	}

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
		slog.With(
			"error", err,
			"uri", string(uri),
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
				slog.With(
					"error", err,
					"sourcePos", rawReport.Pos.String(),
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
			slog.With(
				"error", err,
				"path", string(rawCodeAction.Path),
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

func (c *Cache) ComputeInlayHints(doc protocol.TextDocumentIdentifier, rng protocol.Range) ([]protocol.InlayHint, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	if ok, err := c.latestDocumentContentsWellFormedLocked(doc.URI.SpanURI()); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("document contents not well formed")
	}

	hints := []protocol.InlayHint{}
	hints = append(hints, c.computeMessageLiteralHints(doc, rng)...)
	hints = append(hints, c.computeImportHints(doc, rng)...)
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
	mapper, err := c.GetMapper(doc.URI.SpanURI())
	if err != nil {
		return nil
	}
	a := res.AST()
	startOff, endOff, _ := mapper.RangeOffsets(rng)
	startToken := a.TokenAtOffset(startOff)
	endToken := a.TokenAtOffset(endOff)
	res.RangeFieldReferenceNodesWithDescriptors(func(node ast.Node, desc protoreflect.FieldDescriptor) bool {
		switch node := node.(type) {
		case *ast.FieldReferenceNode:
			if node.Start() > endToken || node.End() < startToken {
				return true
			}
			if node.IsAnyTypeReference() {
				return true
			}
			// insert before the closing )
			closeParen := node.Close
			if closeParen == nil {
				return true
			}
			info := a.NodeInfo(closeParen)
			if desc.Kind() == protoreflect.MessageKind && !desc.IsMap() && !desc.IsList() {
				fieldHint := protocol.InlayHint{
					Kind:         protocol.Type,
					PaddingLeft:  true,
					PaddingRight: false,
				}
				fieldHint.Position = protocol.Position{
					Line:      uint32(info.Start().Line) - 1,
					Character: uint32(info.Start().Col) - 1,
				}
				var location *protocol.Location
				if locations, err := c.FindDefinitionForTypeDescriptor(desc.Message()); err == nil && len(locations) > 0 {
					location = &locations[0]
				}

				fieldHint.Label = append(fieldHint.Label, protocol.InlayHintLabelPart{
					Value: string(desc.Message().Name()),
					// Tooltip:  makeTooltip(desc.Message()),
					Location: location,
				})
				// fieldHint.PaddingLeft = false
				hints = append(hints, fieldHint)
			}
		}
		return true
	})
	return hints

	// fdp := res.FileDescriptorProto()
	// if err != nil {
	// 	return nil
	// }
	// a := res.AST()

	// startOff, endOff, _ := mapper.RangeOffsets(rng)

	// optionsByNode := make(map[*ast.OptionNode][]protoreflect.ExtensionType)

	// for _, decl := range a.Decls {
	// 	if opt, ok := decl.(*ast.OptionNode); ok {
	// 		opt := opt
	// 		if len(opt.Name.Parts) == 1 {
	// 			info := a.NodeInfo(opt.Name)
	// 			if info.End().Offset <= startOff {
	// 				continue
	// 			} else if info.Start().Offset >= endOff {
	// 				break
	// 			}
	// 			part := opt.Name.Parts[0]
	// 			if wellKnownType, ok := wellKnownFileOptions[part.Value()]; ok {
	// 				hints = append(hints, protocol.InlayHint{
	// 					Kind: protocol.Type,
	// 					Position: protocol.Position{
	// 						Line:      uint32(info.End().Line - 1),
	// 						Character: uint32(info.End().Col - 1),
	// 					},
	// 					Label: []protocol.InlayHintLabelPart{
	// 						{
	// 							Value: wellKnownType,
	// 						},
	// 					},
	// 					PaddingLeft: true,
	// 				})
	// 				continue
	// 			}
	// 		}
	// 		// todo(bug): if more than one FileOption is declared in the same file, each option will show up in all usages of the options in the file
	// 		collectOptions[*descriptorpb.FileOptions](opt, fdp, optionsByNode)
	// 	}
	// }
	// // collect all options
	// for _, svc := range fdp.GetService() {
	// 	for _, decl := range res.ServiceNode(svc).(*ast.ServiceNode).Decls {
	// 		info := a.NodeInfo(decl)
	// 		if info.End().Offset <= startOff {
	// 			continue
	// 		} else if info.Start().Offset >= endOff {
	// 			break
	// 		}
	// 		if opt, ok := decl.(*ast.OptionNode); ok {
	// 			collectOptions[*descriptorpb.ServiceOptions](opt, svc, optionsByNode)
	// 		}
	// 	}
	// 	for _, method := range svc.GetMethod() {
	// 		for _, decl := range res.MethodNode(method).(*ast.RPCNode).Decls {
	// 			info := a.NodeInfo(decl)
	// 			if info.End().Offset <= startOff {
	// 				continue
	// 			} else if info.Start().Offset >= endOff {
	// 				break
	// 			}
	// 			if opt, ok := decl.(*ast.OptionNode); ok {
	// 				collectOptions[*descriptorpb.MethodOptions](opt, method, optionsByNode)
	// 			}
	// 		}
	// 	}
	// }
	// for _, msg := range fdp.GetMessageType() {
	// 	for _, decl := range res.MessageNode(msg).(*ast.MessageNode).Decls {
	// 		info := a.NodeInfo(decl)
	// 		if info.End().Offset <= startOff {
	// 			continue
	// 		} else if info.Start().Offset >= endOff {
	// 			break
	// 		}
	// 		if opt, ok := decl.(*ast.OptionNode); ok {
	// 			collectOptions[*descriptorpb.MessageOptions](opt, msg, optionsByNode)
	// 		}
	// 	}
	// 	for _, field := range msg.GetField() {
	// 		fieldNode := res.FieldNode(field)
	// 		info := a.NodeInfo(fieldNode)
	// 		if info.End().Offset <= startOff {
	// 			continue
	// 		} else if info.Start().Offset >= endOff {
	// 			break
	// 		}
	// 		switch fieldNode := fieldNode.(type) {
	// 		case *ast.FieldNode:
	// 			for _, opt := range fieldNode.GetOptions().GetElements() {
	// 				collectOptions[*descriptorpb.FieldOptions](opt, field, optionsByNode)
	// 			}
	// 		case *ast.MapFieldNode:
	// 			for _, opt := range fieldNode.GetOptions().GetElements() {
	// 				collectOptions[*descriptorpb.FieldOptions](opt, field, optionsByNode)
	// 			}
	// 		}
	// 	}
	// }
	// for _, enum := range fdp.GetEnumType() {
	// 	for _, decl := range res.EnumNode(enum).(*ast.EnumNode).Decls {
	// 		info := a.NodeInfo(decl)
	// 		if info.End().Offset <= startOff {
	// 			continue
	// 		} else if info.Start().Offset >= endOff {
	// 			break
	// 		}
	// 		if opt, ok := decl.(*ast.OptionNode); ok {
	// 			collectOptions[*descriptorpb.EnumOptions](opt, enum, optionsByNode)
	// 		}
	// 	}
	// 	for _, val := range enum.GetValue() {
	// 		for _, opt := range res.EnumValueNode(val).(*ast.EnumValueNode).Options.GetElements() {
	// 			collectOptions[*descriptorpb.EnumValueOptions](opt, val, optionsByNode)
	// 		}
	// 	}
	// }
	// // for _, ext := range fdp.GetExtension() {
	// // 	for _, opt := range res.FieldNode(ext).(*ast.FieldNode).GetOptions().GetElements() {
	// // 		collectOptions[*descriptorpb.FieldOptions](opt, ext, optionsByNode)
	// // 	}
	// // }

	// allNodes := a.Children()
	// for _, node := range allNodes {
	// 	// only look at the decls that overlap the range
	// 	info := a.NodeInfo(node)
	// 	if info.End().Offset <= startOff {
	// 		continue
	// 	} else if info.Start().Offset >= endOff {
	// 		break
	// 	}
	// 	ast.Walk(node, &ast.SimpleVisitor{
	// 		DoVisitOptionNode: func(n *ast.OptionNode) error {
	// 			opts, ok := optionsByNode[n]
	// 			if !ok {
	// 				return nil
	// 			}
	// 			for _, opt := range opts {
	// 				msg := opt.TypeDescriptor().Message()
	// 				if msg != nil {
	// 					fullName := msg.FullName()

	// 					info := a.NodeInfo(n.Val)
	// 					hints = append(hints, protocol.InlayHint{
	// 						Position: protocol.Position{
	// 							Line:      uint32(info.Start().Line) - 1,
	// 							Character: uint32(info.Start().Col) - 1,
	// 						},
	// 						Label: []protocol.InlayHintLabelPart{
	// 							{
	// 								Value:   string(fullName),
	// 								Tooltip: makeTooltip(msg),
	// 							},
	// 						},
	// 						Kind:         protocol.Type,
	// 						PaddingLeft:  true,
	// 						PaddingRight: true,
	// 					})
	// 					if lit, ok := n.Val.(*ast.MessageLiteralNode); ok {
	// 						hints = append(hints, buildMessageLiteralHints(lit, msg, a)...)
	// 					}
	// 				}
	// 			}
	// 			return nil
	// 		},
	// 	})
	// }

	// return hints
}

func (c *Cache) computeImportHints(doc protocol.TextDocumentIdentifier, rng protocol.Range) []protocol.InlayHint {
	// show inlay hints for imports that resolve to different paths
	var hints []protocol.InlayHint
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()

	res, err := c.FindParseResultByURI(doc.URI.SpanURI())
	if err != nil {
		return nil
	}
	resAst := res.AST()
	if resAst == nil {
		return nil
	}
	var imports []*ast.ImportNode
	// get the source positions of the import statements
	for _, decl := range resAst.Decls {
		if imp, ok := decl.(*ast.ImportNode); ok {
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
		importPath := imp.Name.AsString()
		resolvedPath := dependencyPaths[i]
		nameInfo := resAst.NodeInfo(imp.Name)
		if resolvedPath != importPath {
			hints = append(hints, protocol.InlayHint{
				Kind:         protocol.Type,
				PaddingLeft:  true,
				PaddingRight: false,
				Position: protocol.Position{
					Line:      uint32(nameInfo.Start().Line) - 1,
					Character: uint32(nameInfo.End().Col) + 2,
				},
				TextEdits: []protocol.TextEdit{
					{
						Range:   adjustColumns(toRange(nameInfo), +1, -1),
						NewText: resolvedPath,
					},
				},
				Label: []protocol.InlayHintLabelPart{
					{
						Tooltip: &protocol.OrPTooltipPLabel{
							Value: fmt.Sprintf("Import resolves to %s", resolvedPath),
						},
						Value: resolvedPath,
					},
				},
			})
		}
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
		// 	slog.Debug("found import", "name", string(node.Name.AsString()))
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
	return c.resolver.SyntheticFileContents(span.URIFromURI(uri))
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
		switch currentNode.(type) {
		// short-circuit for some nodes that we know don't map to descriptors -
		// keywords and numbers
		case *ast.KeywordNode,
			*ast.SyntaxNode,
			*ast.PackageNode,
			*ast.EmptyDeclNode,
			*ast.RuneNode,
			*ast.UintLiteralNode,
			*ast.PositiveUintLiteralNode,
			*ast.NegativeIntLiteralNode,
			*ast.FloatLiteralNode,
			*ast.SpecialFloatLiteralNode,
			*ast.SignedFloatLiteralNode,
			*ast.StringLiteralNode, *ast.CompoundStringLiteralNode: // TODO: this could change in the future
			return nil, protocol.Range{}, nil
		}
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
				isMapEntry := nodeDescriptor.GetOptions().GetMapEntry()
				if isMapEntry {
					// if it is, we're looking for the value message, but only if the
					// location is within the value token and not the key token.
					// look ahead two path entries to check which token we're in
					if len(item.path) > i+2 {
						if mapTypeNode, ok := item.path[i+1].(*ast.MapTypeNode); ok {
							if identNode, ok := item.path[i+2].(ast.IdentValueNode); ok {
								if identNode == mapTypeNode.KeyType {
									return nil, protocol.Range{}, nil
								}
							}
						}
					}
					typeName = strings.TrimPrefix(nodeDescriptor.Field[1].GetTypeName(), ".")
				} else {
					typeName = nodeDescriptor.GetName()
				}
				// check if we're looking for a nested message
				prevIndex := i - 1
				if isMapEntry {
					prevIndex-- // go up one more level, we're inside a map field node
				}
				if prevIndex >= 0 {
					if _, ok := item.path[prevIndex].(*ast.MessageNode); ok {
						// the immediate parent is another message, so this message is not
						// a top-level descriptor. push it on the stack and go up one level
						stack.push(currentNode, nil)
						continue
					}
				}
				// search top-level messages
				desc = linkRes.Messages().ByName(protoreflect.Name(typeName))

				if desc == nil && isMapEntry {
					// the message we are looking for is somewhere nested within the
					// current top-level message, but is not a top-level message itself.
					stack.push(currentNode, nil)
					continue
				}
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
				desc = linkRes.Enums().ByName(protoreflect.Name(nodeDescriptor.GetName()))
			case *descriptorpb.ServiceDescriptorProto:
				desc = linkRes.Services().ByName(protoreflect.Name(nodeDescriptor.GetName()))
			case *descriptorpb.UninterpretedOption_NamePart:
				desc = linkRes.FindOptionNameFieldDescriptor(nodeDescriptor)
			case *descriptorpb.UninterpretedOption:
				field := linkRes.FindOptionFieldDescriptor(nodeDescriptor)
				if field != nil {
					switch field.Kind() {
					case protoreflect.MessageKind:
						desc = field.Message()
					case protoreflect.EnumKind:
						desc = field.Enum()
					default:
						return nil, protocol.Range{}, fmt.Errorf("unexpected option field kind %v", field.Kind())
					}
				}
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

	// slog.Debugf("descriptor: [%T] %v\n", haveDescriptor, haveDescriptor.FullName())

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
					wantName := wantNode.Name.AsString()
					imports := haveDesc.Imports()
					for i, l := 0, imports.Len(); i < l; i++ {
						imp := imports.Get(i)
						if imp.Path() == wantName {
							want.desc = imp
							break
						}
					}
				case *ast.MessageNode:
					want.desc = haveDesc.Messages().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.EnumNode:
					want.desc = haveDesc.Enums().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.ExtendNode:
					want.desc = haveDesc
				case *ast.ServiceNode:
					want.desc = haveDesc.Services().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				}
			case *ast.FieldNode:
				want.desc = haveDesc.Extensions().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
			case *ast.CompoundIdentNode:
				switch prevNode := want.prev.node.(type) {
				case *ast.ExtendNode:
					// looking for the extendee in the "extend <extendee> {" statement
					if wantNode.AsIdentifier() == prevNode.Extendee.AsIdentifier() {
						want.desc = linkRes.FindExtendeeDescriptorByName(protoreflect.FullName(wantNode.AsIdentifier()))
					}
				}
			case *ast.StringLiteralNode:
				if fd, ok := have.desc.(protoreflect.FileImport); ok {
					if fd.FileDescriptor == nil {
						// nothing to do
						return nil, protocol.Range{}, nil
					}
					want.desc = fd.FileDescriptor
				}
			}
		case protoreflect.MessageDescriptor:
			switch wantNode := want.node.(type) {
			case ast.MessageElement:
				switch wantNode := wantNode.(type) {
				case *ast.OptionNode:
					want.desc = haveDesc.Options().(*descriptorpb.MessageOptions).ProtoReflect().Descriptor()
				case *ast.FieldNode:
					if _, ok := have.node.(*ast.ExtendNode); ok {
						// (proto2 only) nested extension declaration
						want.desc = haveDesc.Extensions().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
					} else {
						want.desc = haveDesc.Fields().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
					}
				case *ast.MapFieldNode:
					want.desc = haveDesc.Fields().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.OneofNode:
					want.desc = haveDesc.Oneofs().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.GroupNode:
					want.desc = haveDesc.Fields().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.MessageNode:
					want.desc = haveDesc.Messages().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.EnumNode:
					want.desc = haveDesc.Enums().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.ExtendNode:
					// (proto2 only) looking for a nested extension declaration.
					// can't do anything yet, we need to resolve by field name
					want.desc = haveDesc
				case *ast.ExtensionRangeNode:
				case *ast.ReservedNode:
				}
			case *ast.FieldReferenceNode:
				if wantNode.IsAnyTypeReference() {
					want.desc = linkRes.FindMessageDescriptorByTypeReferenceURLNode(wantNode)
				} else {
					want.desc = haveDesc.Fields().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				}
			case *ast.MessageLiteralNode:
				want.desc = haveDesc
			case *ast.MessageFieldNode:
				name := wantNode.Name
				if name.IsAnyTypeReference() {
					want.desc = linkRes.FindMessageDescriptorByTypeReferenceURLNode(name)
				} else if name.IsExtension() {
					// formatted inside square brackets, e.g. {[path.to.extension.name]: value}
					fqn := linkRes.ResolveMessageLiteralExtensionName(wantNode.Name.Name)
					want.desc = linkRes.FindDescriptorByName(protoreflect.FullName(fqn[1:])).(protoreflect.ExtensionDescriptor)
				} else {
					want.desc = haveDesc.Fields().ByName(protoreflect.Name(wantNode.Name.Value()))
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
						switch haveDesc.Kind() {
						case protoreflect.MessageKind:
							want.desc = haveDesc.Message()
						case protoreflect.EnumKind:
							want.desc = haveDesc.Enum()
						}
					}
				}
			}
		case protoreflect.FieldDescriptor:
			switch wantNode := want.node.(type) {
			case ast.FieldDeclNode:
				switch wantNode := wantNode.(type) {
				case *ast.FieldNode:
					want.desc = haveDesc.Message().Fields().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.GroupNode:
					want.desc = haveDesc.Message().Fields().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.MapFieldNode:
					want.desc = haveDesc.Message().Fields().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.SyntheticMapField:
					want.desc = haveDesc.Message().Fields().ByName(protoreflect.Name(wantNode.Ident.AsIdentifier()))
				}
			case *ast.FieldReferenceNode:
				want.desc = haveDesc
			case *ast.MessageLiteralNode, *ast.ArrayLiteralNode:
				want.desc = haveDesc
			case *ast.MessageFieldNode:
				name := wantNode.Name
				if name.IsAnyTypeReference() {
					want.desc = linkRes.FindMessageDescriptorByTypeReferenceURLNode(name)
				} else {
					want.desc = haveDesc.Message().Fields().ByName(protoreflect.Name(wantNode.Name.Value()))
				}
			case *ast.MapTypeNode:
				// If we get here, we passed through a synthetic map type node, which
				// is directly mapped -- we just couldn't detect it earlier since it
				// isn't actually present at the location we're looking at.
				want.desc = haveDesc.MapValue().Message()
			case *ast.CompactOptionsNode:
				want.desc = haveDesc.Options().(*descriptorpb.FieldOptions).ProtoReflect().Descriptor()
			case ast.IdentValueNode:
				// need to disambiguate
				switch haveNode := have.node.(type) {
				case *ast.FieldReferenceNode:
					want.desc = haveDesc
				case *ast.MessageFieldNode:
					switch haveDesc.Kind() {
					case protoreflect.EnumKind:
						switch val := haveNode.Val.(type) {
						case ast.IdentValueNode:
							want.desc = haveDesc.Enum().Values().ByName(protoreflect.Name(val.AsIdentifier()))
						}
					}
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
					want.desc = haveDesc.Values().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
				case *ast.ReservedNode:
				}
				// default:
				// if enumNode, ok := parentNode.(*ast.EnumValueNode); ok {
				// 	if want == enumNode.Name {
				// 		want.resolved = have.Values().ByName(protoreflect.Name(enumNode.Name.AsIdentifier()))
				// 	}
				// }
			case ast.IdentValueNode:
				want.desc = haveDesc.Values().ByName(protoreflect.Name(wantNode.AsIdentifier()))
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
					want.desc = haveDesc.Methods().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
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
					want.desc = haveDesc.Fields().ByName(protoreflect.Name(wantNode.Name.AsIdentifier()))
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
		node = containingFileResolver.MessageNode(desc.(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.DescriptorProto)).MessageName()
	case protoreflect.EnumDescriptor:
		node = containingFileResolver.EnumNode(desc.(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.EnumDescriptorProto)).GetName()
	case protoreflect.ServiceDescriptor:
		node = containingFileResolver.ServiceNode(desc.(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.ServiceDescriptorProto)).GetName()
	case protoreflect.MethodDescriptor:
		node = containingFileResolver.MethodNode(desc.(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.MethodDescriptorProto)).GetName()
	case protoreflect.FieldDescriptor:
		if !desc.IsExtension() {
			switch desc.(type) {
			case protoutil.DescriptorProtoWrapper:
				node = containingFileResolver.FieldNode(desc.(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.FieldDescriptorProto))
			default:
				// these can be internal filedesc.Field descriptors for e.g. builtin file options
				containingFileResolver.RangeFieldReferenceNodesWithDescriptors(func(n ast.Node, fd protoreflect.FieldDescriptor) bool {
					// TODO: this is a workaround, figure out why the linker wrapper types aren't being used here
					if desc.FullName() == fd.FullName() {
						node = n
						return false
					}
					return true
				})
			}
		} else {
			switch desc := desc.(type) {
			case protoreflect.ExtensionTypeDescriptor:
				node = containingFileResolver.FieldNode(desc.Descriptor().(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.FieldDescriptorProto))
			case protoutil.DescriptorProtoWrapper:
				node = containingFileResolver.FieldNode(desc.AsProto().(*descriptorpb.FieldDescriptorProto))
			}
		}
	case protoreflect.EnumValueDescriptor:
		node = containingFileResolver.EnumValueNode(desc.(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.EnumValueDescriptorProto)).GetName()
	case protoreflect.OneofDescriptor:
		node = containingFileResolver.OneofNode(desc.(protoutil.DescriptorProtoWrapper).AsProto().(*descriptorpb.OneofDescriptorProto))
	case protoreflect.FileDescriptor:
		node = containingFileResolver.FileNode()
		slog.Debug("definition is an import: ", "import", containingFileResolver.Path())
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
	if desc == nil {
		return nil, nil
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

func makeTooltip(d protoreflect.Descriptor) *protocol.OrPTooltipPLabel {
	str, err := format.PrintDescriptor(d)
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

// Checks if the most recently parsed version of the given document has any
// syntax errors, as reported by the diagnostic handler.
func (c *Cache) LatestDocumentContentsWellFormed(uri span.URI) (bool, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.latestDocumentContentsWellFormedLocked(uri)
}

func (c *Cache) latestDocumentContentsWellFormedLocked(uri span.URI) (bool, error) {
	path, err := c.resolver.URIToPath(uri)
	if err != nil {
		return false, err
	}
	diagnostics, _, _ := c.diagHandler.GetDiagnosticsForPath(path)
	for _, diag := range diagnostics {
		var parseErr parser.ParseError
		if errors.As(diag.Error, &parseErr) {
			return false, nil
		}
	}
	return true, nil
}

func (c *Cache) FormatDocument(doc protocol.TextDocumentIdentifier, options protocol.FormattingOptions, maybeRange ...protocol.Range) ([]protocol.TextEdit, error) {
	// check if the file has any parse errors; if it does, don't try to format
	// the document as we will end up erasing anything the user has typed
	// since the last time the document was successfully parsed.
	if ok, err := c.LatestDocumentContentsWellFormed(doc.URI.SpanURI()); err != nil {
		return nil, err
	} else if !ok {
		return nil, nil
	}
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
	format := format.NewFormatter(buf, res.AST())
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

func (c *Cache) AllMessages() []protoreflect.MessageDescriptor {
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

package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	gsync "github.com/kralicky/gpkg/sync"
	"github.com/kralicky/protocompile"
	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protocompile/parser"
	"github.com/kralicky/protocompile/reporter"
	"github.com/kralicky/protols/pkg/format"
	"github.com/kralicky/tools-lite/gopls/pkg/file"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/cache"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
	"github.com/kralicky/tools-lite/pkg/diff"
	"github.com/kralicky/tools-lite/pkg/jsonrpc2"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
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

	// partialResultsMu has an invariant that resultsMu is write-locked; it expects
	// to be required only during compilation. This means that if resultsMu is
	// held (for reading or writing), partialResultsMu does not need to be held.
	partialResultsMu       sync.Mutex
	unlinkedResults        map[protocompile.ResolvedPath]parser.Result
	partiallyLinkedResults map[protocompile.ResolvedPath]linker.Result

	inflightTasksInvalidate gsync.Map[protocompile.ResolvedPath, time.Time]
	inflightTasksCompile    gsync.Map[protocompile.ResolvedPath, time.Time]
	pragmas                 gsync.Map[protocompile.ResolvedPath, *pragmaMap]

	documentVersions *documentVersionQueue
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

// FindExtensionByNumber implements linker.Resolver.
func (c *Cache) FindExtensionsByMessage(message protoreflect.FullName) []protoreflect.ExtensionDescriptor {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	var extensions []protoreflect.ExtensionDescriptor
	for _, res := range c.results {
		extensions = append(extensions, res.(linker.Result).FindExtensionsByMessage(message)...)
	}
	return extensions
}

func (c *Cache) FindResultByPath(path string) (linker.Result, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.findResultByPathLocked(path)
}

func (c *Cache) FindResultByURI(uri protocol.DocumentURI) (linker.Result, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	path, err := c.resolver.URIToPath(uri)
	if err != nil {
		return nil, err
	}
	return c.findResultByPathLocked(path)
}

func (c *Cache) FindResultOrPartialResultByPath(path string) (linker.Result, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.findResultOrPartialResultByPathLocked(path)
}

func (c *Cache) FindResultOrPartialResultByURI(uri protocol.DocumentURI) (linker.Result, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	path, err := c.resolver.URIToPath(uri)
	if err != nil {
		return nil, err
	}
	return c.findResultOrPartialResultByPathLocked(path)
}

func (c *Cache) FindParseResultByPath(path string) (parser.Result, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.findParseResultByPathLocked(path)
}

func (c *Cache) FindParseResultByURI(uri protocol.DocumentURI) (parser.Result, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	path, err := c.resolver.URIToPath(uri)
	if err != nil {
		return nil, err
	}
	return c.findParseResultByPathLocked(path)
}

func (c *Cache) findResultByPathLocked(path string) (linker.Result, error) {
	if c.results == nil {
		return nil, fmt.Errorf("no results exist")
	}
	f := c.results.FindFileByPath(path)
	if f == nil {
		return nil, fmt.Errorf("FindResultByPath: package not found: %q", path)
	}
	return f.(linker.Result), nil
}

func (c *Cache) findResultOrPartialResultByPathLocked(path string) (linker.Result, error) {
	if pr, ok := c.partiallyLinkedResults[protocompile.ResolvedPath(path)]; ok {
		return pr, nil
	}
	return c.findResultByPathLocked(path)
}

func (c *Cache) findParseResultByPathLocked(path string) (parser.Result, error) {
	if pr, ok := c.unlinkedResults[protocompile.ResolvedPath(path)]; ok {
		return pr, nil
	}
	return c.findResultOrPartialResultByPathLocked(path)
}

func (c *Cache) FindFileByURI(uri protocol.DocumentURI) (protoreflect.FileDescriptor, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	path, err := c.resolver.URIToPath(uri)
	if err != nil {
		return nil, err
	}
	return c.results.AsResolver().FindFileByPath(path)
}

func (c *Cache) TracksURI(uri protocol.DocumentURI) bool {
	_, err := c.resolver.URIToPath(uri)
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
	diagHandler := NewDiagnosticHandler()
	reporter := reporter.NewReporter(diagHandler.HandleError, diagHandler.HandleWarning)
	resolver := NewResolver(workspace)
	resolver.PreloadWellKnownPaths()

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
		workdir: protocol.DocumentURI(workspace.URI).Path(),
	}
	cache := &Cache{
		workspace:              workspace,
		compiler:               compiler,
		resolver:               resolver,
		diagHandler:            diagHandler,
		unlinkedResults:        make(map[protocompile.ResolvedPath]parser.Result),
		partiallyLinkedResults: make(map[protocompile.ResolvedPath]linker.Result),
		documentVersions:       newDocumentVersionQueue(),
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

	created := make([]file.Modification, len(files))
	for i, f := range files {
		created[i] = file.Modification{
			Action: file.Create,
			OnDisk: true,
			URI:    protocol.URIFromPath(f),
		}
	}

	c.DidModifyFiles(context.TODO(), created)
}

func (r *Cache) GetMapper(uri protocol.DocumentURI) (*protocol.Mapper, error) {
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

func (s *Cache) ChangedText(ctx context.Context, uri protocol.VersionedTextDocumentIdentifier, changes []protocol.TextDocumentContentChangeEvent) ([]byte, error) {
	if len(changes) == 0 {
		return nil, fmt.Errorf("%w: no content changes provided", jsonrpc2.ErrInternal)
	}

	// Check if the client sent the full content of the file.
	// We accept a full content change even if the server expected incremental changes.
	if len(changes) == 1 && changes[0].Range == nil && changes[0].RangeLength == 0 {
		return []byte(changes[0].Text), nil
	}

	m, err := s.GetMapper(uri.URI)
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

	return protocol.EditsToDiffEdits(mapper, edits)
}

func (c *Cache) preInvalidateHook(path protocompile.ResolvedPath, reason string) {
	slog.Debug("invalidating file", "path", path, "reason", reason)
	c.inflightTasksInvalidate.Store(path, time.Now())
	c.diagHandler.ClearDiagnosticsForPath(string(path))
}

func (c *Cache) postInvalidateHook(path protocompile.ResolvedPath, prevResult linker.File, willRecompile bool) {
	startTime, _ := c.inflightTasksInvalidate.LoadAndDelete(path)
	slog.Debug("file invalidated", "path", path, "took", time.Since(startTime))
	if !willRecompile {
		slog.Debug("file deleted, clearing linker result", "path", path)
		for i, f := range c.results {
			if protocompile.ResolvedPath(f.Path()) == path {
				c.results = append(c.results[:i], c.results[i+1:]...)
				break
			}
		}
	}
}

func (c *Cache) preCompile(path protocompile.ResolvedPath) {
	slog.Debug(fmt.Sprintf("compiling %s\n", path))
	c.inflightTasksCompile.Store(path, time.Now())
	c.partialResultsMu.Lock()
	defer c.partialResultsMu.Unlock()
	delete(c.partiallyLinkedResults, path)
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

func (c *Cache) Compile(protos []string, after ...func()) {
	c.resultsMu.Lock()
	defer c.resultsMu.Unlock()
	for _, f := range after {
		defer f()
	}
	c.compileLocked(protos...)
}

func (c *Cache) compileLocked(protos ...string) {
	slog.Debug("compiling", "protos", len(protos))

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
	slog.Debug("done compiling", "protos", len(protos))
	for _, r := range res.Files {
		path := r.Path()
		found := false
		var pragmas map[string]string
		if resAst := r.(linker.Result).AST(); resAst != nil {
			pragmas = resAst.Pragmas
		}

		for i, f := range c.results {
			// todo: this is big slow
			if f.Path() == path {
				found = true
				slog.With("path", path).Debug("updating existing linker result")
				c.results[i] = r
				if p, ok := c.pragmas.Load(protocompile.ResolvedPath(path)); ok {
					p.update(pragmas)
				}
				break
			}
		}
		if !found {
			slog.With("path", path).Debug("adding new linker result")
			c.results = append(c.results, r)
			c.pragmas.Store(protocompile.ResolvedPath(path), &pragmaMap{m: pragmas})
		}
	}
	c.partialResultsMu.Lock()
	for path, partial := range res.PartialLinkResults {
		partial := partial
		slog.With("path", path).Debug("adding new partial linker result")
		c.partiallyLinkedResults[path] = partial
	}
	for path, partial := range res.UnlinkedParserResults {
		partial := partial
		slog.With("path", path).Debug("adding new partial linker result")
		c.unlinkedResults[path] = partial
	}
	c.partialResultsMu.Unlock()

	syntheticFiles := c.resolver.CheckIncompleteDescriptors(c.results)
	if len(syntheticFiles) == 0 {
		return
	}
	slog.Debug("building new synthetic sources", "sources", len(syntheticFiles))
	c.compileLocked(syntheticFiles...)
}

func (c *Cache) DidModifyFiles(ctx context.Context, modifications []file.Modification) {
	c.resolver.UpdateURIPathMappings(modifications)

	var toRecompile []string
	for _, m := range modifications {
		path, err := c.resolver.URIToPath(m.URI)
		if err != nil {
			slog.With(
				"error", err,
				"uri", m.URI.Path(),
			).Error("failed to resolve uri to path")
			continue
		}
		switch m.Action {
		case file.Open:
		case file.Close:
		case file.Save:
			toRecompile = append(toRecompile, path)
		case file.Change:
			toRecompile = append(toRecompile, path)
		case file.Create:
			toRecompile = append(toRecompile, path)
		case file.Delete:
			toRecompile = append(toRecompile, path)
		}
	}
	if err := c.compiler.fs.UpdateOverlays(ctx, modifications); err != nil {
		panic(fmt.Errorf("internal protocol error: %w", err))
	}
	if len(toRecompile) > 0 {
		c.Compile(toRecompile, func() {
			c.documentVersions.Update(modifications...)
		})
	}
}

func (c *Cache) ComputeSemanticTokens(doc protocol.TextDocumentIdentifier) ([]uint32, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	if ok, err := c.latestDocumentContentsWellFormedLocked(doc.URI, false); err != nil {
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
	if ok, err := c.latestDocumentContentsWellFormedLocked(doc.URI, false); err != nil {
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

func (c *Cache) ComputeDiagnosticReports(uri protocol.DocumentURI, prevResultId string) ([]protocol.Diagnostic, protocol.DocumentDiagnosticReportKind, string, error) {
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
				rawReport.RelatedInformation[i].Location.URI = u
			}
		}
		if rawReport.Severity == protocol.SeverityWarning && rawReport.WerrorCategory != "" {
			// look up Werror debug pragma for this file
			shouldElevate := true
			if p, ok := c.FindPragmasByPath(protocompile.ResolvedPath(rawReport.Pos.Start().Filename)); ok {
				if dbg, ok := p.Lookup(PragmaDebug); ok {
					for _, v := range strings.Fields(dbg) {
						if k, v, ok := strings.Cut(v, "="); ok {
							if k == PragmaDebugWnoerror && (v == rawReport.WerrorCategory || v == WnoerrorAll) {
								shouldElevate = false
								break
							}
						}
					}
				}
			}
			if shouldElevate {
				rawReport.Severity = protocol.SeverityError
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
		if rawReport.WerrorCategory != "" {
			report.Code = rawReport.WerrorCategory
		}
		data := DiagnosticData{
			Metadata:    rawReport.Metadata,
			CodeActions: rawReport.CodeActions,
		}
		jsonData, err := json.Marshal(data)
		if err != nil {
			panic(err)
		}
		rawMsg := json.RawMessage(jsonData)
		report.Data = &rawMsg
		reports = append(reports, report)
	}
	return reports
}

func (c *Cache) toProtocolCodeActions(rawCodeActions []CodeAction, associatedDiagnostic *protocol.Diagnostic) []protocol.CodeAction {
	if len(rawCodeActions) == 0 {
		return []protocol.CodeAction{}
	}
	var codeActions []protocol.CodeAction
	for _, rawCodeAction := range rawCodeActions {
		uri, err := c.resolver.PathToURI(string(rawCodeAction.Path))
		if err != nil {
			slog.With(
				"error", err,
				"path", string(rawCodeAction.Path),
			).Error("failed to resolve path to uri")
			continue
		}
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

type workspaceDiagnosticCallbackFunc = func(uri protocol.DocumentURI, reports []protocol.Diagnostic, kind protocol.DocumentDiagnosticReportKind, resultId string)

func (c *Cache) StreamWorkspaceDiagnostics(ctx context.Context, ch chan<- protocol.WorkspaceDiagnosticReportPartialResult) {
	currentDiagnostics := make(map[protocol.DocumentURI][]protocol.Diagnostic)
	diagnosticVersions := make(map[protocol.DocumentURI]int32)
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
							URI:     uri,
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
							URI:     uri,
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
	if ok, err := c.latestDocumentContentsWellFormedLocked(doc.URI, true); err != nil {
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
	res, err := c.FindResultByURI(doc.URI)
	if err != nil {
		return nil
	}
	mapper, err := c.GetMapper(doc.URI)
	if err != nil {
		return nil
	}
	a := res.AST()
	startOff, endOff, _ := mapper.RangeOffsets(rng)
	startToken := a.ItemAtOffset(startOff)
	endToken := a.ItemAtOffset(endOff)
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
				if l, err := c.FindDefinitionForTypeDescriptor(desc.Message()); err == nil {
					location = &l
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
}

func (c *Cache) computeImportHints(doc protocol.TextDocumentIdentifier, rng protocol.Range) []protocol.InlayHint {
	// show inlay hints for imports that resolve to different paths
	var hints []protocol.InlayHint
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()

	res, err := c.FindParseResultByURI(doc.URI)
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

func (c *Cache) GetSyntheticFileContents(ctx context.Context, uri protocol.DocumentURI) (string, error) {
	return c.resolver.SyntheticFileContents(uri)
}

func (c *Cache) FindTypeDescriptorAtLocation(params protocol.TextDocumentPositionParams) (protoreflect.Descriptor, protocol.Range, error) {
	parseRes, err := c.FindParseResultByURI(params.TextDocument.URI)
	if err != nil {
		return nil, protocol.Range{}, err
	}
	linkRes, err := c.FindResultOrPartialResultByURI(params.TextDocument.URI)
	if err != nil {
		return nil, protocol.Range{}, err
	}

	mapper, err := c.GetMapper(params.TextDocument.URI)
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
	root := enc.AST()

	token := root.ItemAtOffset(offset)
	computeSemanticTokens(c, &enc, ast.WithIntersection(token))

	item, found := findNarrowestSemanticToken(parseRes, enc.items, params.Position)
	if !found {
		return nil, protocol.Range{}, nil
	}

	return deepPathSearch(item.path, parseRes, linkRes)
}

func (c *Cache) FindDefinitionForTypeDescriptor(desc protoreflect.Descriptor) (protocol.Location, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	ref, err := findDefinition(desc, c.results)
	if err != nil {
		return protocol.Location{}, err
	}
	uri, err := c.resolver.PathToURI(ref.NodeInfo.Start().Filename)
	if err != nil {
		return protocol.Location{}, err
	}
	return protocol.Location{
		URI:   uri,
		Range: toRange(ref.NodeInfo),
	}, nil
}

func (c *Cache) FindReferenceLocationsForTypeDescriptor(desc protoreflect.Descriptor) ([]protocol.Location, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	var locations []protocol.Location
	for span := range findNodeReferences(desc, c.results) {
		filename := span.NodeInfo.Start().Filename
		uri, err := c.resolver.PathToURI(filename)
		if err != nil {
			continue
		}
		locations = append(locations, protocol.Location{
			URI:   uri,
			Range: toRange(span.NodeInfo),
		})
	}
	return locations, nil
}

func (c *Cache) FindReferencesForTypeDescriptor(desc protoreflect.Descriptor) ([]ast.NodeReference, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	var refs []ast.NodeReference
	for node := range findNodeReferences(desc, c.results) {
		refs = append(refs, node)
	}
	return refs, nil
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
		if l, err := c.FindDefinitionForTypeDescriptor(desc); err == nil {
			locations = append(locations, l)
		} else {
			return nil, err
		}
	}

	refs, err := c.FindReferenceLocationsForTypeDescriptor(desc)
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
func (c *Cache) LatestDocumentContentsWellFormed(uri protocol.DocumentURI, strict bool) (bool, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.latestDocumentContentsWellFormedLocked(uri, strict)
}

func (c *Cache) latestDocumentContentsWellFormedLocked(uri protocol.DocumentURI, strict bool) (bool, error) {
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
		if strict {
			var ext parser.ExtendedSyntaxError
			if errors.As(diag.Error, &ext) && !ext.CanFormat() {
				return false, nil
			}
		}
	}
	return true, nil
}

func (c *Cache) FormatDocument(doc protocol.TextDocumentIdentifier, options protocol.FormattingOptions, maybeRange ...protocol.Range) ([]protocol.TextEdit, error) {
	// check if the file has any parse errors; if it does, don't try to format
	// the document as we will end up erasing anything the user has typed
	// since the last time the document was successfully parsed.
	if ok, err := c.LatestDocumentContentsWellFormed(doc.URI, true); err != nil {
		return nil, err
	} else if !ok {
		return nil, nil
	}
	mapper, err := c.GetMapper(doc.URI)
	if err != nil {
		return nil, err
	}
	res, err := c.FindParseResultByURI(doc.URI)
	if err != nil {
		return nil, err
	}
	resAst := res.AST()
	if resAst == nil {
		return nil, nil
	}
	if _, ok := resAst.Pragma(PragmaNoFormat); ok {
		return nil, nil
	}
	// format whole file
	buf := bytes.NewBuffer(make([]byte, 0, len(mapper.Content)))
	format := format.NewFormatter(buf, res.AST())
	if err := format.Run(); err != nil {
		return nil, err
	}

	edits := diff.Bytes(mapper.Content, buf.Bytes())
	return protocol.EditsFromDiffEdits(mapper, edits)
}

func (c *Cache) FindAllDescriptorsByPrefix(ctx context.Context, prefix string, filter ...func(protoreflect.Descriptor) bool) []protoreflect.Descriptor {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	eg, ctx := errgroup.WithContext(ctx)
	resultsByPackage := make([][]protoreflect.Descriptor, len(c.results))
	for i, res := range c.results {
		i, res := i, res
		pkg := res.Package()
		if pkg == "" {
			// skip searching in files that do not have a package name
			continue
		}
		eg.Go(func() (err error) {
			resultsByPackage[i], err = res.(linker.Result).FindDescriptorsByPrefix(ctx, string(pkg.Append(protoreflect.Name(prefix))), filter...)
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

// Like FindAllDescriptorsByPrefix, but assumes a fully qualified prefix with
// package name.
func (c *Cache) FindAllDescriptorsByQualifiedPrefix(ctx context.Context, prefix string, filter ...func(protoreflect.Descriptor) bool) []protoreflect.Descriptor {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	eg, ctx := errgroup.WithContext(ctx)
	resultsByPackage := make([][]protoreflect.Descriptor, len(c.results))
	isFullyQualified := strings.HasPrefix(prefix, ".")
	if isFullyQualified {
		prefix = prefix[1:]
	}
	for i, res := range c.results {
		i, res := i, res
		pkg := res.Package()
		if pkg == "" {
			// skip searching in files that do not have a package name
			continue
		}
		if isFullyQualified && !strings.HasPrefix(string(pkg), prefix) {
			// optimization: skip results whose package names don't match the prefix
			continue
		}
		eg.Go(func() (err error) {
			resultsByPackage[i], err = res.(linker.Result).FindDescriptorsByPrefix(ctx, prefix, filter...)
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

func (c *Cache) FindImportPathsByPrefix(ctx context.Context, prefix string) map[protocol.DocumentURI]string {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.resolver.findImportPathsByPrefix(prefix)
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

func (c *Cache) FindPragmasByPath(path protocompile.ResolvedPath) (Pragmas, bool) {
	p, ok := c.pragmas.Load(path)
	return p, ok
}

func (c *Cache) WaitDocumentVersion(ctx context.Context, uri protocol.DocumentURI, version int32) error {
	ctx, ca := context.WithTimeout(ctx, 2*time.Second)
	defer ca()
	return c.documentVersions.Wait(ctx, uri, version)
}

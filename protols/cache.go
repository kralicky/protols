package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
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
	"github.com/bufbuild/protocompile/walk"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/desc/protoprint"
	gsync "github.com/kralicky/gpkg/sync"
	"github.com/kralicky/ragu"
	"go.uber.org/zap"
	"golang.org/x/exp/maps"
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
	lg             *zap.Logger
	compiler       *Compiler
	diagHandler    *DiagnosticHandler
	resultsMu      sync.RWMutex
	results        linker.Files
	partialResults map[string]parser.Result
	indexMu        sync.RWMutex
	// indexedDirsByGoPkg map[string]string   // go package name -> directory
	// indexedGoPkgsByDir map[string]string   // directory -> go package name
	filePathsByURI map[span.URI]string // URI -> canonical file path (go package + file name)
	fileURIsByPath map[string]span.URI // canonical file path (go package + file name) -> URI

	todoModLock sync.Mutex

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
		fmt.Printf("%v\n", c.filePathsByURI)
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
	path, ok := c.filePathsByURI[uri]
	if !ok {
		return nil, fmt.Errorf("no file found for URI %q", uri)
	}
	f := c.results.FindFileByPath(path)
	if f == nil {
		fmt.Printf("%v\n", c.filePathsByURI)
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
	path, ok := c.filePathsByURI[uri]
	if !ok {
		return nil, fmt.Errorf("no file found for URI %q", uri)
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
	path, ok := c.filePathsByURI[uri]
	if !ok {
		return nil, fmt.Errorf("no file found for URI %q", uri)
	}
	return c.results.AsResolver().FindFileByPath(path)
}

func (c *Cache) PathToURI(path string) (span.URI, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	uri, ok := c.fileURIsByPath[path]
	if !ok {
		return "", fmt.Errorf("no file found for path %q", path)
	}
	return uri, nil
}
func (c *Cache) URIToPath(uri span.URI) (string, error) {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	path, ok := c.filePathsByURI[uri]
	if !ok {
		return "", fmt.Errorf("no file found for URI %q", uri)
	}
	return path, nil
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
	workdir string
	overlay *Overlay
}

type Overlay struct {
	baseAccessor func(path string) (io.ReadCloser, error)
	sourcesMu    sync.Mutex
	sources      map[string]*protocol.Mapper
}

func (o *Overlay) Create(uri span.URI, path string, contents []byte) error {
	o.sourcesMu.Lock()
	defer o.sourcesMu.Unlock()
	if _, ok := o.sources[path]; ok {
		return fmt.Errorf("%w: file already exists", jsonrpc2.ErrInternal)
	}
	o.sources[path] = protocol.NewMapper(uri, contents)
	return nil
}

// requires sourcesMu to be locked (todo: fix)
func (o *Overlay) Update(uri span.URI, path string, contentChanges []protocol.TextDocumentContentChangeEvent) error {
	if len(contentChanges) == 0 {
		return fmt.Errorf("%w: no content changes provided", jsonrpc2.ErrInternal)
	}

	if _, ok := o.sources[path]; !ok {
		baseReader, err := o.baseAccessor(path)
		if err != nil {
			return err
		}
		defer baseReader.Close()
		baseContent, _ := io.ReadAll(baseReader)
		o.sources[path] = protocol.NewMapper(uri, baseContent)
	}
	source := o.sources[path]
	newSrc, err := applyChanges(source, contentChanges)
	if err != nil {
		return err
	}

	o.sources[path] = protocol.NewMapper(uri, newSrc)
	return nil
}

// requires sourcesMu to be held
func applyChanges(m *protocol.Mapper, changes []protocol.TextDocumentContentChangeEvent) ([]byte, error) {
	if len(changes) == 0 {
		return nil, fmt.Errorf("%w: no content changes provided", jsonrpc2.ErrInternal)
	}

	// Check if the client sent the full content of the file.
	// We accept a full content change even if the server expected incremental changes.
	if len(changes) == 1 && changes[0].Range == nil && changes[0].RangeLength == 0 {
		return []byte(changes[0].Text), nil
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

func (o *Overlay) Delete(path string) {
	o.sourcesMu.Lock()
	defer o.sourcesMu.Unlock()
	delete(o.sources, path)
}

func (o *Overlay) Accessor(path string) (io.ReadCloser, error) {
	o.sourcesMu.Lock()
	defer o.sourcesMu.Unlock()
	if source, ok := o.sources[path]; ok {
		return io.NopCloser(bytes.NewReader(source.Content)), nil
	}
	return nil, os.ErrNotExist
}
func (o *Overlay) Get(path string) (*protocol.Mapper, error) {
	o.sourcesMu.Lock()
	defer o.sourcesMu.Unlock()
	if source, ok := o.sources[path]; ok {
		return source, nil
	}
	return nil, os.ErrNotExist
}

var requiredGoEnvVars = []string{"GO111MODULE", "GOFLAGS", "GOINSECURE", "GOMOD", "GOMODCACHE", "GONOPROXY", "GONOSUMDB", "GOPATH", "GOPROXY", "GOROOT", "GOSUMDB", "GOWORK"}

func NewCache(workdir string, lg *zap.Logger) *Cache {
	synthesizer := NewProtoSourceSynthesizer(workdir)
	fmt.Println("SourceAccessor", workdir)
	memoizedFs := cache.NewMemoizedFS()

	tryReadFromFs := func(importName string) (_ io.ReadCloser, _err error) {
		fmt.Println("tryReadFromOverlay", importName)
		defer func() { fmt.Println("tryReadFromOverlay done", importName, _err) }()
		fh, err := memoizedFs.ReadFile(context.TODO(), span.URIFromPath(importName))
		if err == nil {
			content, err := fh.Content()
			if err != nil {
				return nil, err
			}
			if content != nil {
				return io.NopCloser(bytes.NewReader(content)), nil
			}
		}
		return nil, err
	}
	resolverFn := protocompile.ResolverFunc(func(importName string) (result protocompile.SearchResult, _ error) {
		if strings.HasPrefix(importName, "google/") {
			return protocompile.SearchResult{}, os.ErrNotExist
		}
		if strings.HasPrefix(importName, "gogoproto/") {
			importName = "github.com/gogo/protobuf/" + importName
		}

		rc, err := tryReadFromFs(importName)
		if err == nil {
			result.Source = rc
			return
		}
		f, dir, err := synthesizer.ImportFromGoModule(importName)
		if err == nil {
			result.Source, _ = os.Open(f)
			return
		}
		if dir != "" {
			if synthesized, err := synthesizer.SynthesizeFromGoSource(importName, dir); err == nil {
				result.Proto = synthesized
				return
			} else {
				return protocompile.SearchResult{}, fmt.Errorf("failed to synthesize %s: %w", importName, err)
			}
		}

		return protocompile.SearchResult{}, fmt.Errorf("failed to resolve %s: %w", importName, err)
	})

	// NewCache creates a new cache.
	diagHandler := NewDiagnosticHandler()
	reporter := reporter.NewReporter(diagHandler.HandleError, diagHandler.HandleWarning)
	accessor := tryReadFromFs
	overlay := &Overlay{
		baseAccessor: accessor,
		sources:      map[string]*protocol.Mapper{},
	}
	resolver := protocompile.CompositeResolver{
		&protocompile.SourceResolver{
			Accessor: overlay.Accessor,
		},
		resolverFn,
		protocompile.ResolverFunc(func(path string) (protocompile.SearchResult, error) {
			fd, err := desc.LoadFileDescriptor(path)
			if err != nil {
				return protocompile.SearchResult{}, err
			}
			return protocompile.SearchResult{Desc: fd.UnwrapFile()}, nil
		}),
	}

	compiler := &Compiler{
		Compiler: &protocompile.Compiler{
			Resolver:       resolver,
			MaxParallelism: runtime.NumCPU() * 4,
			Reporter:       reporter,
			SourceInfoMode: protocompile.SourceInfoExtraComments | protocompile.SourceInfoExtraOptionLocations,
			RetainResults:  true,
			RetainASTs:     true,
		},
		workdir: workdir,
		overlay: overlay,
	}
	cache := &Cache{
		lg:          lg,
		compiler:    compiler,
		diagHandler: diagHandler,
		// indexedDirsByGoPkg: map[string]string{},
		// indexedGoPkgsByDir: map[string]string{},
		filePathsByURI: map[span.URI]string{},
		fileURIsByPath: map[string]span.URI{},
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
	delete(c.partialResults, path)
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

	c.indexMu.Lock()
	// maps.Clear(c.indexedDirsByGoPkg)
	// maps.Clear(c.indexedGoPkgsByDir)
	maps.Clear(c.filePathsByURI)
	maps.Clear(c.partialResults)
	c.indexMu.Unlock()

	allProtos, _ := doublestar.Glob(path.Join(c.compiler.workdir, "**/*.proto"))
	c.lg.Debug("found protos", zap.Strings("protos", allProtos))
	created := make([]protocol.FileCreate, len(allProtos))
	for i, proto := range allProtos {
		created[i] = protocol.FileCreate{
			URI: string(span.URIFromPath(proto)),
		}
	}

	if err := c.OnFilesCreated(created); err != nil {
		c.lg.Error("failed to index files", zap.Error(err))
	}
}

func (c *Cache) Compile(protos ...string) {
	c.resultsMu.Lock()
	defer c.resultsMu.Unlock()
	c.lg.Info("compiling", zap.Int("protos", len(protos)))
	res, err := c.compiler.Compile(context.TODO(), protos...)
	if err != nil {
		if !errors.Is(err, reporter.ErrInvalidSource) {
			c.lg.With(zap.Error(err)).Error("failed to compile")
			return
		}
	}
	c.lg.Info("done compiling", zap.Int("protos", len(protos)))
	for _, r := range res.Files {
		path := r.Path()
		found := false
		// delete(c.partialResults, path)
		for i, f := range c.results {
			// todo: this is big slow
			if f.Path() == path {
				found = true
				c.results[i] = r
				break
			}
		}
		if !found {
			c.results = append(c.results, r)
		}
	}
	for path, partial := range res.UnlinkedParserResults {
		partial := partial
		c.partialResults[path] = partial
	}
	c.lg.Info("reindexed", zap.Int("protos", len(protos)))
}

func (s *Cache) OnFileOpened(doc protocol.TextDocumentItem) {
	s.lg.With(
		zap.String("file", string(doc.URI)),
		zap.String("path", s.filePathsByURI[doc.URI.SpanURI()]),
	).Debug("file opened")
	s.compiler.overlay.Create(doc.URI.SpanURI(), s.filePathsByURI[doc.URI.SpanURI()], []byte(doc.Text))
}

func (s *Cache) OnFileClosed(doc protocol.TextDocumentIdentifier) {
	s.lg.With(
		zap.String("file", string(doc.URI)),
	).Debug("file closed")
	s.compiler.overlay.Delete(s.filePathsByURI[doc.URI.SpanURI()])
}

func (s *Cache) OnFileModified(f protocol.VersionedTextDocumentIdentifier, contentChanges []protocol.TextDocumentContentChangeEvent) error {
	s.todoModLock.Lock()
	defer s.todoModLock.Unlock()
	s.lg.With(
		zap.String("file", string(f.URI)),
	).Debug("file modified")

	if err := s.compiler.overlay.Update(f.URI.SpanURI(), s.filePathsByURI[f.URI.SpanURI()], contentChanges); err != nil {
		return err
	}
	s.Compile(s.filePathsByURI[f.URI.SpanURI()])
	return nil
}

func (c *Cache) OnFilesDeleted(f []protocol.FileDelete) error {
	c.indexMu.Lock()
	defer c.indexMu.Unlock()
	// remove from cache
	paths := make([]string, len(f))
	for i, file := range f {
		paths[i] = c.filePathsByURI[span.URIFromURI(file.URI)]
		c.compiler.overlay.Delete(paths[i])
	}
	c.lg.With(
		zap.Strings("files", paths),
	).Debug("files deleted")
	c.Compile(paths...)

	for _, path := range paths {
		uri := c.fileURIsByPath[path]
		delete(c.filePathsByURI, uri)
		delete(c.fileURIsByPath, path)
	}
	return nil
}

func (c *Cache) OnFilesCreated(files []protocol.FileCreate) error {
	c.indexMu.Lock()
	resolved := make([]string, 0, len(files))
	for _, f := range files {
		uri := span.URIFromURI(f.URI)
		filename := uri.Filename()
		goPkg, err := ragu.FastLookupGoModule(filename)
		if err != nil {
			c.lg.With(
				zap.String("filename", filename),
				zap.Error(err),
			).Debug("failed to lookup go module")
			continue
		}
		canonicalName := filepath.Join(goPkg, filepath.Base(filename))
		c.filePathsByURI[uri] = canonicalName
		c.fileURIsByPath[canonicalName] = uri
		resolved = append(resolved, canonicalName)
	}
	c.indexMu.Unlock()
	c.lg.With(
		zap.Int("files", len(resolved)),
	).Debug("files created")
	c.Compile(resolved...)

	return nil
}

func (c *Cache) OnFilesRenamed(f []protocol.FileRename) error {
	c.lg.With(
		zap.Any("files", f),
	).Debug("files renamed")

	c.indexMu.Lock()
	defer c.indexMu.Unlock()

	paths := make([]string, len(f))
	for _, file := range f {
		oldURI := span.URIFromURI(file.OldURI)
		newURI := span.URIFromURI(file.NewURI)
		path := c.filePathsByURI[oldURI]
		delete(c.filePathsByURI, oldURI)
		c.filePathsByURI[newURI] = path
		c.fileURIsByPath[path] = newURI
		paths = append(paths, path)
	}

	c.Compile(paths...)
	return nil
}

func (s *Cache) OnFileSaved(f *protocol.DidSaveTextDocumentParams) error {
	s.lg.With(
		zap.String("file", string(f.TextDocument.URI)),
	).Debug("file modified")

	if err := s.compiler.overlay.Update(f.TextDocument.URI.SpanURI(), s.filePathsByURI[f.TextDocument.URI.SpanURI()], []protocol.Msg_TextDocumentContentChangeEvent{
		{Text: *f.Text},
	}); err != nil {
		return err
	}
	s.Compile(s.filePathsByURI[f.TextDocument.URI.SpanURI()])
	return nil
}

func (c *Cache) ComputeSemanticTokens(doc protocol.TextDocumentIdentifier) ([]uint32, error) {
	result, err := semanticTokensFull(c, doc)
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (c *Cache) ComputeSemanticTokensRange(doc protocol.TextDocumentIdentifier, rng protocol.Range) ([]uint32, error) {
	result, err := semanticTokensRange(c, doc, rng)
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (c *Cache) getMapper(uri span.URI) (*protocol.Mapper, error) {
	return c.compiler.overlay.Get(c.filePathsByURI[uri])
}

func (c *Cache) ComputeDiagnosticReports(uri span.URI) ([]*protocol.Diagnostic, error) {
	c.resultsMu.Lock()
	defer c.resultsMu.Unlock()
	rawReports, found := c.diagHandler.GetDiagnosticsForPath(c.filePathsByURI[uri])
	if !found {
		return nil, nil // no reports
	}
	mapper, err := c.getMapper(uri)
	if err != nil {
		return nil, err
	}

	// convert to protocol reports
	var reports []*protocol.Diagnostic
	for _, rawReport := range rawReports {
		rng, err := mapper.OffsetRange(rawReport.Pos.Start().Offset, rawReport.Pos.End().Offset+1)
		if err != nil {
			c.lg.With(
				zap.String("file", string(uri)),
				zap.Error(err),
			).Debug("failed to map range")
			continue
		}
		reports = append(reports, &protocol.Diagnostic{
			Range:    rng,
			Severity: rawReport.Severity,
			Message:  rawReport.Error.Error(),
			Tags:     rawReport.Tags,
			Source:   "protols",
		})
	}

	return reports, nil
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
		if uri, ok := c.fileURIsByPath[path]; ok {
			nameInfo := resAst.NodeInfo(imp.Name)
			links = append(links, protocol.DocumentLink{
				Range:  toRange(nameInfo),
				Target: uri.Filename(),
			})
		}
	}

	return links, nil
}

func (c *Cache) ComputeInlayHints(doc protocol.TextDocumentIdentifier, rng protocol.Range) ([]protocol.InlayHint, error) {
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
	mapper, err := c.getMapper(doc.URI.SpanURI())
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

func (c *Cache) ComputeHover(params protocol.TextDocumentPositionParams) (*protocol.Hover, error) {
	panic("not implemented")
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
	wrap, err := desc.WrapDescriptor(d)
	if err != nil {
		return nil
	}
	printer := protoprint.Printer{
		SortElements:       true,
		CustomSortFunction: SortElements,
		Indent:             "  ",
		Compact:            protoprint.CompactDefault,
	}
	str, err := printer.PrintProtoToString(wrap)
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
	printer := protoprint.Printer{
		SortElements:       true,
		CustomSortFunction: SortElements,
		Indent:             "  ", // todo: tabs break semantic tokens
		Compact:            protoprint.CompactDefault,
	}
	path, err := c.URIToPath(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	mapper, err := c.compiler.overlay.Get(path)
	if err != nil {
		return nil, err
	}
	res, err := c.FindResultByURI(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}

	if len(maybeRange) == 1 {
		rng := maybeRange[0]
		// format range
		start, end, err := mapper.RangeOffsets(rng)
		if err != nil {
			return nil, err
		}

		// Try to map the range to a single top-level element. If the range overlaps
		// multiple top level elements, we'll just format the whole file.

		targetDesc, err := findDescriptorWithinRangeOffsets(res, start, end)
		if err != nil {
			return nil, err
		}
		splicedBuffer := bytes.NewBuffer(bytes.Clone(mapper.Content[:start]))

		wrap, err := desc.WrapDescriptor(targetDesc)
		if err != nil {
			return nil, err
		}

		err = printer.PrintProto(wrap, splicedBuffer)
		if err != nil {
			return nil, err
		}
		splicedBuffer.Write(mapper.Content[end:])
		spliced := splicedBuffer.Bytes()
		fmt.Printf("old:\n%s\nnew:\n%s\n", string(mapper.Content), string(spliced))

		edits := diff.Bytes(mapper.Content, spliced)
		return source.ToProtocolEdits(mapper, edits)
	}

	wrap, err := desc.WrapFile(res)
	if err != nil {
		return nil, err
	}
	// format whole file
	buf := bytes.NewBuffer(make([]byte, 0, len(mapper.Content)))
	err = printer.PrintProtoFile(wrap, buf)
	if err != nil {
		return nil, err
	}

	edits := diff.Bytes(mapper.Content, buf.Bytes())
	return source.ToProtocolEdits(mapper, edits)
}

func findDescriptorWithinRangeOffsets(res linker.Result, start, end int) (output protoreflect.Descriptor, err error) {
	ast := res.AST()

	err = walk.Descriptors(res, func(d protoreflect.Descriptor) error {
		node := res.Node(protoutil.ProtoFromDescriptor(d))
		tokenStart := ast.TokenInfo(node.Start())
		tokenEnd := ast.TokenInfo(node.End())
		if tokenStart.Start().Offset >= start && tokenEnd.End().Offset <= end {
			output = d
			return sentinel
		}
		return nil
	})
	if err == sentinel {
		err = nil
	}
	return
}

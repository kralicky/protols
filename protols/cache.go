package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/bmatcuk/doublestar"
	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/reporter"
	"github.com/jhump/protoreflect/desc"
	"github.com/kralicky/ragu"
	"go.uber.org/zap"
	"golang.org/x/exp/maps"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
	"golang.org/x/tools/gopls/pkg/lsp/source"
	"golang.org/x/tools/gopls/pkg/span"
	"golang.org/x/tools/pkg/diff"
	"golang.org/x/tools/pkg/jsonrpc2"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Cache is responsible for keeping track of all the known proto source files
// and definitions.
type Cache struct {
	lg                 *zap.Logger
	compiler           *Compiler
	resultsMu          sync.RWMutex
	results            linker.Files
	indexMu            sync.RWMutex
	indexedDirsByGoPkg map[string]string   // go package name -> directory
	indexedGoPkgsByDir map[string]string   // directory -> go package name
	filePathsByURI     map[span.URI]string // URI -> canonical file path (go package + file name)
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
		return nil, fmt.Errorf("package not found: %q", path)
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
		return nil, fmt.Errorf("package not found: %q", path)
	}
	return f.(linker.Result), nil
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

func (c *Cache) filePathToGoPackage(path string) string {
	dir, name := filepath.Split(path)
	dir = filepath.Clean(dir)
	pkg, ok := c.indexedGoPkgsByDir[dir]
	if !ok {
		c.lg.Debug("no go package found for directory", zap.String("dir", dir))
		return path
	}
	return filepath.Join(pkg, name)
}

var _ linker.Resolver = (*Cache)(nil)

type Compiler struct {
	*protocompile.Compiler
	workdir string
	overlay *Overlay
}

func NewCompiler(workdir string) *Compiler {
	accessor := ragu.SourceAccessor(nil)
	overlay := &Overlay{
		baseAccessor: accessor,
		sources:      map[string]*protocol.Mapper{},
	}
	resolver := protocompile.CompositeResolver{
		&protocompile.SourceResolver{
			Accessor: overlay.Accessor,
		},
		&protocompile.SourceResolver{
			Accessor: accessor,
		},
		protocompile.ResolverFunc(func(path string) (protocompile.SearchResult, error) {
			fd, err := desc.LoadFileDescriptor(path)
			if err != nil {
				return protocompile.SearchResult{}, err
			}
			return protocompile.SearchResult{Desc: fd.UnwrapFile()}, nil
		}),
	}
	return &Compiler{
		Compiler: &protocompile.Compiler{
			Resolver:       resolver,
			MaxParallelism: -1,
			Reporter:       reporter.NewReporter(nil, nil),
			SourceInfoMode: protocompile.SourceInfoExtraComments | protocompile.SourceInfoExtraOptionLocations,
			RetainResults:  true,
			RetainASTs:     true,
		},
		workdir: workdir,
		overlay: overlay,
	}
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

func (o *Overlay) Update(uri span.URI, path string, contentChanges []protocol.TextDocumentContentChangeEvent) error {
	if len(contentChanges) == 0 {
		return fmt.Errorf("%w: no content changes provided", jsonrpc2.ErrInternal)
	}

	o.sourcesMu.Lock()
	defer o.sourcesMu.Unlock()
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

// NewCache creates a new cache.
func NewCache(workdir string, lg *zap.Logger) *Cache {
	cache := &Cache{
		lg:                 lg,
		compiler:           NewCompiler(workdir),
		indexedDirsByGoPkg: map[string]string{},
		indexedGoPkgsByDir: map[string]string{},
		filePathsByURI:     map[span.URI]string{},
	}
	cache.Reindex()
	return cache
}

func (c *Cache) Reindex() {
	c.lg.Debug("reindexing")
	c.indexMu.Lock()
	defer c.indexMu.Unlock()

	maps.Clear(c.indexedDirsByGoPkg)
	maps.Clear(c.indexedGoPkgsByDir)
	maps.Clear(c.filePathsByURI)
	allProtos, _ := doublestar.Glob(path.Join(c.compiler.workdir, "**/*.proto"))
	c.lg.Debug("found protos", zap.Strings("protos", allProtos))
	var resolved []string
	for _, proto := range allProtos {
		goPkg, err := ragu.FastLookupGoModule(proto)
		if err != nil {
			c.lg.With(
				zap.String("proto", proto),
				zap.Error(err),
			).Debug("failed to lookup go module")
			continue
		}
		c.indexedDirsByGoPkg[goPkg] = filepath.Dir(proto)
		c.indexedGoPkgsByDir[filepath.Dir(proto)] = goPkg
		canonicalName := filepath.Join(goPkg, filepath.Base(proto))
		c.filePathsByURI[span.URIFromPath(proto)] = canonicalName
		resolved = append(resolved, canonicalName)
	}
	c.Compile(resolved...)
}

func (c *Cache) Compile(protos ...string) {
	c.resultsMu.Lock()
	defer c.resultsMu.Unlock()
	c.lg.Info("compiling", zap.Int("protos", len(protos)))
	res, err := c.compiler.Compile(context.TODO(), protos...)
	if err != nil {
		c.lg.With(zap.Error(err)).Error("failed to compile")
		return
	}
	c.results = res
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
	s.lg.With(
		zap.String("file", string(f.URI)),
	).Debug("file modified")

	if err := s.compiler.overlay.Update(f.URI.SpanURI(), s.filePathsByURI[f.URI.SpanURI()], contentChanges); err != nil {
		return err
	}
	s.Compile(s.filePathsByURI[f.URI.SpanURI()])
	return nil
}

func (s *Cache) OnFileDeleted(f protocol.FileDelete) error {
	return nil // TODO
}

func (s *Cache) OnFileCreated(f protocol.FileCreate) error {
	return nil // TODO
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

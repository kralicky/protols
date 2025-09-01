package lsp

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gsync "github.com/kralicky/gpkg/sync"
	"github.com/kralicky/protocompile"
	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protocompile/parser"
	"github.com/kralicky/protocompile/reporter"
	"github.com/kralicky/tools-lite/gopls/pkg/file"
	"github.com/kralicky/tools-lite/gopls/pkg/protocol"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/encoding/protowire"
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
	settings    atomic.Pointer[Settings]

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

type CacheOptions struct{}

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
			InterpretOptionsLenient:      true,
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
	cache.DidChangeConfiguration(context.TODO(), Settings{}) // load default settings

	compiler.Hooks = protocompile.CompilerHooks{
		PreInvalidate:  cache.preInvalidateHook,
		PostInvalidate: cache.postInvalidateHook,
		PreCompile:     cache.preCompile,
		PostCompile:    cache.postCompile,
	}
	return cache
}

func (c *Cache) LoadFiles(files []string) {
	created := make([]file.Modification, len(files))
	for i, f := range files {
		created[i] = file.Modification{
			Action:  file.Create,
			OnDisk:  true,
			URI:     protocol.URIFromPath(f),
			Version: -1,
		}
	}

	c.DidModifyFiles(context.TODO(), created)
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
		if res.IsPlaceholder() {
			continue
		}
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
		return nil, fmt.Errorf("FindResultByPath: no results exist")
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
		parseRes:     parseRes,
		maybeLinkRes: linkRes,
	}
	offset, err := mapper.PositionOffset(params.Position)
	if err != nil {
		return nil, protocol.Range{}, err
	}
	root := enc.AST()
	if root == nil {
		return nil, protocol.Range{}, nil
	}
	token, comment := root.ItemAtOffset(offset)
	if comment.IsValid() {
		return nil, protocol.Range{}, nil
	}

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
	parentFile := desc.ParentFile()
	if parentFile == nil {
		return protocol.Location{}, fmt.Errorf("no parent file found for descriptor")
	}
	linkRes, err := c.findResultOrPartialResultByPathLocked(parentFile.Path())
	if err != nil {
		return protocol.Location{}, err
	}

	ref, err := findDefinition(desc, linkRes)
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

func (c *Cache) DidChangeConfiguration(ctx context.Context, settings Settings) error {
	c.settings.Store(&settings)
	return nil
}

type WorkspaceDescriptors interface {
	Len() int
	All() []protoreflect.Descriptor
	Range(func(path string, descriptors []protoreflect.Descriptor))
}

type workspaceDescriptors struct {
	results   [][]protoreflect.Descriptor
	pathIndex []string
	len       int
}

func (wd workspaceDescriptors) All() []protoreflect.Descriptor {
	all := make([]protoreflect.Descriptor, 0, wd.len)
	for _, item := range wd.results {
		all = append(all, item...)
	}
	return all
}

func (wd workspaceDescriptors) Range(fn func(path string, descriptors []protoreflect.Descriptor)) {
	for i, path := range wd.pathIndex {
		fn(path, wd.results[i])
	}
}

func (wd workspaceDescriptors) Len() int {
	return wd.len
}

func (c *Cache) FindAllDescriptorsByPrefix(ctx context.Context, prefix string, filter ...func(protoreflect.Descriptor) bool) WorkspaceDescriptors {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.findAllDescriptorsByPrefixLocked(ctx, prefix, filter...)
}

func (c *Cache) findAllDescriptorsByPrefixLocked(ctx context.Context, prefix string, filter ...func(protoreflect.Descriptor) bool) WorkspaceDescriptors {
	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(runtime.NumCPU())
	descriptorsByFile := make([][]protoreflect.Descriptor, len(c.results))
	pathIndex := make([]string, len(c.results))
	for i, res := range c.results {
		if res.IsPlaceholder() {
			if partial, ok := c.partiallyLinkedResults[protocompile.ResolvedPath(res.Path())]; ok {
				res = partial
			} else {
				continue
			}
		}
		pathIndex[i] = res.Path()
		pkg := res.Package()
		if pkg == "" {
			// skip searching in files that do not have a package name
			continue
		}
		eg.Go(func() (err error) {
			descriptorsByFile[i], err = res.(linker.Result).FindDescriptorsByPrefix(ctx, string(pkg.Append(protoreflect.Name(prefix))), filter...)
			return
		})
	}
	eg.Wait()
	totalLen := 0
	for _, item := range descriptorsByFile {
		totalLen += len(item)
	}
	return workspaceDescriptors{
		results:   descriptorsByFile,
		pathIndex: pathIndex,
		len:       totalLen,
	}
}

// Like FindAllDescriptorsByPrefix, but assumes a fully qualified prefix with
// package name.
func (c *Cache) FindAllDescriptorsByQualifiedPrefix(ctx context.Context, prefix string, filter ...func(protoreflect.Descriptor) bool) WorkspaceDescriptors {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.findAllDescriptorsByQualifiedPrefixLocked(ctx, prefix, filter...)
}

func (c *Cache) findAllDescriptorsByQualifiedPrefixLocked(ctx context.Context, prefix string, filter ...func(protoreflect.Descriptor) bool) WorkspaceDescriptors {
	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(runtime.NumCPU())
	resultsByPackage := make([][]protoreflect.Descriptor, len(c.results))
	pathIndex := make([]string, len(c.results))
	isFullyQualified := strings.HasPrefix(prefix, ".")
	if isFullyQualified {
		prefix = prefix[1:]
	}
	for i, res := range c.results {
		if res.IsPlaceholder() {
			if partial, ok := c.partiallyLinkedResults[protocompile.ResolvedPath(res.Path())]; ok {
				res = partial
			} else {
				continue
			}
		}
		pathIndex[i] = res.Path()
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
	totalLen := 0
	for _, item := range resultsByPackage {
		totalLen += len(item)
	}
	return workspaceDescriptors{
		results:   resultsByPackage,
		pathIndex: pathIndex,
		len:       totalLen,
	}
}

// RangeAllDescriptors calls fn for each descriptor in the cache, possibly in
// a separate goroutine for each file.
func (c *Cache) RangeAllDescriptors(ctx context.Context, fn func(protoreflect.Descriptor) bool) error {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.rangeAllDescriptorsLocked(ctx, fn)
}

func (c *Cache) rangeAllDescriptorsLocked(ctx context.Context, fn func(protoreflect.Descriptor) bool) error {
	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(runtime.NumCPU())
	for _, res := range c.results {
		if res.IsPlaceholder() {
			continue
		}
		eg.Go(func() (err error) {
			return res.(linker.Result).RangeDescriptors(ctx, fn)
		})
	}
	return eg.Wait()
}

func (c *Cache) FindImportPathsByPrefix(ctx context.Context, prefix string) map[protocol.DocumentURI]string {
	c.resultsMu.RLock()
	defer c.resultsMu.RUnlock()
	return c.resolver.findImportPathsByPrefix(prefix)
}

func (c *Cache) FindPragmasByPath(path protocompile.ResolvedPath) (Pragmas, bool) {
	p, ok := c.pragmas.Load(path)
	return p, ok
}

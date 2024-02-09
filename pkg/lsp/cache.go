package lsp

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	gsync "github.com/kralicky/gpkg/sync"
	"github.com/kralicky/protocompile"
	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protocompile/parser"
	"github.com/kralicky/protocompile/reporter"
	"github.com/kralicky/tools-lite/gopls/pkg/file"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
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

	token, comment := root.ItemAtOffset(offset)
	if token == ast.TokenError && comment.IsValid() {
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

func (c *Cache) FindPragmasByPath(path protocompile.ResolvedPath) (Pragmas, bool) {
	p, ok := c.pragmas.Load(path)
	return p, ok
}

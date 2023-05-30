package main

import (
	"context"
	"os"
	"path"
	"path/filepath"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/reporter"
	"github.com/kralicky/ragu"
	"github.com/samber/lo"
	"go.uber.org/zap"
)

// Cache is responsible for keeping track of all the known proto source files
// and definitions.
type Cache struct {
	lg              *zap.Logger
	sources         []string
	sourcePackages  map[string]string
	sourcePkgDirs   map[string]string
	sourceFilenames map[string]string
	compiler        *protocompile.Compiler
	files           linker.Files
}

// NewCache creates a new cache.
func NewCache(sources []string, lg *zap.Logger) *Cache {
	return &Cache{
		lg:      lg,
		sources: sources,
	}
}

func (c *Cache) Reindex(ctx context.Context) error {
	c.sourcePackages = map[string]string{}
	c.sourcePkgDirs = map[string]string{}
	c.sourceFilenames = map[string]string{}

	resolved, err := ragu.ResolvePatterns(c.sources)
	if err != nil {
		return err
	}
	c.lg.With(
		zap.Strings("sources", c.sources),
		zap.Strings("files", resolved),
	).Debug("sources resolved")

	for _, source := range resolved {
		goPkg, err := ragu.FastLookupGoModule(source)
		if err != nil {
			c.lg.With(
				zap.String("source", source),
				zap.Error(err),
			).Debug("failed to lookup go module")
			continue
		}
		c.sourcePkgDirs[goPkg] = filepath.Dir(source)
		c.sourcePackages[path.Join(goPkg, path.Base(source))] = source
		c.sourceFilenames[source] = path.Join(goPkg, path.Base(source))
	}
	accessor := ragu.SourceAccessor(c.sourcePackages)
	res := ragu.NewResolver(accessor)
	compiler := protocompile.Compiler{
		Resolver:       res,
		MaxParallelism: -1,
		SourceInfoMode: protocompile.SourceInfoExtraComments | protocompile.SourceInfoExtraOptionLocations,
		Reporter:       reporter.NewReporter(nil, nil),
		RetainASTs:     true,
	}
	results, err := compiler.Compile(ctx, lo.Keys(c.sourcePackages)...)
	if err != nil {
		c.lg.With(
			zap.Strings("sources", c.sources),
			zap.Error(err),
		).Debug("failed to compile")
		return err
	}
	c.compiler = &compiler
	c.files = results
	return nil
}

func (c *Cache) FindFileByPath(path string) (linker.Result, error) {
	if f, ok := c.sourceFilenames[path]; ok {
		path = f
	}
	f := c.files.FindFileByPath(path)
	if f == nil {
		c.lg.With(
			zap.String("path", path),
			zap.Any("files", c.files),
		).Debug("file not found")
		return nil, os.ErrNotExist
	}
	c.lg.With(
		zap.String("path", path),
		zap.Any("result", f),
	).Debug("file found")
	return f.(linker.Result), nil
}

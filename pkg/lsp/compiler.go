package lsp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/kralicky/protocompile"
	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protocompile/reporter"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/cache"
)

type Compiler struct {
	*protocompile.Compiler
	fs      *cache.OverlayFS
	workdir string
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
	c.compileLocked(protos...)
	for _, f := range after {
		f()
	}
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
	c.partialResultsMu.Lock()
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
		delete(c.partiallyLinkedResults, protocompile.ResolvedPath(path))
		delete(c.unlinkedResults, protocompile.ResolvedPath(path))
	}
	for path, partial := range res.PartialLinkResults {
		partial := partial
		slog.With("path", path).Debug("adding new partial linker result")
		c.partiallyLinkedResults[path] = partial
		delete(c.unlinkedResults, path)
		for i, f := range c.results {
			if f.Path() == string(path) {
				c.results[i] = linker.NewPlaceholderFile(partial.Path())
				break
			}
		}
		c.pragmas.Store(path, &pragmaMap{m: partial.AST().Pragmas})
	}
	for path, partial := range res.UnlinkedParserResults {
		partial := partial
		slog.With("path", path).Debug("adding new partial linker result")
		c.unlinkedResults[path] = partial
		delete(c.partiallyLinkedResults, path)
		for i, f := range c.results {
			if f.Path() == string(path) {
				c.results[i] = linker.NewPlaceholderFile(f.Path())
				break
			}
		}
		c.pragmas.Store(path, &pragmaMap{m: partial.AST().Pragmas})
	}
	c.partialResultsMu.Unlock()

	syntheticFiles := c.resolver.CheckIncompleteDescriptors(c.results)
	if len(syntheticFiles) == 0 {
		return
	}
	slog.Debug("building new synthetic sources", "sources", len(syntheticFiles))
	c.compileLocked(syntheticFiles...)
}

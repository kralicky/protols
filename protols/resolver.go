package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bufbuild/protocompile"
	"github.com/jhump/protoreflect/desc"
	"go.uber.org/zap"
	"golang.org/x/exp/maps"
	"golang.org/x/tools/gopls/pkg/lsp/cache"
	"golang.org/x/tools/gopls/pkg/lsp/source"
	"golang.org/x/tools/gopls/pkg/span"
)

type Resolver struct {
	*cache.OverlayFS
	synthesizer    *ProtoSourceSynthesizer
	lg             *zap.Logger
	pathsMu        sync.RWMutex
	filePathsByURI map[span.URI]string // URI -> canonical file path (go package + file name)
	fileURIsByPath map[string]span.URI // canonical file path (go package + file name) -> URI
}

func NewResolver(workdir string, lg *zap.Logger) *Resolver {
	return &Resolver{
		lg:             lg,
		OverlayFS:      cache.NewOverlayFS(cache.NewMemoizedFS()),
		synthesizer:    NewProtoSourceSynthesizer(workdir),
		filePathsByURI: make(map[span.URI]string),
		fileURIsByPath: make(map[string]span.URI),
	}
}

func (r *Resolver) ResetPathMappings() {
	r.pathsMu.Lock()
	defer r.pathsMu.Unlock()
	maps.Clear(r.filePathsByURI)
	maps.Clear(r.fileURIsByPath)
}

func (r *Resolver) PathToURI(path string) (span.URI, error) {
	r.pathsMu.RLock()
	defer r.pathsMu.RUnlock()
	uri, ok := r.fileURIsByPath[path]
	if !ok {
		return "", fmt.Errorf("%w: path %q", os.ErrNotExist, path)
	}
	return uri, nil
}

func (r *Resolver) URIToPath(uri span.URI) (string, error) {
	r.pathsMu.RLock()
	defer r.pathsMu.RUnlock()
	path, ok := r.filePathsByURI[uri]
	if !ok {
		return "", fmt.Errorf("%w: URI %q", os.ErrNotExist, uri)
	}
	return path, nil
}

func (r *Resolver) UpdateURIPathMappings(modifications []source.FileModification) {
	r.pathsMu.Lock()
	defer r.pathsMu.Unlock()
	for _, m := range modifications {
		switch m.Action {
		case source.Open:
			filename := m.URI.Filename()
			mod, err := FastLookupGoModule(filename)
			if err != nil {
				r.lg.With(
					zap.String("filename", filename),
					zap.Error(err),
				).Error("failed to lookup go module")
				continue
			}
			path := filepath.Join(mod, filepath.Base(filename))
			r.filePathsByURI[m.URI] = path
		case source.Close:
		case source.Save:
		case source.Change:
			if m.OnDisk {
				// check for go_package modification
				existingPath := r.filePathsByURI[m.URI]
				filename := m.URI.Filename()
				mod, err := FastLookupGoModule(filename)
				if err != nil {
					r.lg.With(
						zap.String("filename", filename),
						zap.Error(err),
					).Error("failed to lookup go module")
					continue
				}
				updatedPath := filepath.Join(mod, filepath.Base(filename))
				if updatedPath != existingPath {
					r.filePathsByURI[m.URI] = updatedPath
					r.fileURIsByPath[updatedPath] = m.URI
					delete(r.fileURIsByPath, existingPath)
				}
			}
		case source.Create:
			filename := m.URI.Filename()
			goPkg, err := FastLookupGoModule(filename)
			if err != nil {
				r.lg.With(
					zap.String("filename", filename),
					zap.Error(err),
				).Error("failed to lookup go module")
				continue
			}
			canonicalName := filepath.Join(goPkg, filepath.Base(filename))
			r.filePathsByURI[m.URI] = canonicalName
			r.fileURIsByPath[canonicalName] = m.URI
		case source.Delete:
			path := r.filePathsByURI[m.URI]
			delete(r.filePathsByURI, m.URI)
			delete(r.fileURIsByPath, path)
		}
	}
}

// Path resolution order:
// 1. Check for well-known import paths like google/*
// 2. Check if the path is a file on disk
// 3. Check if the path is a go module containing proto sources
// 3.5. Check if the path is a go module path containing generated code, but no proto sources
// 4. Check if the path is found in the global message cache
func (r *Resolver) FindFileByPath(path string) (protocompile.SearchResult, error) {
	r.pathsMu.RLock()
	defer r.pathsMu.RUnlock()

	if result, err := r.checkWellKnownImportPath(path); err == nil {
		r.lg.With(zap.String("path", path)).Debug("resolved to well-known import path")
		return result, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		r.lg.With(zap.String("path", path)).Error("failed to check well-known import path")
		return protocompile.SearchResult{}, err
	}
	if result, err := r.checkFS(path); err == nil {
		r.lg.With(zap.String("path", path)).Debug("resolved to cached file")
		return result, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		r.lg.With(zap.String("path", path), zap.Error(err)).Debug("failed to check cached file")
		return protocompile.SearchResult{}, err
	}
	if result, err := r.checkGoModule(path); err == nil {
		r.lg.With(zap.String("path", path)).Debug("resolved to go module")
		return result, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		r.lg.With(zap.String("path", path)).Debug("failed to check go module")
		return protocompile.SearchResult{}, err
	}
	if result, err := r.checkGlobalCache(path); err == nil {
		r.lg.With(zap.String("path", path)).Debug("resolved to type in global descriptor cache")
		return result, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		r.lg.With(zap.String("path", path)).Debug("failed to check global descriptor cache")
		return protocompile.SearchResult{}, err
	}

	r.lg.With(zap.String("path", path)).Debug("could not resolve path")
	return protocompile.SearchResult{}, os.ErrNotExist
}

func (r *Resolver) checkWellKnownImportPath(path string) (protocompile.SearchResult, error) {
	if strings.HasPrefix(path, "google/") {
		return r.checkGlobalCache(path)
	}
	if strings.HasPrefix(path, "gogoproto/") {
		return r.checkGlobalCache("github.com/gogo/protobuf/" + path)
	}
	return protocompile.SearchResult{}, os.ErrNotExist
}

func (r *Resolver) checkFS(path string) (protocompile.SearchResult, error) {
	uri, err := r.PathToURI(path)
	if err != nil {
		return protocompile.SearchResult{}, err
	}
	if fh, err := r.ReadFile(context.TODO(), uri); err == nil {
		content, err := fh.Content()
		if err == nil && content != nil {
			return protocompile.SearchResult{
				Source: bytes.NewReader(content),
			}, nil
		}
	}
	return protocompile.SearchResult{}, os.ErrNotExist
}

func (r *Resolver) checkGoModule(path string) (protocompile.SearchResult, error) {
	f, dir, err := r.synthesizer.ImportFromGoModule(path)
	if err == nil {
		src, err := os.Open(f)
		if err == nil {
			return protocompile.SearchResult{
				Source: src,
			}, nil
		}
	}
	if dir != "" {
		if synthesized, err := r.synthesizer.SynthesizeFromGoSource(path, dir); err == nil {
			return protocompile.SearchResult{
				Proto: synthesized,
			}, nil
		} else {
			return protocompile.SearchResult{}, fmt.Errorf("failed to synthesize %s: %w", path, err)
		}
	}
	return protocompile.SearchResult{}, os.ErrNotExist
}

func (r *Resolver) checkGlobalCache(path string) (protocompile.SearchResult, error) {
	fd, err := desc.LoadFileDescriptor(path)
	if err != nil {
		return protocompile.SearchResult{}, err
	}
	return protocompile.SearchResult{Desc: fd.UnwrapFile()}, nil
}

// type Overlay struct {
// 	baseAccessor func(uri span.URI) (io.ReadCloser, error)
// 	sourcesMu    sync.Mutex
// 	sources      map[span.URI]*protocol.Mapper
// }

// func (o *Overlay) Create(uri span.URI, contents []byte) error {
// 	o.sourcesMu.Lock()
// 	defer o.sourcesMu.Unlock()
// 	if _, ok := o.sources[uri]; ok {
// 		return fmt.Errorf("%w: file already exists", jsonrpc2.ErrInternal)
// 	}
// 	o.sources[uri] = protocol.NewMapper(uri, contents)
// 	return nil
// }

// // requires sourcesMu to be locked (todo: fix)
// func (o *Overlay) Update(uri span.URI, contentChanges []protocol.TextDocumentContentChangeEvent) error {
// 	if len(contentChanges) == 0 {
// 		return fmt.Errorf("%w: no content changes provided", jsonrpc2.ErrInternal)
// 	}

// 	if _, ok := o.sources[uri]; !ok {
// 		baseReader, err := o.baseAccessor(uri)
// 		if err != nil {
// 			return err
// 		}
// 		defer baseReader.Close()
// 		baseContent, _ := io.ReadAll(baseReader)
// 		o.sources[uri] = protocol.NewMapper(uri, baseContent)
// 	}
// 	source := o.sources[uri]
// 	newSrc, err := applyChanges(source, contentChanges)
// 	if err != nil {
// 		return err
// 	}

// 	o.sources[uri] = protocol.NewMapper(uri, newSrc)
// 	return nil
// }

// func (o *Overlay) ReloadFromDisk(uri span.URI) error {
// 	o.sourcesMu.Lock()
// 	defer o.sourcesMu.Unlock()
// 	baseReader, err := o.baseAccessor(uri)
// 	if err != nil {
// 		return err
// 	}
// 	defer baseReader.Close()
// 	baseContent, _ := io.ReadAll(baseReader)
// 	o.sources[uri] = protocol.NewMapper(uri, baseContent)
// 	return nil
// }

// func (o *Overlay) Delete(path string) {
// 	o.sourcesMu.Lock()
// 	defer o.sourcesMu.Unlock()
// 	delete(o.sources, path)
// }

// func (o *Overlay) Accessor(path string) (io.ReadCloser, error) {
// 	o.sourcesMu.Lock()
// 	defer o.sourcesMu.Unlock()
// 	if source, ok := o.sources[path]; ok {
// 		return io.NopCloser(bytes.NewReader(source.Content)), nil
// 	}
// 	return nil, os.ErrNotExist
// }
// func (o *Overlay) Get(path string) (*protocol.Mapper, error) {
// 	o.sourcesMu.Lock()
// 	defer o.sourcesMu.Unlock()
// 	if source, ok := o.sources[path]; ok {
// 		return source, nil
// 	}
// 	return nil, os.ErrNotExist
// }

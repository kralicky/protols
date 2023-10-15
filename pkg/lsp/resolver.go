package lsp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/linker"
	gogo "github.com/gogo/protobuf/proto"
	"github.com/jhump/protoreflect/desc"
	"github.com/kralicky/protols/pkg/format"
	"golang.org/x/tools/gopls/pkg/lsp/cache"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
	"golang.org/x/tools/gopls/pkg/lsp/source"
	"golang.org/x/tools/gopls/pkg/span"
	"google.golang.org/protobuf/reflect/protodesc"
)

type ImportSource int

const (
	SourceWellKnown ImportSource = iota + 1
	SourceRelativePath
	SourceLocalGoModule
	SourceGoModuleCache
	SourceSynthetic
)

type Resolver struct {
	*cache.OverlayFS
	folder             protocol.WorkspaceFolder
	synthesizer        *ProtoSourceSynthesizer
	pathsMu            sync.RWMutex
	filePathsByURI     map[span.URI]string // URI -> canonical file path (go package + file name)
	fileURIsByPath     map[string]span.URI // canonical file path (go package + file name) -> URI
	importSourcesByURI map[span.URI]ImportSource
	syntheticFiles     map[span.URI]string
}

func NewResolver(folder protocol.WorkspaceFolder) *Resolver {
	return &Resolver{
		folder:             folder,
		OverlayFS:          cache.NewOverlayFS(cache.NewMemoizedFS()),
		synthesizer:        NewProtoSourceSynthesizer(span.URIFromURI(folder.URI).Filename()),
		filePathsByURI:     make(map[span.URI]string),
		fileURIsByPath:     make(map[string]span.URI),
		syntheticFiles:     make(map[span.URI]string),
		importSourcesByURI: map[span.URI]ImportSource{},
	}
}

func (r *Resolver) PathToURI(path string) (span.URI, error) {
	r.pathsMu.RLock()
	defer r.pathsMu.RUnlock()

	if i := strings.IndexRune(path, ';'); i != -1 {
		path = path[:i] // strip trailing ;packagename directive
	}

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

func (r *Resolver) SyntheticFileContents(uri span.URI) (string, error) {
	r.pathsMu.RLock()
	defer r.pathsMu.RUnlock()
	contents, ok := r.syntheticFiles[uri]
	if !ok {
		return "", fmt.Errorf("%w: URI %q", os.ErrNotExist, uri)
	}
	return contents, nil
}

func (r *Resolver) UpdateURIPathMappings(modifications []source.FileModification) {
	r.pathsMu.Lock()
	defer r.pathsMu.Unlock()
	for _, m := range modifications {
		switch m.Action {
		case source.Close:
		case source.Change, source.Save:
			// check for go_package modification
			if r.importSourcesByURI[m.URI] == SourceLocalGoModule {
				existingPath := r.filePathsByURI[m.URI]
				filename := m.URI.Filename()
				var f io.ReadCloser
				if m.Text != nil {
					f = io.NopCloser(bytes.NewReader(m.Text))
				} else {
					var err error
					f, err = os.Open(filename)
					if err != nil {
						slog.With(
							"filename", filename,
							"error", err,
						).Error("failed to open file")
						continue
					}
				}
				mod, err := r.LookupGoModule(filename, f)
				if err != nil {
					slog.With(
						"filename", filename,
						"error", err,
					).Error("failed to lookup go module")
					continue
				}
				updatedPath := filepath.Join(mod, filepath.Base(filename))
				if updatedPath != existingPath {
					slog.With(
						"existingPath", existingPath,
						"updatedPath", updatedPath,
					).Debug("updating path mapping")
					r.filePathsByURI[m.URI] = updatedPath
					r.fileURIsByPath[updatedPath] = m.URI
					if existingPath != "" {
						delete(r.fileURIsByPath, existingPath)
					}
				}
			}
		case source.Create:
			filename := m.URI.Filename()
			f, err := os.Open(filename)
			if err != nil {
				slog.With(
					"filename", filename,
					"error", err,
				).Error("failed to open file")
				continue
			}
			goPkg, err := r.LookupGoModule(filename, f)
			if err != nil {
				slog.With(
					"filename", filename,
					"error", err,
				).Error("failed to lookup go module")
				r.filePathsByURI[m.URI] = ""
				continue
			}
			canonicalName := filepath.Join(goPkg, filepath.Base(filename))
			r.filePathsByURI[m.URI] = canonicalName
			r.fileURIsByPath[canonicalName] = m.URI
			r.importSourcesByURI[m.URI] = SourceLocalGoModule
		case source.Delete:
			path := r.filePathsByURI[m.URI]
			delete(r.filePathsByURI, m.URI)
			delete(r.importSourcesByURI, m.URI)
			delete(r.fileURIsByPath, path)
		case source.Open:
			// not necessarily a local go module

		}
	}
}

// CheckIncompleteDescriptors fills in placeholder sources for synthetic files
// that did not have fully linked descriptors at the time of creation, and
// returns a list of paths that need to be compiled again.
func (r *Resolver) CheckIncompleteDescriptors(results linker.SortedFiles) []string {
	r.pathsMu.Lock()
	defer r.pathsMu.Unlock()

	compileAgain := []string{}
	for uri, path := range r.filePathsByURI {
		if strings.HasPrefix(string(uri), "proto://") {
			if _, ok := r.syntheticFiles[uri]; !ok {
				res := results.FindFileByPath(path)
				newFile, err := protodesc.NewFile(protodesc.ToFileDescriptorProto(res), results.AsResolver())
				if err != nil {
					slog.With(
						"uri", string(uri),
						"error", err,
					).Error("failed to generate synthetic file descriptor")
				}
				src, err := format.PrintDescriptor(newFile)
				if err != nil {
					slog.With(
						"uri", string(uri),
						"error", err,
					).Error("failed to generate synthetic file source")
					continue
				}
				r.syntheticFiles[uri] = src
				// these files aren't going to have ASTs yet and will need to be recompiled
				compileAgain = append(compileAgain, path)
			}
		}
	}
	return compileAgain
}

// Path resolution order:
// 1. Check for well-known import paths like google/*
// 2. Check if the path is a file on disk
// 3. Check if the path is a go module containing proto sources
// 3.5. Check if the path is a go module path containing generated code, but no proto sources
// 4. Check if the path is found in the global message cache
func (r *Resolver) FindFileByPath(path string) (protocompile.SearchResult, error) {
	r.pathsMu.Lock()
	defer r.pathsMu.Unlock()
	return r.findFileByPathLocked(path)
}

func (r *Resolver) findFileByPathLocked(path string) (protocompile.SearchResult, error) {
	var isSynthetic bool
	if uri, ok := r.fileURIsByPath[path]; ok {
		if strings.HasPrefix(string(uri), "proto://") {
			isSynthetic = true
		}
	}
	lg := slog.With("path", path)
	if result, err := r.checkWellKnownImportPath(path); err == nil {
		lg.Debug("resolved to well-known import path")
		return result, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		lg.Error("failed to check well-known import path")
		return protocompile.SearchResult{}, err
	}
	if !isSynthetic {
		if result, err := r.checkFS(path); err == nil {
			lg.Debug("resolved to cached file")
			return result, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			slog.With("path", path, "error", err).Debug("failed to check cached file")
			return protocompile.SearchResult{}, err
		}
	}
	if result, err := r.checkGoModule(path); err == nil {
		lg.Debug("resolved to go module")
		return result, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		lg.Debug("failed to check go module")
		return protocompile.SearchResult{}, err
	}
	if result, err := r.checkGlobalCache(path); err == nil {
		lg.Debug("resolved to type in global descriptor cache")
		return result, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		lg.Debug("failed to check global descriptor cache")
		return protocompile.SearchResult{}, err
	}

	lg.Debug("could not resolve path")
	return protocompile.SearchResult{}, os.ErrNotExist
}

func (r *Resolver) checkWellKnownImportPath(path string) (protocompile.SearchResult, error) {
	if strings.HasPrefix(path, "google/") {
		return r.checkGlobalCache(path)
	}
	if filepath.Base(path) == "gogo.proto" {
		descriptorBytes := gogo.FileDescriptor("gogo.proto")
		if descriptorBytes != nil {
			fd, err := DecodeRawFileDescriptor(descriptorBytes)
			if err != nil {
				return protocompile.SearchResult{}, err
			}
			*fd.Name = path
			syntheticURI := url.URL{
				Scheme:   "proto",
				Path:     path,
				Fragment: r.folder.Name,
			}
			uri := span.URI(syntheticURI.String())
			r.filePathsByURI[uri] = path
			r.fileURIsByPath[path] = uri
			return protocompile.SearchResult{
				Proto: fd,
			}, nil
		}
	}
	return protocompile.SearchResult{}, os.ErrNotExist
}

func (r *Resolver) checkFS(path string) (protocompile.SearchResult, error) {
	uri, ok := r.fileURIsByPath[path]
	if !ok {
		return protocompile.SearchResult{}, os.ErrNotExist
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
	res, err := r.synthesizer.ImportFromGoModule(path)
	if err != nil {
		return protocompile.SearchResult{}, err
	}
	if res.SourceExists {
		src, err := os.Open(res.SourcePath)
		if err != nil {
			return protocompile.SearchResult{}, err
		}
		uri := span.URIFromPath(res.SourcePath)
		r.filePathsByURI[uri] = path
		r.fileURIsByPath[path] = uri
		// todo: need to fix the incomplete go_package values in some imports
		return protocompile.SearchResult{
			Source: src,
		}, nil
	} else if src, ok := r.syntheticFiles[r.fileURIsByPath[path]]; ok {
		return protocompile.SearchResult{
			Source: strings.NewReader(src),
		}, nil
	} else if synthesized, err := r.synthesizer.SynthesizeFromGoSource(path, res); err == nil {
		syntheticURI := url.URL{
			Scheme:   "proto",
			Path:     synthesized.GetName(),
			Fragment: r.folder.Name,
		}
		uri := span.URI(syntheticURI.String())
		r.filePathsByURI[uri] = path
		r.fileURIsByPath[path] = uri
		return protocompile.SearchResult{
			Proto: synthesized,
		}, nil
	}
	return protocompile.SearchResult{}, fmt.Errorf("failed to synthesize %s: %w", path, err)
}

func (r *Resolver) checkGlobalCache(path string) (protocompile.SearchResult, error) {
	fd, err := desc.LoadFileDescriptor(path)
	if err != nil {
		return protocompile.SearchResult{}, err
	}
	syntheticURI := url.URL{
		Scheme:   "proto",
		Path:     path,
		Fragment: r.folder.Name,
	}
	uri := span.URI(syntheticURI.String())
	r.filePathsByURI[uri] = path
	r.fileURIsByPath[path] = uri
	src, err := format.PrintDescriptor(fd.UnwrapFile())
	if err != nil {
		return protocompile.SearchResult{Desc: fd.UnwrapFile()}, nil
	}
	r.syntheticFiles[uri] = src
	return protocompile.SearchResult{
		Source: strings.NewReader(src),
	}, nil
}

func (r *Resolver) SyntheticFiles() []span.URI {
	var uris []span.URI
	for uri := range r.syntheticFiles {
		uris = append(uris, uri)
	}
	sort.Slice(uris, func(i, j int) bool {
		return string(uris[i]) < string(uris[j])
	})
	return uris
}

func (r *Resolver) LookupGoModule(filename string, f io.ReadCloser) (string, error) {
	// // Check if the file is in a go package directory
	// if pkgName, err := imports.PackageDirToName(filepath.Dir(filename)); err == nil {
	// 	return pkgName, nil
	// }

	// Check if the filename is relative to a local go module
	if pkgName, err := r.synthesizer.ImplicitGoPackagePath(filename); err == nil {
		return pkgName, nil
	}

	// If the file contains a go_module option, use that
	if mod, err := FastLookupGoModule(f); err == nil {
		return mod, nil
	}

	return "", fmt.Errorf("could not determine go module for %s", filename)
}

func FastLookupGoModule(f io.ReadCloser) (string, error) {
	// Search the .proto file for `option go_package = "...";`
	// We know this will be somewhere at the top of the file.
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, "option") {
			continue
		}
		index := strings.Index(line, "go_package")
		if index == -1 {
			continue
		}
		for ; index < len(line); index++ {
			if line[index] == '=' {
				break
			}
		}
		for ; index < len(line); index++ {
			if line[index] == '"' {
				break
			}
		}
		if index == len(line) {
			continue
		}
		startIdx := index + 1
		endIdx := strings.LastIndexByte(line, '"')
		endSemicolon := strings.IndexByte(line, ';')
		if endSemicolon > startIdx && endSemicolon < endIdx {
			endIdx = endSemicolon
		}
		if endIdx <= startIdx {
			continue
		}
		return line[startIdx:endIdx], nil
	}
	return "", fmt.Errorf("no go_package option found")
}

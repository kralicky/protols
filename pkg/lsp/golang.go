package lsp

import (
	"bytes"
	"compress/gzip"
	"fmt"
	goast "go/ast"
	goparser "go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kralicky/tools-lite/gopls/pkg/protocol"
	"github.com/kralicky/tools-lite/pkg/diff"
	"github.com/kralicky/tools-lite/pkg/gocommand"
	"github.com/kralicky/tools-lite/pkg/imports"
	"golang.org/x/mod/module"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

type GoLanguageDriver struct {
	processEnv                *imports.ProcessEnv
	moduleResolver            *imports.ModuleResolver
	knownAlternativePackages  [][]diff.Edit
	localModDir, localModName string
}

var requiredGoEnvVars = []string{"GO111MODULE", "GOFLAGS", "GOINSECURE", "GOMOD", "GOMODCACHE", "GONOPROXY", "GONOSUMDB", "GOPATH", "GOPROXY", "GOROOT", "GOSUMDB", "GOWORK"}

func NewGoLanguageDriver(workdir string) *GoLanguageDriver {
	env := map[string]string{}
	for _, key := range requiredGoEnvVars {
		if v, ok := os.LookupEnv(key); ok {
			env[key] = v
		}
	}
	procEnv := &imports.ProcessEnv{
		GocmdRunner: &gocommand.Runner{},
		Env:         env,
		ModFlag:     "readonly",
		WorkingDir:  workdir,
	}
	res, err := procEnv.GetResolver()
	if err != nil || res == nil {
		slog.Warn("failed to create module resolver", "error", err)
		return nil
	}
	resolver := res.(*imports.ModuleResolver)
	modDir, modName := resolver.ModInfo(workdir)

	return &GoLanguageDriver{
		processEnv:     procEnv,
		moduleResolver: resolver,
		localModDir:    modDir,
		localModName:   modName,
	}
}

func (s *GoLanguageDriver) RefreshModules() {
	s.moduleResolver.ClearForNewScan()
}

func (s *GoLanguageDriver) HasGoModule() bool {
	if s == nil {
		return false
	}
	return s.localModDir != "" && s.localModName != "" && s.moduleResolver != nil
}

type ParsedGoFile struct {
	*goast.File
	Fset     *token.FileSet
	Filename string
}

func (f ParsedGoFile) Position(pos token.Pos) protocol.Position {
	position := f.Fset.Position(pos)
	return protocol.Position{
		Line:      uint32(position.Line) - 1,
		Character: uint32(position.Column) - 1,
	}
}

func (s *GoLanguageDriver) FindGeneratedFiles(uri protocol.DocumentURI, fileOpts *descriptorpb.FileOptions, matchSourcePath string) ([]ParsedGoFile, error) {
	pkgPath := fileOpts.GetGoPackage()
	if pkgPath == "" && uri.IsFile() {
		var err error
		pkgPath, err = s.ImplicitGoPackagePath(uri.Path())
		if err != nil {
			return nil, err
		}
	}
	var pkgNameAlias string
	if strings.Contains(pkgPath, ";") {
		// path/to/package;alias
		pkgPath, pkgNameAlias, _ = strings.Cut(pkgPath, ";")
	} else if uri.IsFile() && !strings.Contains(pkgPath, "/") && !strings.Contains(pkgPath, ".") {
		// alias only
		implicitPath, err := s.ImplicitGoPackagePath(uri.Path())
		if err != nil {
			return nil, err
		}
		pkgPath, pkgNameAlias = implicitPath, pkgPath
	}
	mod, dir := s.moduleResolver.FindPackage(pkgPath)
	if mod == nil {
		return nil, fmt.Errorf("no package found for %s", pkgPath)
	}
	fset := token.NewFileSet()
	pkgs, _ := goparser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		if strings.HasSuffix(fi.Name(), "_test.go") {
			return false
		}
		return strings.HasSuffix(fi.Name(), ".pb.go")
	}, goparser.ParseComments)
	var res []ParsedGoFile
	for _, pkg := range pkgs {
		if pkgNameAlias != "" && pkg.Name != pkgNameAlias {
			continue
		}
		for filename, f := range pkg.Files {
			preamble, ok := ParseGeneratedPreamble(f)
			if !ok {
				if matchSourcePath != "" {
					if matchSourcePath != preamble.Source {
						continue
					}
				} else {
					var uriPath string
					if uri.IsFile() {
						uriPath = uri.Path()
					} else {
						if u, err := url.Parse(string(uri)); err == nil {
							uriPath = u.Path
						} else {
							continue
						}
					}
					// if matchSourcePath is empty, check the base filename
					if filepath.Base(filename) != filepath.Base(uriPath) {
						continue
					}
				}
			}
			res = append(res, ParsedGoFile{
				File:     f,
				Fset:     fset,
				Filename: filename,
			})
		}
	}
	return res, nil
}

type GoModuleImportResults struct {
	Module       *gocommand.ModuleJSON
	DirInModule  string
	SourceExists bool
	SourcePath   string
	KnownAltPath string
}

func (s *GoLanguageDriver) ImportFromGoModule(importName string) (GoModuleImportResults, error) {
	last := strings.LastIndex(importName, "/")
	if last == -1 {
		return GoModuleImportResults{}, fmt.Errorf("%w: %s", os.ErrNotExist, "not a go import")
	}
	filename := importName[last+1:]
	if !strings.HasSuffix(filename, ".proto") {
		return GoModuleImportResults{}, fmt.Errorf("%w: %s", os.ErrNotExist, "not a .proto file")
	}

	// check if the path (excluding the filename) is a well-formed go module
	importPath := importName[:last]
	if err := module.CheckImportPath(importPath); err != nil {
		return GoModuleImportResults{}, fmt.Errorf("%w: %s", os.ErrNotExist, err)
	}

	var knownAltPath string
	pkgData, dir := s.moduleResolver.FindPackage(importPath)
	if pkgData == nil || dir == "" {
		for _, edits := range s.knownAlternativePackages {
			edited, err := diff.Apply(importPath, edits)
			if err == nil {
				pkgData, dir = s.moduleResolver.FindPackage(edited)
				if pkgData != nil && dir != "" {
					knownAltPath = path.Join(edited, filename)
					goto edit_success
				}
			}
		}
		return GoModuleImportResults{}, fmt.Errorf("%w: %s", os.ErrNotExist, "no packages found")
	}
edit_success:
	// We now have a valid go package. First check if there's a .proto file in the package.
	// If there is, we're done.
	if _, err := os.Stat(filepath.Join(dir, filename)); err == nil {
		// thank god
		return GoModuleImportResults{
			Module:       pkgData,
			DirInModule:  dir,
			SourceExists: true,
			SourcePath:   filepath.Join(dir, filename),
		}, nil
	}
	res := GoModuleImportResults{
		Module:       pkgData,
		DirInModule:  dir,
		SourceExists: false,

		// even if there is no source, this may have been edited from a known
		// alternative pattern, in which case we'll know how to resolve it later.
		KnownAltPath: knownAltPath,
	}
	return res, nil
}

func (s *GoLanguageDriver) ImplicitGoPackagePath(filename string) (string, error) {
	// check if there is a known go module at the path
	relativePath, err := filepath.Rel(s.localModDir, filename)
	if err != nil {
		return "", err
	}
	// it's in the same module, so we can use the module name
	return path.Join(s.localModName, path.Dir(relativePath)), nil
}

func (s *GoLanguageDriver) SynthesizeFromGoSource(importName string, res GoModuleImportResults) (desc *descriptorpb.FileDescriptorProto, _err error) {
	// buckle up
	fset := token.NewFileSet()
	packages, err := goparser.ParseDir(fset, res.DirInModule, func(fi fs.FileInfo) bool {
		if strings.HasSuffix(fi.Name(), "_test.go") {
			return false
		}
		return strings.HasSuffix(fi.Name(), ".pb.go") && !strings.HasSuffix(fi.Name(), "_grpc.pb.go")
	}, goparser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, err)
	}
	if len(packages) != 1 {
		return nil, fmt.Errorf("wrong number of packages found: %d", len(packages))
	}
	var rawDescByteArray *goast.Object
PACKAGES:
	for _, pkg := range packages {
		// we're looking for the byte array that contains the raw file descriptor
		// it's named "file_<filename>_rawDesc" where <filename> is the import path
		// used when compiling the generated code, with slashes replaced by underscores.
		// e.g. file_example_com_foo_bar_baz_proto_rawDesc => "example.com/foo/bar/baz.proto"
		// only one catch: the go package path is not necessarily the same as the import path.
		// luckily, there's a comment at the top of the file that tells us what the import path is.
		// it looks like "// source: example.com/foo/bar/baz.proto"
		for filename, f := range pkg.Files {
			// find the source file with the matching basename and the .pb.go extension
			if strings.TrimSuffix(path.Base(filename), ".pb.go") != strings.TrimSuffix(path.Base(importName), ".proto") {
				// generated from a different proto file
				continue
			}
			preamble, ok := ParseGeneratedPreamble(f)
			if !ok || preamble.Source == "" {
				continue
			}

			// found a possible match, check if there's a symbol with the right name
			symbolName := fmt.Sprintf("file_%s_rawDesc", strings.ReplaceAll(strings.ReplaceAll(preamble.Source, "/", "_"), ".", "_"))
			object := f.Scope.Lookup(symbolName)
			if object != nil && object.Kind == goast.Var {
				// found it!
				rawDescByteArray = object
				break PACKAGES
			}
		}
	}
	if rawDescByteArray == nil {
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, "could not find file descriptor in package")
	}
	// we have the raw descriptor byte array, which is just a bunch of hex numbers in a slice
	// which we can decode from the ast.
	// The ast for the byte array will look like:
	// *ast.Object {
	//   Kind: var
	//   Name: "file_<filename>_rawDesc"
	//   Decl: *ast.ValueSpec {
	//     Values: []ast.Expr (len = 1) {
	//       0: *ast.CompositeLit {
	//         Elts: []ast.Expr (len = {len}) {
	//           0: *ast.BasicLit {
	//             Value: "0x0a"
	//           }
	//           1: *ast.BasicLit {
	//             Value: "0x2c"
	//           }
	//           ...
	elements := rawDescByteArray.Decl.(*goast.ValueSpec).Values[0].(*goast.CompositeLit).Elts
	buf := bytes.NewBuffer(make([]byte, 0, 4096))
	for _, b := range elements {
		str := b.(*goast.BasicLit).Value
		i, err := strconv.ParseUint(str, 0, 8)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", os.ErrNotExist, err)
		}
		buf.WriteByte(byte(i))
	}

	// now we have a byte array containing the raw file descriptor, which we can unmarshal
	// into a FileDescriptorProto.
	// the buffer may or may not be gzipped, so we need to check that first.
	fd, err := DecodeRawFileDescriptor(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, err)
	}
	if fd.GetName() != importName {
		// this package uses an alternate import path. we need to keep track of this
		// in case any of its dependencies use a similar path structure.
		alternateImportPath := fd.GetName()
		resolvedImportPath := importName
		edits := diff.Strings(alternateImportPath, resolvedImportPath)
		s.knownAlternativePackages = append(s.knownAlternativePackages, edits)
	}
	return fd, nil
}

func DecodeRawFileDescriptor(data []byte) (*descriptorpb.FileDescriptorProto, error) {
	var reader io.Reader = bytes.NewReader(data)
	if bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
		var err error
		reader, err = gzip.NewReader(reader)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", os.ErrNotExist, err)
		}
	}
	decompressedBytes, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, err)
	}

	fd := &descriptorpb.FileDescriptorProto{}
	if err := proto.Unmarshal(decompressedBytes, fd); err != nil {
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, err)
	}

	return fd, nil
}

type PreambleInfo struct {
	GeneratedBy string
	Versions    map[string]string
	Source      string
}

var (
	doNotEditRegex    = regexp.MustCompile(`^// Code generated(?:\s*by\s+(.+)\.)?.*DO NOT EDIT\.?$`)
	versionEntryRegex = regexp.MustCompile(`^// [\s-]+([^\s]+)\s+([^\s]+)$`)
)

func ParseGeneratedPreamble(f *goast.File) (PreambleInfo, bool) {
	var info PreambleInfo
	var group *goast.CommentGroup
COMMENTS:
	for _, cg := range f.Comments {
		for _, comment := range cg.List {
			if comment.Pos() > f.Package {
				break
			}
			matches := doNotEditRegex.FindStringSubmatch(comment.Text)
			if len(matches) == 0 {
				continue
			}
			if len(matches) > 1 {
				info.GeneratedBy = matches[1]
			}
			group = cg
			break COMMENTS
		}
	}
	if group == nil {
		return PreambleInfo{}, false
	}
	if len(group.List) == 1 {
		return info, true
	}
	for i, comment := range group.List[1:] {
		switch {
		case strings.HasPrefix(comment.Text, "// versions:"):
			for j := i + 1; j < len(group.List); j++ {
				matches := versionEntryRegex.FindStringSubmatch(group.List[j].Text)
				if len(matches) != 3 {
					break
				}
				if info.Versions == nil {
					info.Versions = make(map[string]string)
				}
				info.Versions[matches[1]] = matches[2]
			}
		case strings.HasPrefix(comment.Text, "// source:"):
			info.Source = strings.TrimSpace(strings.TrimPrefix(comment.Text, "// source:"))
		}
	}
	return info, true
}

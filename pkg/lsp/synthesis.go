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
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.uber.org/zap"
	"golang.org/x/mod/module"
	"golang.org/x/tools/pkg/diff"
	"golang.org/x/tools/pkg/gocommand"
	"golang.org/x/tools/pkg/imports"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// creates proto files out of thin air
type ProtoSourceSynthesizer struct {
	processEnv                *imports.ProcessEnv
	moduleResolver            *imports.ModuleResolver
	knownAlternativePackages  [][]diff.Edit
	localModDir, localModName string
}

func NewProtoSourceSynthesizer(workdir string) *ProtoSourceSynthesizer {
	env := map[string]string{}
	for _, key := range requiredGoEnvVars {
		if v, ok := os.LookupEnv(key); ok {
			env[key] = v
		}
	}
	procEnv := &imports.ProcessEnv{
		GocmdRunner: &gocommand.Runner{},
		Env:         env,
		ModFile:     filepath.Join(workdir, "go.mod"),
		ModFlag:     "readonly",
		WorkingDir:  workdir,
		Logf:        zap.S().Debugf,
	}
	res, err := procEnv.GetResolver()
	if err != nil {
		panic(err)
	}
	resolver := res.(*imports.ModuleResolver)
	resolver.ClearForNewMod()

	modDir, modName := resolver.ModInfo(workdir)

	return &ProtoSourceSynthesizer{
		processEnv:     procEnv,
		moduleResolver: resolver,
		localModDir:    modDir,
		localModName:   modName,
	}
}

func (s *ProtoSourceSynthesizer) ImportFromGoModule(importName string) (_filePath string, _pkgData *gocommand.ModuleJSON, _err error) {
	// fmt.Println("tryGoImport", importName)
	// defer func() { fmt.Println("tryGoImport done", _filePath, _err) }()

	last := strings.LastIndex(importName, "/")
	if last == -1 {
		return "", nil, fmt.Errorf("%w: %s", os.ErrNotExist, "not a go import")
	}
	filename := importName[last+1:]
	if !strings.HasSuffix(filename, ".proto") {
		return "", nil, fmt.Errorf("%w: %s", os.ErrNotExist, "not a .proto file")
	}

	// check if the path (excluding the filename) is a well-formed go module
	importPath := importName[:last]
	if err := module.CheckImportPath(importPath); err != nil {
		return "", nil, fmt.Errorf("%w: %s", os.ErrNotExist, err)
	}

	pkgData, dir := s.moduleResolver.FindPackage(importPath)
	if pkgData == nil || dir == "" {
		for _, edits := range s.knownAlternativePackages {
			edited, err := diff.Apply(importPath, edits)
			// fmt.Printf("tryGoImport > %q not found, trying %q instead based on previously detected patterns\n", importPath, edited)
			if err == nil {
				pkgData, dir = s.moduleResolver.FindPackage(edited)
				if pkgData != nil && dir != "" {
					// fmt.Println("tryGoImport > successfully found", edited)
					goto edit_success
				}
			}
		}
		return "", nil, fmt.Errorf("%w: %s", os.ErrNotExist, "no packages found")
	}
edit_success:
	// fmt.Println("tryGoImport > pkgData", pkgData)

	// We now have a valid go package. First check if there's a .proto file in the package.
	// If there is, we're done.
	if _, err := os.Stat(filepath.Join(dir, filename)); err == nil {
		// thank god
		return filepath.Join(dir, filename), pkgData, nil
	}
	return "", pkgData, fmt.Errorf("%w: %s", os.ErrNotExist, "no .proto file found")
}

func (s *ProtoSourceSynthesizer) SynthesizeFromGoSource(importName string, pkgData *gocommand.ModuleJSON) (desc *descriptorpb.FileDescriptorProto, _err error) {
	// buckle up
	fset := token.NewFileSet()
	packages, err := goparser.ParseDir(fset, pkgData.Dir, func(fi fs.FileInfo) bool {
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
	// fmt.Println(">> [OK] found packages:", packages)
PACKAGES:
	for _, pkg := range packages {
		// we're looking for the byte array that contains the raw file descriptor
		// it's named "file_<filename>_rawDesc" where <filename> is the import path
		// used when compiling the generated code, with slashes replaced by underscores.
		// e.g. file_example_com_foo_bar_baz_proto_rawDesc => "example.com/foo/bar/baz.proto"
		// only one catch: the go package path is not necessarily the same as the import path.
		// luckily, there's a comment at the top of the file that tells us what the import path is.
		// it looks like "// source: example.com/foo/bar/baz.proto"
		for _, f := range pkg.Files {
			for _, comment := range f.Comments {
				text := comment.Text()
				_, path, ok := strings.Cut(text, "source: ")
				path = strings.TrimSpace(path)
				if !ok || !strings.HasSuffix(path, ".proto") {
					continue
				}

				// found a possible match, check if there's a symbol with the right name
				symbolName := fmt.Sprintf("file_%s_rawDesc", strings.ReplaceAll(strings.ReplaceAll(path, "/", "_"), ".", "_"))
				object := f.Scope.Lookup(symbolName)
				if object != nil && object.Kind == goast.Var {
					// found it!
					rawDescByteArray = object
					break PACKAGES
				}
			}
		}
	}
	if rawDescByteArray == nil {
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, "could not find file descriptor in package")
	}
	// fmt.Println(">> [OK] found ast object")
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
	// fmt.Println(">> [OK] decoded byte array")

	// now we have a byte array containing the raw file descriptor, which we can unmarshal
	// into a FileDescriptorProto.
	// the buffer may or may not be gzipped, so we need to check that first.
	var reader io.Reader = buf
	if bytes.HasPrefix(buf.Bytes(), []byte{0x1f, 0x8b}) {
		reader, err = gzip.NewReader(buf)
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
	// fmt.Println(">> [OK] decoded raw file descriptor")
	if fd.GetName() != importName {
		// this package uses an alternate import path. we need to keep track of this
		// in case any of its dependencies use a similar path structure.
		alternateImportPath := fd.GetName()
		resolvedImportPath := importName
		edits := diff.Strings(alternateImportPath, resolvedImportPath)
		s.knownAlternativePackages = append(s.knownAlternativePackages, edits)

		*fd.Name = importName
	}
	return fd, nil
}

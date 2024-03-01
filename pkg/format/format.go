package format

import (
	"bytes"
	"io"
	"os"

	"github.com/kralicky/protocompile/ast"
	"github.com/kralicky/protocompile/parser"
	"github.com/kralicky/protocompile/reporter"
	"github.com/kralicky/protols/pkg/format/protoprint"
	"github.com/kralicky/protols/pkg/util"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func Format(in io.Reader, out io.Writer) error {
	a, err := parser.Parse("", in, reporter.NewHandler(reporter.NewReporter(
		func(err reporter.ErrorWithPos) error {
			return err
		},
		func(err reporter.ErrorWithPos) {},
	)), 0)
	if err != nil {
		return err
	}
	formatter := NewFormatter(out, a)
	return formatter.Run()
}

func File(filename string, out io.Writer) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	return Format(f, out)
}

func FileInPlace(filename string) error {
	info, err := os.Stat(filename)
	if err != nil {
		return err
	}
	original, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	var formatted bytes.Buffer
	if err := Format(bytes.NewReader(original), &formatted); err != nil {
		return err
	}
	return util.OverwriteFile(filename, original, formatted.Bytes(), info.Mode().Perm(), info.Size())
}

func PrintDescriptor(d protoreflect.Descriptor) (string, error) {
	printer := protoprint.Printer{
		CustomSortFunction: SortElements,
		Indent:             "  ",
		Compact:            protoprint.CompactDefault | protoprint.CompactTopLevelDeclarations,
	}
	str, err := printer.PrintProtoToString(d)
	if err != nil {
		return "", err
	}
	return str, nil
}

func NewDefaultPrinter() *protoprint.Printer {
	return &protoprint.Printer{
		CustomSortFunction: SortElements,
		Indent:             "  ",
		Compact:            protoprint.CompactDefault | protoprint.CompactTopLevelDeclarations,
	}
}

func PrintAndFormatFileDescriptor(fd protoreflect.FileDescriptor, out io.Writer) error {
	printer := NewDefaultPrinter()
	var buf bytes.Buffer
	err := printer.PrintProto(fd, &buf)
	if err != nil {
		return err
	}
	return Format(&buf, out)
}

func PrintNode(fileNode FileNodeInterface, node ast.Node) (string, error) {
	if fileNode == nil {
		fileNode = ast.NewEmptyFileNode("", 0)
	}
	if ast.IsNil(node) {
		return "", nil
	}
	var writer bytes.Buffer

	f := NewFormatter(&writer, fileNode)
	switch node := node.(type) {
	case *ast.FileNode:
		if err := f.Run(); err != nil {
			return "", err
		}
	default:
		f.writeNode(node)
	}
	str := writer.String()
	if len(str) > 0 {
		lower, upper := 0, len(str)
		if str[0] == '\n' {
			lower = 1
		}
		if str[len(str)-1] == '\n' {
			upper = len(str) - 1
		}
		str = str[lower:upper]
	}
	return str, nil
}

func NodeInfoOverlay(fileNode FileNodeInterface, infos map[ast.Node]ast.NodeInfo) FileNodeInterface {
	for node := range infos {
		ast.Inspect(node, func(cn ast.Node) bool {
			if cn == node {
				return true
			}
			if terminalNode, ok := cn.(ast.TerminalNode); ok && terminalNode.GetToken() == 0 {
				if _, ok := infos[cn]; !ok {
					infos[cn] = ast.NodeInfo{}
				}
			}
			return true
		})
	}
	return &fileNodeInfoOverlay{
		FileNodeInterface: fileNode,
		infos:             infos,
	}
}

type fileNodeInfoOverlay struct {
	FileNodeInterface

	infos map[ast.Node]ast.NodeInfo
}

func (f *fileNodeInfoOverlay) NodeInfo(node ast.Node) ast.NodeInfo {
	if info, ok := f.infos[node]; ok {
		return info
	}
	return f.FileNodeInterface.NodeInfo(node)
}

package format

import (
	"bytes"
	"io"
	"os"

	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/desc/protoprint"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func Format(in io.Reader, out io.Writer) error {
	a, err := parser.Parse("", in, reporter.NewHandler(reporter.NewReporter(
		func(err reporter.ErrorWithPos) error {
			return err
		},
		func(err reporter.ErrorWithPos) {},
	)))
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
	return writeFile(filename, original, formatted.Bytes(), info.Mode().Perm(), info.Size())
}

func PrintDescriptor(d protoreflect.Descriptor) (string, error) {
	wrap, err := desc.WrapDescriptor(d)
	if err != nil {
		return "", err
	}
	printer := protoprint.Printer{
		CustomSortFunction: SortElements,
		Indent:             "  ",
		Compact:            protoprint.CompactDefault,
	}
	str, err := printer.PrintProtoToString(wrap)
	if err != nil {
		return "", err
	}
	return str, nil
}

func NewDefaultPrinter() *protoprint.Printer {
	return &protoprint.Printer{
		CustomSortFunction: SortElements,
		Indent:             "  ",
		Compact:            protoprint.CompactDefault,
	}
}

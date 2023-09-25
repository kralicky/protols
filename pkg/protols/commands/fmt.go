package commands

import (
	"bytes"
	"os"

	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
	"github.com/kralicky/protols/pkg/format"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

// FmtCmd represents the fmt command
func BuildFmtCmd() *cobra.Command {
	var write bool
	cmd := &cobra.Command{
		Use:   "fmt [filenames...]",
		Short: "Format proto source files",
		RunE: func(cmd *cobra.Command, args []string) error {
			var eg errgroup.Group
			for _, filename := range args {
				filename := filename
				eg.Go(func() error {

					info, err := os.Stat(filename)
					if err != nil {
						return err
					}

					original, err := os.ReadFile(filename)
					if err != nil {
						return err
					}
					a, err := parser.Parse(filename, bytes.NewReader(original), reporter.NewHandler(reporter.NewReporter(
						func(err reporter.ErrorWithPos) error {
							cmd.PrintErrln(err.Error())
							return nil
						},
						func(err reporter.ErrorWithPos) {
							cmd.PrintErrln(err.Error())
						},
					)))
					if err != nil {
						return err
					}
					formatted := new(bytes.Buffer)
					formatter := format.NewFormatter(formatted, a)
					if err := formatter.Run(); err != nil {
						return err
					}

					if write {
						return writeFile(filename, original, formatted.Bytes(), info.Mode().Perm(), info.Size())
					}
					_, err = cmd.OutOrStdout().Write(formatted.Bytes())
					return err
				})
			}
			return eg.Wait()
		},
	}
	cmd.Flags().BoolVarP(&write, "write", "w", false, "write result to (source) file instead of stdout")
	return cmd
}

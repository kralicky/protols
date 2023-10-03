package commands

import (
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
					return format.FileInPlace(filename)
				})
			}
			return eg.Wait()
		},
	}
	cmd.Flags().BoolVarP(&write, "write", "w", false, "write result to (source) file instead of stdout")
	return cmd
}

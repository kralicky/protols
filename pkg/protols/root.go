package protols

import (
	"os"

	//+cobra:commandsImport
	"github.com/kralicky/protols/pkg/protols/commands"
	"github.com/spf13/cobra"
)

// rootCmd represents the base command when called without any subcommands
func BuildRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "protols",
		Short: "Protobuf Language Server",
	}

	//+cobra:subcommands
	rootCmd.AddCommand(commands.BuildServeCmd())

	return rootCmd
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := BuildRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

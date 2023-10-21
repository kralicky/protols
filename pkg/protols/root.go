package protols

import (
	"os"

	"github.com/kralicky/protols/pkg/protols/commands"
	"github.com/spf13/cobra"
)

// rootCmd represents the base command when called without any subcommands
func BuildRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "protols",
		Short: "Protobuf Language Server",
	}

	rootCmd.AddCommand(commands.BuildFmtCmd())
	rootCmd.AddCommand(commands.BuildServeCmd())
	rootCmd.AddCommand(commands.BuildVetCmd())
	rootCmd.AddCommand(commands.BuildDecodeCmd())
	//+cobra:subcommands

	return rootCmd
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := BuildRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

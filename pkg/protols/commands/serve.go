package commands

import (
	"fmt"
	"net"
	"os"

	"github.com/kralicky/protols/pkg/lsp"
	"go.uber.org/zap"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"

	"golang.org/x/tools/pkg/jsonrpc2"

	"github.com/spf13/cobra"
)

// ServeCmd represents the serve command
func BuildServeCmd() *cobra.Command {
	var pipe string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the language server",
		Run: func(cmd *cobra.Command, args []string) {
			lg, _ := zap.NewDevelopment()

			server := lsp.NewServer(lg)

			cc, err := net.Dial("unix", pipe)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %s\n", err.Error())
				os.Exit(1)
			}
			lg.Info("listening on " + pipe)
			stream := jsonrpc2.NewHeaderStream(cc)
			stream = protocol.LoggingStream(stream, os.Stdout)
			conn := jsonrpc2.NewConn(stream)
			client := protocol.ClientDispatcher(conn)
			ctx := protocol.WithClient(cmd.Context(), client)
			conn.Go(ctx,
				protocol.Handlers(
					protocol.ServerHandler(server,
						jsonrpc2.MethodNotFound)))

			<-conn.Done()
			if err := conn.Err(); err != nil {
				fmt.Fprintf(os.Stderr, "server exited with error: %s\n", err.Error())
				os.Exit(1)
			}
		},
	}

	cmd.Flags().StringVar(&pipe, "pipe", "", "socket name to listen on")
	cmd.MarkFlagRequired("pipe")

	return cmd
}

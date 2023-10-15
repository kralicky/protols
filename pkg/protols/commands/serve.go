package commands

import (
	"context"
	"fmt"
	"net"
	"os"

	"log/slog"

	"github.com/kralicky/protols/pkg/lsp"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"

	"golang.org/x/tools/pkg/event"
	"golang.org/x/tools/pkg/jsonrpc2"

	"github.com/spf13/cobra"
)

// ServeCmd represents the serve command
func BuildServeCmd() *cobra.Command {
	var pipe string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the language server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cc, err := net.Dial("unix", pipe)
			if err != nil {
				return err
			}
			stream := jsonrpc2.NewHeaderStream(cc)
			stream = protocol.LoggingStream(stream, os.Stdout)
			conn := jsonrpc2.NewConn(stream)
			client := protocol.ClientDispatcher(conn)
			// r, w := io.Pipe()
			// go func() {
			// 	scan := bufio.NewScanner(r)
			// 	for scan.Scan() {
			// 		log := scan.Bytes()
			// 		client.LogMessage(context.Background(), &protocol.LogMessageParams{
			// 			Type:    protocol.Log,
			// 			Message: string(log),
			// 		})
			// 	}
			// }()
			slog.SetDefault(slog.New(slog.NewTextHandler(cmd.OutOrStderr(), &slog.HandlerOptions{
				AddSource: true,
				Level:     slog.LevelDebug,
			})))

			ctx := protocol.WithClient(cmd.Context(), client)
			server := lsp.NewServer(client)
			conn.Go(ctx, protocol.CancelHandler(
				AsyncHandler(
					jsonrpc2.MustReplyHandler(
						protocol.ServerHandler(server, jsonrpc2.MethodNotFound)))))

			<-conn.Done()
			if err := conn.Err(); err != nil {
				return fmt.Errorf("server exited with error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&pipe, "pipe", "", "socket name to listen on")
	cmd.MarkFlagRequired("pipe")

	return cmd
}

// methods that are intended to be long-lived, and should not hold up the queue
var streamingRequestMethods = map[string]bool{
	"workspace/diagnostic": true,
}

func AsyncHandler(handler jsonrpc2.Handler) jsonrpc2.Handler {
	nextRequest := make(chan struct{})
	close(nextRequest)
	return func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		waitForPrevious := nextRequest
		nextRequest = make(chan struct{})
		unlockNext := nextRequest
		if streamingRequestMethods[req.Method()] {
			close(unlockNext)
		} else {
			innerReply := reply
			reply = func(ctx context.Context, result interface{}, err error) error {
				close(unlockNext)
				return innerReply(ctx, result, err)
			}
		}
		_, queueDone := event.Start(ctx, "queued")
		go func() {
			<-waitForPrevious
			queueDone()
			if err := handler(ctx, reply, req); err != nil {
				event.Error(ctx, "jsonrpc2 async message delivery failed", err)
			}
		}()
		return nil
	}
}

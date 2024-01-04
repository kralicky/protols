package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"

	"github.com/kralicky/protols/pkg/lsp"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"

	"github.com/kralicky/tools-lite/pkg/event"
	"github.com/kralicky/tools-lite/pkg/event/core"
	"github.com/kralicky/tools-lite/pkg/event/keys"
	"github.com/kralicky/tools-lite/pkg/event/label"
	"github.com/kralicky/tools-lite/pkg/jsonrpc2"

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

			slog.SetDefault(slog.New(slog.NewTextHandler(cmd.OutOrStderr(), &slog.HandlerOptions{
				AddSource: true,
				Level:     slog.LevelDebug,
			})))

			var eventMu sync.Mutex
			event.SetExporter(func(ctx context.Context, e core.Event, lm label.Map) context.Context {
				eventMu.Lock()
				defer eventMu.Unlock()
				if event.IsError(e) {
					if err := keys.Err.Get(e); errors.Is(err, context.Canceled) {
						return ctx
					}
					var args []any
					for i := 0; e.Valid(i); i++ {
						l := e.Label(i)
						if !l.Valid() || l.Key() == keys.Msg {
							continue
						}
						key := l.Key()
						var val bytes.Buffer
						key.Format(&val, nil, l)
						args = append(args, l.Key().Name(), val.String())
					}
					slog.Error(keys.Msg.Get(e), args...)
				}
				return ctx
			})

			server := lsp.NewServer(client)
			conn.Go(cmd.Context(), protocol.CancelHandler(
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

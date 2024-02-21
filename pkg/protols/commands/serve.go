package commands

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"

	"github.com/kralicky/protols/pkg/lsprpc"
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

			cc, err := net.Dial("unix", pipe)
			if err != nil {
				return err
			}
			stream := jsonrpc2.NewHeaderStream(cc)
			conn := jsonrpc2.NewConn(stream)
			ss := lsprpc.NewStreamServer()
			return ss.ServeStream(cmd.Context(), conn)
		},
	}

	cmd.Flags().StringVar(&pipe, "pipe", "", "socket name to listen on")
	cmd.MarkFlagRequired("pipe")

	return cmd
}

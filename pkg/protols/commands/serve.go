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
	"github.com/kralicky/protols/pkg/lsprpc"
	"github.com/kralicky/protols/pkg/version"
	"github.com/kralicky/tools-lite/pkg/event"
	"github.com/kralicky/tools-lite/pkg/event/core"
	"github.com/kralicky/tools-lite/pkg/event/keys"
	"github.com/kralicky/tools-lite/pkg/event/label"
	"github.com/kralicky/tools-lite/pkg/fakenet"
	"github.com/kralicky/tools-lite/pkg/jsonrpc2"
	"github.com/spf13/cobra"
)

func BuildServeCmd() *cobra.Command {
	var stdio bool
	var pipe string
	var defaultLogLevel string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the language server",
		RunE: func(cmd *cobra.Command, args []string) error {
			slog.SetDefault(slog.New(slog.NewTextHandler(cmd.OutOrStderr(), &slog.HandlerOptions{
				AddSource: true,
				Level:     lsp.GlobalAtomicLeveler,
			})))
			slog.With("version", version.FriendlyVersion()).Info("Starting protols")
			if defaultLogLevel != "" {
				level, ok := lsp.ParseLogLevel(defaultLogLevel)
				if ok {
					lsp.GlobalAtomicLeveler.SetLevel(level)
				} else {
					return fmt.Errorf("invalid log level '%s' (expecting 'debug', 'info', 'warning', or 'error')", defaultLogLevel)
				}
			}
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

			var cc net.Conn
			if stdio {
				cc = fakenet.NewConn("stdio", os.Stdin, os.Stdout)
			} else {
				var err error
				cc, err = net.Dial("unix", pipe)
				if err != nil {
					return err
				}
			}
			stream := jsonrpc2.NewHeaderStream(cc)
			conn := jsonrpc2.NewConn(stream)
			ss := lsprpc.NewStreamServer()
			return ss.ServeStream(cmd.Context(), conn)
		},
	}

	cmd.Flags().StringVar(&defaultLogLevel, "default-log-level", "", "default log level (if not controlled by lsp client)")
	cmd.Flags().BoolVar(&stdio, "stdio", false, "communicate over stdin/stdout")
	cmd.Flags().StringVar(&pipe, "pipe", "", "socket name to listen on")
	cmd.MarkFlagsOneRequired("stdio", "pipe")
	cmd.MarkFlagsMutuallyExclusive("stdio", "pipe")

	return cmd
}

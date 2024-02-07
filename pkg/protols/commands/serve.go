package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/kralicky/protocompile/linker"
	"github.com/kralicky/protols/pkg/lsp"
	"github.com/kralicky/protols/pkg/util"
	"github.com/kralicky/protols/sdk/codegen"
	"github.com/kralicky/protols/sdk/plugin"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/types/descriptorpb"

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

			server := lsp.NewServer(client,
				lsp.WithUnknownCommandHandler(
					&unknownHandler{Generators: codegen.DefaultGenerators()},
					"protols/generate",
					"protols/generateWorkspace",
				),
			)
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
	"workspace/diagnostic":     true,
	"workspace/executeCommand": true,
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

type unknownHandler struct {
	Generators []codegen.Generator
}

// Execute implements lsp.UnknownCommandHandler.
func (h *unknownHandler) Execute(ctx context.Context, uc lsp.UnknownCommand) (any, error) {
	switch uc.Command {
	case "protols/generate":
		var req lsp.GenerateCodeRequest
		if err := json.Unmarshal(uc.Arguments[0], &req); err != nil {
			return nil, err
		}
		if uc.Cache == nil {
			return nil, errors.New("no cache available")
		}
		return nil, h.doGenerate(ctx, uc.Cache, req.URIs)
	case "protols/generateWorkspace":
		if uc.Cache == nil {
			return nil, errors.New("no cache available")
		}
		return nil, h.doGenerate(ctx, uc.Cache, uc.Cache.XListWorkspaceLocalURIs())
	default:
		panic("unknown command: " + uc.Command)
	}
}

var _ lsp.UnknownCommandHandler = (*unknownHandler)(nil)

func (h *unknownHandler) doGenerate(ctx context.Context, cache *lsp.Cache, uris []protocol.DocumentURI) error {
	pathMappings := cache.XGetURIPathMappings()
	roots := make(linker.Files, 0, len(uris))
	outputDirs := map[string]string{}
	for _, uri := range uris {
		res, err := cache.FindResultByURI(uri)
		if err != nil {
			return err
		}
		if res.Package() == "" {
			continue
		}
		if _, ok := res.AST().Pragma(lsp.PragmaNoGenerate); ok {
			continue
		}
		roots = append(roots, res)
		p := pathMappings.FilePathsByURI[uri]
		outputDirs[path.Dir(p)] = path.Dir(uri.Path())
		if opts := res.Options(); opts.ProtoReflect().IsValid() {
			if goPkg := opts.(*descriptorpb.FileOptions).GoPackage; goPkg != nil {
				// if the file has a different go_package than the implicit one, add
				// it to the output dirs map as well
				outputDirs[strings.Split(*goPkg, ";")[0]] = path.Dir(uri.Path())
			}
		}
	}
	closure := linker.ComputeReflexiveTransitiveClosure(roots)
	closureResults := make([]linker.Result, len(closure))
	for i, res := range closure {
		closureResults[i] = res.(linker.Result)
	}

	plugin, err := plugin.New(roots, closureResults, pathMappings)
	if err != nil {
		return err
	}
	for _, g := range h.Generators {
		if err := g.Generate(plugin); err != nil {
			return err
		}
	}
	response := plugin.Response()
	if response.Error != nil {
		return errors.New(response.GetError())
	}
	var errs error
	for _, rf := range response.GetFile() {
		dir, ok := outputDirs[path.Dir(rf.GetName())]
		if !ok {
			errs = errors.Join(errs, fmt.Errorf("cannot write outside of workspace module: %s", rf.GetName()))
			continue
		}
		absPath := path.Join(dir, path.Base(rf.GetName()))
		if info, err := os.Stat(absPath); err == nil {
			original, err := os.ReadFile(absPath)
			if err != nil {
				return err
			}
			updated := rf.GetContent()
			if err := util.OverwriteFile(absPath, original, []byte(updated), info.Mode().Perm(), info.Size()); err != nil {
				return err
			}
		} else {
			if err := os.WriteFile(absPath, []byte(rf.GetContent()), 0o644); err != nil {
				return err
			}
		}
	}
	return errs
}

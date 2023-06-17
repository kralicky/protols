package main

import (
	"context"
	"fmt"
	"net"
	"os"

	flag "github.com/spf13/pflag"
	"go.uber.org/zap"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"

	"golang.org/x/tools/pkg/jsonrpc2"

	_ "google.golang.org/genproto/googleapis/api/annotations"
	_ "google.golang.org/genproto/googleapis/rpc/code"
	_ "google.golang.org/genproto/googleapis/rpc/context"
	_ "google.golang.org/genproto/googleapis/rpc/context/attribute_context"
	_ "google.golang.org/genproto/googleapis/rpc/errdetails"
	_ "google.golang.org/genproto/googleapis/rpc/http"
	_ "google.golang.org/genproto/googleapis/rpc/status"
)

func main() {
	fmt.Println(os.Args)
	var pipe string
	flag.StringVar(&pipe, "pipe", "", "socket name to listen on")
	flag.Parse()

	lg, _ := zap.NewDevelopment()
	ctx := context.Background()

	server := NewServer(lg)

	cc, err := net.Dial("unix", pipe)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err.Error())
		os.Exit(1)
	}
	stream := jsonrpc2.NewHeaderStream(cc)
	stream = protocol.LoggingStream(stream, os.Stdout)
	conn := jsonrpc2.NewConn(stream)
	client := protocol.ClientDispatcher(conn)
	ctx = protocol.WithClient(ctx, client)
	conn.Go(ctx,
		protocol.Handlers(
			protocol.ServerHandler(server,
				jsonrpc2.MethodNotFound)))

	<-conn.Done()
	if err := conn.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "server exited with error: %s\n", err.Error())
		os.Exit(1)
	}
}

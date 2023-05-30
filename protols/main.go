package main

import (
	"context"
	"fmt"
	"net"
	"os"

	flag "github.com/spf13/pflag"
	"go.lsp.dev/jsonrpc2"
	protocol "go.lsp.dev/protocol"
	"go.uber.org/zap"
)

func main() {
	fmt.Println(os.Args)
	var sourcePatterns []string
	var pipe string
	flag.StringVar(&pipe, "pipe", "", "socket name to listen on")
	flag.StringSliceVar(&sourcePatterns, "source-patterns", nil, "source file patterns to watch")
	flag.Parse()

	lg, _ := zap.NewDevelopment()
	ctx := context.Background()
	ctx = protocol.WithLogger(ctx, lg)

	server := NewServer(lg)

	cc, err := net.Dial("unix", pipe)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err.Error())
		os.Exit(1)
	}

	stream := jsonrpc2.NewStream(cc)
	conn := jsonrpc2.NewConn(stream)

	ss := jsonrpc2.HandlerServer(protocol.ServerHandler(server, jsonrpc2.MethodNotFoundHandler))
	err = ss.ServeStream(ctx, conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err.Error())
		os.Exit(1)
	}
}

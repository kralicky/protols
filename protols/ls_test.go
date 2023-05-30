package main_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"

	"go.lsp.dev/jsonrpc2"
	protocol "go.lsp.dev/protocol"
	"go.uber.org/zap"
)

func TestCompletion(t *testing.T) {
	testDir, err := filepath.Abs("../../testdata")
	if err != nil {
		t.Fatal(err)
	}

	// assume existing server running on localhost:3333
	cc, err := net.Dial("tcp", "localhost:3333")
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	stream := jsonrpc2.NewStream(cc)
	defer stream.Close()
	conn := jsonrpc2.NewConn(stream)
	defer conn.Close()

	lg, _ := zap.NewDevelopment()
	ctx := context.Background()
	ctx = protocol.WithLogger(ctx, lg)

	// create client
	client := protocol.ClientDispatcher(conn, lg)
	ctx = protocol.WithClient(ctx, client)

	var initResp protocol.InitializeResult
	_, err = conn.Call(ctx, protocol.MethodInitialize, &protocol.InitializeParams{
		RootURI: protocol.URI(testDir),
	}, &initResp)
	if err != nil {
		t.Fatal(err)
	}
}

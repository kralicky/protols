package test

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kralicky/protols/pkg/lsprpc"
	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
	"github.com/kralicky/tools-lite/gopls/pkg/test/integration"
	"github.com/kralicky/tools-lite/gopls/pkg/test/integration/fake"
	"github.com/kralicky/tools-lite/pkg/jsonrpc2"
	"github.com/kralicky/tools-lite/pkg/jsonrpc2/servertest"
	"github.com/kralicky/tools-lite/pkg/testenv"
)

var runner *Runner

var (
	printLogs                = flag.Bool("print-logs", false, "whether to print LSP logs")
	printGoroutinesOnFailure = flag.Bool("print-goroutines", false, "whether to print goroutines info on failure")
	skipCleanup              = flag.Bool("skip-cleanup", false, "whether to skip cleaning up temp directories")
)

func Main(m *testing.M) {
	dir, err := os.MkdirTemp("", "protols-test-")
	if err != nil {
		panic(fmt.Errorf("creating temp directory: %v", err))
	}
	flag.Parse()

	runner = &Runner{
		SkipCleanup: *skipCleanup,
		tempDir:     dir,
	}
	var code int
	defer func() {
		if err := runner.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "closing test runner: %v\n", err)
			os.Exit(1)
		}
		os.Exit(code)
	}()
	code = m.Run()
}

func Run(t *testing.T, files string, f TestFunc) {
	runner.Run(t, files, f)
}

type Runner struct {
	SkipCleanup bool

	tempDir string
	tsOnce  sync.Once
	ts      *servertest.TCPServer
}

type (
	TestFunc  func(t *testing.T, env *integration.Env)
	runConfig struct {
		editor  fake.EditorConfig
		sandbox fake.SandboxConfig
	}
)

func defaultConfig() runConfig {
	return runConfig{
		editor: fake.EditorConfig{
			ClientName: "gotest",
			FileAssociations: map[string]string{
				"protobuf": `.*\.proto$`,
			},
		},
	}
}

// Run executes the test function in the default configured gopls execution
// modes. For each a test run, a new workspace is created containing the
// un-txtared files specified by filedata.
func (r *Runner) Run(t *testing.T, files string, test TestFunc, opts ...integration.RunOption) {
	// TODO(rfindley): this function has gotten overly complicated, and warrants
	// refactoring.
	t.Helper()

	config := defaultConfig()
	t.Run("in-process", func(t *testing.T) {
		ctx := context.Background()
		if d, ok := testenv.Deadline(t); ok {
			timeout := time.Until(d) * 19 / 20 // Leave an arbitrary 5% for cleanup.
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		rootDir := filepath.Join(r.tempDir, filepath.FromSlash(t.Name()))
		if err := os.MkdirAll(rootDir, 0o755); err != nil {
			t.Fatal(err)
		}
		files := fake.UnpackTxt(files)
		config.sandbox.Files = files
		config.sandbox.RootDir = rootDir
		sandbox, err := fake.NewSandbox(&config.sandbox)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if !r.SkipCleanup {
				if err := sandbox.Close(); err != nil {
					t.Errorf("closing the sandbox: %v", err)
				}
			}
		}()
		ss := lsprpc.NewStreamServer()
		r.ts = servertest.NewTCPServer(ctx, ss, nil)

		framer := jsonrpc2.NewRawStream
		ls := &loggingFramer{}
		framer = ls.framer(jsonrpc2.NewRawStream)
		ts := servertest.NewPipeServer(ss, framer)
		awaiter := integration.NewAwaiter(sandbox.Workdir)
		const skipApplyEdits = false
		editor, err := fake.NewEditor(sandbox, config.editor).Connect(ctx, ts, awaiter.Hooks(), skipApplyEdits)
		if err != nil {
			t.Fatal(err)
		}
		env := &integration.Env{
			T:       t,
			Ctx:     ctx,
			Sandbox: sandbox,
			Editor:  editor,
			Server:  ts,
			Awaiter: awaiter,
		}
		defer func() {
			if t.Failed() {
				ls.printBuffers(t.Name(), os.Stderr)
			}
			// For tests that failed due to a timeout, don't fail to shutdown
			// because ctx is done.
			//
			// There is little point to setting an arbitrary timeout for closing
			// the editor: in general we want to clean up before proceeding to the
			// next test, and if there is a deadlock preventing closing it will
			// eventually be handled by the `go test` timeout.
			if err := editor.Close(context.WithoutCancel(ctx)); err != nil {
				t.Errorf("error closing editor: %v", err)
			}
		}()
		// Always await the initial workspace load.
		env.Await(integration.AllOf(
			integration.LogMatching(protocol.Info, "initialized workspace folders", 1, true),
		))
		test(t, env)
	})
}

// Close cleans up resource that have been allocated to this workspace.
func (r *Runner) Close() error {
	var errmsgs []string
	if r.ts != nil {
		if err := r.ts.Close(); err != nil {
			errmsgs = append(errmsgs, err.Error())
		}
	}
	if !r.SkipCleanup {
		if err := os.RemoveAll(r.tempDir); err != nil {
			errmsgs = append(errmsgs, err.Error())
		}
	}
	if len(errmsgs) > 0 {
		return fmt.Errorf("errors closing the test runner:\n\t%s", strings.Join(errmsgs, "\n\t"))
	}
	return nil
}

type loggingFramer struct {
	mu  sync.Mutex
	buf *safeBuffer
}

// safeBuffer is a threadsafe buffer for logs.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (s *loggingFramer) framer(f jsonrpc2.Framer) jsonrpc2.Framer {
	return func(nc net.Conn) jsonrpc2.Stream {
		s.mu.Lock()
		framed := false
		if s.buf == nil {
			s.buf = &safeBuffer{buf: bytes.Buffer{}}
			framed = true
		}
		s.mu.Unlock()
		stream := f(nc)
		if framed {
			return protocol.LoggingStream(stream, s.buf)
		}
		return stream
	}
}

func (s *loggingFramer) printBuffers(testname string, w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.buf == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "#### Start Gopls Test Logs for %q\n", testname)
	s.buf.mu.Lock()
	io.Copy(w, &s.buf.buf)
	s.buf.mu.Unlock()
	fmt.Fprintf(os.Stderr, "#### End Gopls Test Logs for %q\n", testname)
}

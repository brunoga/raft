// Package shutdowncheck runs an example command end to end and checks that a
// signal stops it.
//
// It lives here rather than in each example because the fault it looks for
// cannot be reached any other way. A service shuts down in main: deferred
// calls, a signal handler, a server whose handlers have to be let go of. None
// of that is reachable from a normal test, which is why watchtower could sit
// forever after printing "shutting down" with a green test suite, and why
// configsvc and ledger could carry a shutdown path that had never once run.
//
// The two faults it was written for:
//
//   - A shutdown that never finishes, because two deferred calls wait on each
//     other. Deferred calls run last-registered-first, so a wait registered
//     after the thing it waits for runs before it.
//   - A shutdown that finishes only after its timeout, because an open
//     server-sent-event stream has no natural end and http.Server.Shutdown
//     waits for in-flight requests rather than cancelling them.
//
// Both reproduce on every run once you have the assembled binary, and neither
// is visible without it.
package shutdowncheck

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// promptly is how long a shutdown may take before it is treated as having
// waited for something. It is generous -- the work itself is milliseconds --
// but well under the ten-second server shutdown timeout these examples set,
// which is the duration the bug it catches produces.
const promptly = 5 * time.Second

// Options describes the command under test.
type Options struct {
	// Package is the import path to build, relative to the caller's
	// directory. "." for the package the test is in.
	Package string
	// Args builds the command line, given the addresses and data directory
	// chosen for this run. The command must serve HTTP on httpAddr.
	Args func(raftAddr, httpAddr, dataDir string) []string
	// ReadyPath is an HTTP path that answers once the service is up. Any
	// status counts; the test only needs to know the listener is accepting.
	ReadyPath string
	// StreamPath, when set, is a server-sent-event endpoint the check attaches
	// to before signalling, to cover the case of a shutdown that has to
	// interrupt a request rather than wait for it.
	StreamPath string
	// WantLogLine, when set, must appear in the output. Use it for the line
	// that proves a deferred cleanup ran -- an exit that skips it looks
	// identical from outside.
	WantLogLine string
}

// Run builds the command, starts it, sends it an interrupt, and fails t unless
// it exits cleanly and promptly.
//
// When StreamPath is set it runs twice: once plain, and once with a client
// attached to the stream.
func Run(t *testing.T, opts Options) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sending a signal to another process is not supported on Windows")
	}

	bin := build(t, opts.Package)

	cases := []struct {
		name   string
		attach bool
	}{{name: "plain", attach: false}}
	if opts.StreamPath != "" {
		cases = append(cases, struct {
			name   string
			attach bool
		}{name: "with an open stream", attach: true})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runOnce(t, bin, opts, tc.attach)
		})
	}
}

func runOnce(t *testing.T, bin string, opts Options, attach bool) {
	t.Helper()
	raftAddr, httpAddr := freeAddr(t), freeAddr(t)
	dataDir := filepath.Join(t.TempDir(), "data")

	cmd := exec.Command(bin, opts.Args(raftAddr, httpAddr, dataDir)...)
	out, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	cmd.Stdout = cmd.Stderr
	if startErr := cmd.Start(); startErr != nil {
		t.Fatalf("start: %v", startErr)
	}
	defer func() { _ = cmd.Process.Kill() }()

	logs := drain(out)
	awaitServing(t, httpAddr, opts.ReadyPath, logs)

	if attach {
		resp, cancel := openStream(t, httpAddr, opts.StreamPath)
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
	}

	if sigErr := cmd.Process.Signal(os.Interrupt); sigErr != nil {
		t.Fatalf("signal: %v", sigErr)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Well past any shutdown timeout an example sets, so that a failure here
	// means stuck rather than slow.
	started := time.Now()
	select {
	case waitErr := <-done:
		if waitErr != nil {
			t.Errorf("exited with %v, want a clean exit; output:\n%s", waitErr, logs())
		}
	case <-time.After(25 * time.Second):
		t.Fatalf("still running 25s after an interrupt; output:\n%s", logs())
	}

	// Exiting is not enough. A service that waits for an open stream exits
	// only when its shutdown timeout expires, which these examples set to ten
	// seconds -- and some of them treat that as a warning rather than an
	// error, so the process still exits zero and nothing looks wrong. Stopping
	// a node and closing its log takes milliseconds, so anything in the
	// seconds means something was waited on that should have been cancelled.
	if elapsed := time.Since(started); elapsed > promptly {
		t.Errorf("took %s to exit, want under %s: a shutdown that slow is one waiting out "+
			"a timeout rather than finishing. http.Server.Shutdown waits for in-flight "+
			"requests instead of cancelling them, so an open event stream holds it for "+
			"the whole timeout unless every request context descends from a BaseContext "+
			"that shutdown cancels. Output:\n%s", elapsed.Round(time.Millisecond), promptly, logs())
	}

	if opts.WantLogLine != "" && !strings.Contains(logs(), opts.WantLogLine) {
		t.Errorf("output does not contain %q, so the shutdown path did not run; a process "+
			"killed by the default signal handler exits just as quietly. Output:\n%s",
			opts.WantLogLine, logs())
	}
}

// build compiles pkg and returns the path to the binary.
func build(t *testing.T, pkg string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "svc")
	cmd := exec.Command("go", "build", "-o", bin, pkg)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, out)
	}
	return bin
}

// freeAddr returns a loopback address nothing is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if closeErr := l.Close(); closeErr != nil {
		t.Fatalf("close: %v", closeErr)
	}
	return addr
}

// awaitServing blocks until the service answers on path.
func awaitServing(t *testing.T, httpAddr, path string, logs func() string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + httpAddr + path) //nolint:noctx // short-lived probe
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("never answered on %s%s; output:\n%s", httpAddr, path, logs())
}

// openStream attaches to an event stream and returns once the response headers
// are in, so the handler is known to be running when the signal arrives.
func openStream(t *testing.T, httpAddr, path string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+httpAddr+path, http.NoBody)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp, cancel
}

// drain reads the process output so it cannot block on a full pipe, and
// returns a func giving back what has been read so far.
//
// The accessor is used while the reader is still running -- a process that
// failed to exit never closes its pipe, and that is exactly the case whose
// output is worth printing -- so the lines are behind a mutex.
func drain(r io.Reader) func() string {
	var (
		mu    sync.Mutex
		lines []string
	)
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			mu.Lock()
			lines = append(lines, sc.Text())
			mu.Unlock()
		}
	}()
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return fmt.Sprintf("  %s", strings.Join(lines, "\n  "))
	}
}

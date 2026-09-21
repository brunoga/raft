package main

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

// TestShutdown_SignalStopsTheProcess is a regression test for a shutdown that
// never completed.
//
// The observer goroutine returns when the event channel closes, and only the
// stop function returned by Node.Events closes it. Both were deferred, and
// deferred calls run last-registered-first, so the wait for the goroutine ran
// before the call that lets the goroutine finish. The process printed
// "shutting down" and stayed there forever; pressing ^C again did nothing,
// because signal.Notify had already replaced the default handler that would
// have killed it.
//
// Nothing shorter than running the binary catches that. The deadlock is
// between two deferred calls in main, so it exists only in the assembled
// program, and it reproduces on every single run.
func TestShutdown_SignalStopsTheProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sending signals to another process is not supported on Windows")
	}
	t.Parallel()

	for _, tc := range []struct {
		name   string
		attach bool
	}{
		// The plain case is the deadlock above.
		{name: "no client attached", attach: false},
		// With a viewer on /events, which is how this example is meant to be
		// watched. Shutdown does not cancel in-flight requests, it waits for
		// them, and an event stream has no natural end -- so a single attached
		// viewer held the process open for the whole shutdown timeout and then
		// exited with "context deadline exceeded". Every request context
		// descends from the server's BaseContext, which is cancelled first.
		{name: "events client attached", attach: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin := buildWatchtower(t)
			raftAddr, httpAddr := freePort(t), freePort(t)

			cmd := exec.Command(bin,
				"--id", "n1",
				"--raft-addr", raftAddr,
				"--http-addr", httpAddr,
				"--data-dir", filepath.Join(t.TempDir(), "data"))
			out, err := cmd.StderrPipe()
			if err != nil {
				t.Fatalf("stderr pipe: %v", err)
			}
			cmd.Stdout = cmd.Stderr
			if startErr := cmd.Start(); startErr != nil {
				t.Fatalf("start: %v", startErr)
			}
			defer func() { _ = cmd.Process.Kill() }()

			logs := drainInBackground(out)
			awaitServing(t, httpAddr)

			if tc.attach {
				resp, cancel := openEventStream(t, httpAddr)
				defer cancel()
				defer func() { _ = resp.Body.Close() }()
			}

			if sigErr := cmd.Process.Signal(os.Interrupt); sigErr != nil {
				t.Fatalf("signal: %v", sigErr)
			}

			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()

			// Generously past the ten-second shutdown timeout, so that a
			// failure means the process is stuck rather than slow.
			select {
			case waitErr := <-done:
				if waitErr != nil {
					t.Errorf("exited with %v, want a clean exit; output:\n%s", waitErr, logs())
				}
			case <-time.After(25 * time.Second):
				t.Fatalf("still running 25s after SIGINT; output:\n%s", logs())
			}
		})
	}
}

// buildWatchtower compiles the command under test and returns the path to it.
func buildWatchtower(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "watchtower")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// freePort returns a loopback address nothing is listening on.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

// awaitServing blocks until the node answers on its HTTP address.
func awaitServing(t *testing.T, httpAddr string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + httpAddr + "/health") //nolint:noctx // short-lived probe in a test
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("node never answered on %s", httpAddr)
}

// openEventStream attaches to /events and returns once the response headers
// are in, so the handler is known to be running when the signal arrives.
func openEventStream(t *testing.T, httpAddr string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+httpAddr+"/events", http.NoBody)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /events: %v", err)
	}
	return resp, cancel
}

// drainInBackground reads the process output so it cannot block on a full
// pipe, and returns a func giving back what has been read so far.
//
// The accessor is used while the reader is still running -- a process that
// failed to exit never closes its pipe, and that is exactly the case whose
// output is worth printing -- so the lines are behind a mutex rather than
// handed over on a closed channel.
func drainInBackground(r io.Reader) func() string {
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

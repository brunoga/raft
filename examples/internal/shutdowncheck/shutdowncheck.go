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
//
// # What the bound has to clear
//
// The check is a wall-clock one, so it has to sit above everything slow that
// is not a fault. There is one such thing, and it is not obvious:
// http.Server.Shutdown will not finish while any connection sits in
// http.StateNew -- accepted, but with no request read from it yet -- and it
// waits a full five seconds before treating such a connection as idle and
// closing it (net/http's own comment cites Go issue 22682). So a single
// connection that was opened and never used costs a shutdown five seconds,
// whatever the service does.
//
// A client is enough to produce one by accident. http.Transport answers a
// request by racing the idle pool against a fresh dial, and when the pool
// wins, the dial that lost is parked in the pool having sent nothing --
// which is exactly a StateNew connection on the server. That is what made
// this check fail intermittently: the checker's own client was leaving one
// behind. The client below therefore keeps no idle connections and drops
// what it has before signalling, and the bound still allows for five
// seconds in case something else opens one.
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
	"syscall"
	"testing"
	"time"
)

// promptly is how long a shutdown may take before it is treated as having
// waited for something. Three numbers decide it: the work itself is
// milliseconds, a connection that was opened and never used costs
// http.Server.Shutdown five seconds (see above), and the fault this looks
// for -- waiting out the shutdown timeout instead of cancelling -- costs the
// ten seconds these examples give it. Eight seconds is the room between the
// worst case that is not a fault and the best case that is.
const promptly = 8 * time.Second

// newClient returns an HTTP client that leaves no connection behind on the
// service, and the transport to drop what it holds before signalling.
//
// Keep-alives are off because an idle connection in the pool is what gives
// http.Transport something to race a dial against, and the dial that loses
// that race is the unused connection that costs Shutdown five seconds. Each
// run gets its own, so one subtest cannot leave a connection pooled for the
// next.
func newClient() (*http.Client, *http.Transport) {
	tr := &http.Transport{DisableKeepAlives: true}
	return &http.Client{Transport: tr}, tr
}

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
	addrs := freeAddrs(t, 2)
	raftAddr, httpAddr := addrs[0], addrs[1]
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
	client, transport := newClient()
	awaitServing(t, client, httpAddr, opts.ReadyPath, logs)

	if attach {
		resp, cancel := openStream(t, client, httpAddr, opts.StreamPath)
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
	}

	// Anything still pooled here is a connection the service would have to
	// wait out; the one being measured is the attached stream, which is in
	// use and so is not touched by this.
	transport.CloseIdleConnections()

	if sigErr := cmd.Process.Signal(os.Interrupt); sigErr != nil {
		t.Fatalf("signal: %v", sigErr)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Exiting is not enough. A service that waits for an open stream exits
	// only when its shutdown timeout expires, which these examples set to ten
	// seconds -- and some of them treat that as a warning rather than an
	// error, so the process still exits zero and nothing looks wrong. Stopping
	// a node and closing its log takes milliseconds, so anything in the
	// seconds means something was waited on that should have been cancelled.
	//
	// A process that misses the bound is asked for its stacks before it is
	// waited for any further. Without that, all a failure here can say is how
	// long it took, which is the one thing that does not identify the cause --
	// and the cause is a goroutine blocked on something, which the dump names
	// outright. SIGQUIT is what Go turns into that dump.
	started := time.Now()
	select {
	case waitErr := <-done:
		if waitErr != nil {
			t.Errorf("exited with %v, want a clean exit; output:\n%s", waitErr, logs())
		}
		if elapsed := time.Since(started); elapsed > promptly {
			t.Errorf("took %s to exit, want under %s; output:\n%s",
				elapsed.Round(time.Millisecond), promptly, logs())
		}
	case <-time.After(promptly):
		_ = cmd.Process.Signal(syscall.SIGQUIT)
		select {
		case <-done:
		case <-time.After(25 * time.Second):
			t.Fatalf("still running 25s after SIGQUIT; output:\n%s", logs())
		}
		// Give the dump, which the process writes as it dies, a moment to
		// reach the pipe reader.
		time.Sleep(100 * time.Millisecond)
		t.Fatalf("still running %s after an interrupt, so something is being waited on "+
			"that should have been cancelled. A shutdown that slow is one waiting out a "+
			"timeout rather than finishing: http.Server.Shutdown waits for in-flight "+
			"requests instead of cancelling them, so an open event stream holds it for "+
			"the whole timeout unless every request context descends from a BaseContext "+
			"that shutdown cancels. The goroutine dump below is from SIGQUIT at that "+
			"point; look for the one parked in the shutdown path. Output:\n%s",
			promptly, logs())
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

// freeAddrs returns n distinct loopback addresses nothing is listening on.
//
// Every listener is held open until all n have been chosen. Picking them one
// at a time, each released before the next is asked for, lets the kernel hand
// the same port out twice -- and a service given one address for its Raft
// port and its HTTP port binds the first, fails the second, and exits before
// it has answered anything.
func freeAddrs(t *testing.T, n int) []string {
	t.Helper()
	lns := make([]net.Listener, 0, n)
	addrs := make([]string, 0, n)
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		lns = append(lns, l)
		addrs = append(addrs, l.Addr().String())
	}
	for _, l := range lns {
		if err := l.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	return addrs
}

// awaitServing blocks until the service answers on path.
func awaitServing(t *testing.T, client *http.Client, httpAddr, path string, logs func() string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + httpAddr + path) //nolint:noctx // short-lived probe
		if err == nil {
			// Drained, not just closed: a body left unread is a connection
			// the transport cannot reuse and has to replace.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("never answered on %s%s; output:\n%s", httpAddr, path, logs())
}

// openStream attaches to an event stream and returns once the response headers
// are in, so the handler is known to be running when the signal arrives.
func openStream(t *testing.T, client *http.Client, httpAddr, path string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+httpAddr+path, http.NoBody)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
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

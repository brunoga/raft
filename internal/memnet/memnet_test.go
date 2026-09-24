package memnet_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2/internal/memnet"
)

// echo accepts one connection and copies it back, until the listener closes.
func echo(ln *memnet.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = c.Close() }()
			_, _ = io.Copy(c, c)
		}()
	}
}

func TestRoundTrip(t *testing.T) {
	ln := memnet.Listen("node-1:7000")
	defer func() { _ = ln.Close() }()
	go echo(ln)

	c, err := ln.Dial(context.Background())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	if _, err := io.WriteString(c, "hello"); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf); got != "hello" {
		t.Errorf("read %q, want %q", got, "hello")
	}
}

func TestAddrIsWhatItWasNamed(t *testing.T) {
	ln := memnet.Listen("node-1:7000")
	defer func() { _ = ln.Close() }()
	if got := ln.Addr().String(); got != "node-1:7000" {
		t.Errorf("Addr() = %q, want %q", got, "node-1:7000")
	}
	if got := ln.Addr().Network(); got != "memnet" {
		t.Errorf("Network() = %q, want %q", got, "memnet")
	}
}

func TestCloseUnblocksAcceptAndDial(t *testing.T) {
	ln := memnet.Listen("node-1:7000")
	accepted := make(chan error, 1)
	go func() {
		_, err := ln.Accept()
		accepted <- err
	}()
	// Give Accept a moment to park, then close.
	time.Sleep(10 * time.Millisecond)
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-accepted; !errors.Is(err, memnet.ErrClosed) {
		t.Errorf("Accept after Close returned %v, want ErrClosed", err)
	}
	if _, err := ln.Dial(context.Background()); !errors.Is(err, memnet.ErrClosed) {
		t.Errorf("Dial after Close returned %v, want ErrClosed", err)
	}
}

// Close is called by the server that owns the listener and often by the test
// that made it, so calling it twice must not panic on a closed channel.
func TestCloseTwice(t *testing.T) {
	ln := memnet.Listen("node-1:7000")
	if err := ln.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestDialRespectsContext(t *testing.T) {
	ln := memnet.Listen("node-1:7000") // nobody accepts
	defer func() { _ = ln.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := ln.Dial(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Dial with no acceptor returned %v, want DeadlineExceeded", err)
	}
}

// The reason this package exists: a listener sitting idle inside a synctest
// bubble must not stop the clock. A real one does, because a goroutine parked
// in Accept on a socket is not durably blocked, and the test then hangs
// instead of failing.
func TestIdleListenerDoesNotStopTheBubbleClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ln := memnet.Listen("node-1:7000")
		defer func() { _ = ln.Close() }()
		go echo(ln)

		start := time.Now()
		time.Sleep(time.Hour)
		if elapsed := time.Since(start); elapsed != time.Hour {
			t.Errorf("fake clock advanced %v, want %v", elapsed, time.Hour)
		}
	})
}

// And an HTTP server on one serves requests in a bubble, which is what the
// easyraft tests need.
func TestHTTPServerInBubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ln := memnet.Listen("node-1:8000")
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "ok")
			}),
		}
		go func() { _ = srv.Serve(ln) }()
		defer func() { _ = srv.Close() }()

		client := &http.Client{Transport: &http.Transport{DialContext: ln.DialContext}}
		resp, err := client.Get("http://node-1:8000/")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if string(body) != "ok" {
			t.Errorf("body = %q, want %q", body, "ok")
		}

		// The connection is now idle in the client's pool, which is exactly
		// the state that wedges a bubble when the socket is real.
		start := time.Now()
		time.Sleep(time.Minute)
		if elapsed := time.Since(start); elapsed != time.Minute {
			t.Errorf("fake clock advanced %v after a request, want %v", elapsed, time.Minute)
		}
	})
}

var _ net.Listener = (*memnet.Listener)(nil)

func TestNetworkRoutesByAddress(t *testing.T) {
	nw := memnet.NewNetwork()
	for _, name := range []string{"n1:8080", "n2:8080"} {
		ln := nw.Listen(name)
		who := name
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, who)
		})}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() {
			_ = srv.Close()
			_ = ln.Close()
		})
	}

	client := nw.HTTPClient()
	for _, name := range []string{"n1:8080", "n2:8080"} {
		resp, err := client.Get("http://" + name + "/")
		if err != nil {
			t.Fatalf("Get %s: %v", name, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(body) != name {
			t.Errorf("%s answered %q, want %q", name, body, name)
		}
	}
}

func TestNetworkUnknownAddress(t *testing.T) {
	n := memnet.NewNetwork()
	_, err := n.DialContext(context.Background(), "tcp", "nobody:9999")
	var missing *memnet.ErrNoListener
	if !errors.As(err, &missing) {
		t.Fatalf("Dial to an address with no listener returned %v, want ErrNoListener", err)
	}
	if missing.Addr != "nobody:9999" {
		t.Errorf("ErrNoListener names %q, want %q", missing.Addr, "nobody:9999")
	}
}

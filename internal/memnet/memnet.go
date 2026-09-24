// Package memnet provides an in-process net.Listener and the dialer that
// reaches it, so a server can be started and talked to without a socket.
//
// It exists for tests that run inside a testing/synctest bubble. A bubble's
// clock only advances once every goroutine in it is durably blocked, and a
// goroutine parked in a real Accept or a real socket read is not durably
// blocked: the runtime cannot know whether the kernel is about to wake it.
// One idle listener is therefore enough to stop the clock for good, and the
// test hangs rather than fails. Connections made of net.Pipe are plain
// channel operations, which are durable, so a server built on this one can
// sit idle in a bubble without stopping time.
//
// It is not a simulation of a network: there is no latency, no loss and no
// reordering. It is the socket layer removed.
package memnet

import (
	"context"
	"errors"
	"net"
	"sync"
)

// ErrClosed is returned by Accept and Dial once the listener is closed.
var ErrClosed = errors.New("memnet: listener closed")

// Listener is an in-process net.Listener. The zero value is not usable;
// call Listen.
type Listener struct {
	addr   addr
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

// Listen returns a Listener whose address reports name, which is the string
// a client passes to Dial and which shows up wherever the server prints
// where it is listening.
func Listen(name string) *Listener {
	return &Listener{
		addr:   addr(name),
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

// Accept implements net.Listener.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, ErrClosed
	}
}

// Close implements net.Listener. It is safe to call more than once, which
// matters because a server and its test both tend to close what they own.
func (l *Listener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

// Addr implements net.Listener.
func (l *Listener) Addr() net.Addr { return l.addr }

// Dial returns one end of a new connection to this listener, handing the
// other end to whoever is in Accept. It blocks until the server accepts,
// which is what a real dial does too.
func (l *Listener) Dial(ctx context.Context) (net.Conn, error) {
	server, client := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
		_ = server.Close()
		_ = client.Close()
		return nil, ErrClosed
	case <-ctx.Done():
		_ = server.Close()
		_ = client.Close()
		return nil, ctx.Err()
	}
}

// DialContext has the shape http.Transport.DialContext wants. The network
// and address are ignored: this listener is the only place a connection can
// go.
func (l *Listener) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	return l.Dial(ctx)
}

// DialTarget has the shape grpc.WithContextDialer wants, which takes the
// target as one string rather than a network and an address. Same listener,
// same ignored argument; gRPC just spells its dialer differently.
func (l *Listener) DialTarget(ctx context.Context, _ string) (net.Conn, error) {
	return l.Dial(ctx)
}

// addr is a net.Addr whose string is whatever the listener was named.
type addr string

func (a addr) Network() string { return "memnet" }
func (a addr) String() string  { return string(a) }

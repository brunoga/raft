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
	"net/http"
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

// Network is a set of listeners addressed by name, and the dialer that
// reaches them.
//
// A cluster test needs more than one server, and the client then has to be
// told which in-process listener an address belongs to, because "n2:8080" is
// not somewhere the operating system can take it. Network is that lookup,
// with the http.Client that uses it.
//
// The zero value is not usable; call NewNetwork.
type Network struct {
	mu  sync.Mutex
	lns map[string]*Listener
}

// NewNetwork returns an empty Network.
func NewNetwork() *Network {
	return &Network{lns: make(map[string]*Listener)}
}

// Listen adds a listener under name and returns it. Listening twice on the
// same name replaces the first, which is what restarting a node looks like.
func (n *Network) Listen(name string) *Listener {
	ln := Listen(name)
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lns[name] = ln
	return ln
}

// Has reports whether anything is listening on addr.
//
// It exists for callers that have to choose between this network and the
// operating system for each address, which is what a test package holding
// both in-memory and real nodes has to do. Deciding by asking is better than
// deciding by falling back: a dialer that quietly reaches the real network
// when it does not recognise an address is how a test comes to believe it is
// in memory when it is not.
func (n *Network) Has(addr string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.lns[addr]
	return ok
}

// ErrNoListener is returned when nothing is listening on the address dialled.
// It stands in for connection-refused, and says which address so a test that
// mistypes one is told rather than left to time out.
type ErrNoListener struct{ Addr string }

func (e *ErrNoListener) Error() string {
	return "memnet: nothing is listening on " + e.Addr
}

// DialContext has the shape http.Transport.DialContext wants and routes by
// address.
func (n *Network) DialContext(ctx context.Context, _, addr string) (net.Conn, error) {
	n.mu.Lock()
	ln, ok := n.lns[addr]
	n.mu.Unlock()
	if !ok {
		return nil, &ErrNoListener{Addr: addr}
	}
	return ln.Dial(ctx)
}

// DialTarget has the shape grpc.WithContextDialer wants and routes by target.
func (n *Network) DialTarget(ctx context.Context, target string) (net.Conn, error) {
	return n.DialContext(ctx, "memnet", target)
}

// HTTPClient returns a client whose connections go to this network's
// listeners rather than to the operating system.
func (n *Network) HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: n.DialContext}}
}

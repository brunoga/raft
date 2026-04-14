// Package udpbroadcast provides a UDP-broadcast-based implementation of
// discovery.Discovery for local-area-network Raft cluster peer discovery.
//
// Each node periodically sends a small JSON announcement containing its NodeID
// and gRPC address over UDP to the configured broadcast address. Nodes receive
// announcements from peers and maintain a live peer table; entries that have
// not been refreshed within the configured TTL are excluded from Discover
// results.
//
// # Typical usage
//
//	b, err := udpbroadcast.New(udpbroadcast.Config{
//	    NodeID: cfg.ID,
//	    Addr:   "192.168.1.10:7001",
//	})
//	if err != nil { ... }
//
//	// Start broadcasting and listening in the background.
//	go b.Run(ctx) //nolint:errcheck // Run returns ctx.Err(); unactionable in a background goroutine.
//
//	// Wire into a DiscoveryAgent that feeds the gRPC transport.
//	agent := discovery.NewAgent(b, grpcTransport, 30*time.Second)
//	go agent.Run(ctx) //nolint:errcheck // same rationale.
//
// # Scope
//
// UDP broadcast is suitable for development environments and LAN deployments
// where all nodes share the same IP subnet. It is not suitable for cross-subnet
// or cloud deployments — use dnsdiscovery or a service-registry Discovery there.
package udpbroadcast

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/discovery"
)

const (
	defaultBroadcastPort = 9199
	defaultInterval      = 2 * time.Second
	maxPacketSize        = 512 // sufficient for any NodeID + Addr pair
)

// Config holds the configuration for a UDPBroadcast.
type Config struct {
	// NodeID is this node's Raft identity; included in every outbound
	// announcement. Required.
	NodeID raft.NodeID

	// Addr is the gRPC address other peers should dial to reach this node
	// (host:port). Required.
	Addr string

	// BroadcastAddr is the UDP destination for outbound announcements.
	// Defaults to "255.255.255.255:9199" (subnet-limited broadcast).
	BroadcastAddr string

	// ListenAddr is the local UDP address to bind for receiving announcements.
	// Defaults to ":<port>" where port is taken from BroadcastAddr, so that
	// all interfaces on the same port are covered.
	ListenAddr string

	// Interval is how often this node sends an announcement.
	// Defaults to 2s.
	Interval time.Duration

	// TTL is how long a peer entry remains valid without being refreshed.
	// Peers not heard from within TTL are excluded from Discover results.
	// Defaults to 3 × Interval.
	TTL time.Duration
}

// announcement is the wire format for a single UDP broadcast message.
type announcement struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

type peerEntry struct {
	addr     string
	lastSeen time.Time
}

// UDPBroadcast sends periodic UDP announcements and maintains a table of peers
// heard from the same subnet. It implements discovery.Discovery.
//
// Call Run to start sending and receiving (it blocks until ctx is cancelled).
// Discover returns a point-in-time snapshot of live peers (those heard from
// within the TTL window). Both are safe for concurrent use.
type UDPBroadcast struct {
	cfg   Config
	mu    sync.RWMutex
	peers map[raft.NodeID]peerEntry
}

// New validates cfg, applies defaults, and returns a UDPBroadcast ready to run.
// cfg is copied internally; the caller may modify or discard it after New returns.
func New(cfg *Config) (*UDPBroadcast, error) {
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("udpbroadcast: NodeID is required")
	}
	if cfg.Addr == "" {
		return nil, fmt.Errorf("udpbroadcast: Addr is required")
	}
	c := *cfg // copy so we can fill in defaults without mutating the caller's struct
	if c.BroadcastAddr == "" {
		c.BroadcastAddr = fmt.Sprintf("255.255.255.255:%d", defaultBroadcastPort)
	}
	if c.ListenAddr == "" {
		_, port, err := net.SplitHostPort(c.BroadcastAddr)
		if err != nil {
			return nil, fmt.Errorf("udpbroadcast: invalid BroadcastAddr %q: %w",
				c.BroadcastAddr, err)
		}
		c.ListenAddr = ":" + port
	}
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.TTL <= 0 {
		c.TTL = 3 * c.Interval
	}
	return &UDPBroadcast{
		cfg:   c,
		peers: make(map[raft.NodeID]peerEntry),
	}, nil
}

// Discover returns all peers heard from within the TTL window.
// It implements discovery.Discovery and is safe for concurrent use.
func (u *UDPBroadcast) Discover(_ context.Context) ([]discovery.PeerInfo, error) {
	now := time.Now()
	u.mu.RLock()
	defer u.mu.RUnlock()
	var out []discovery.PeerInfo
	for id, e := range u.peers {
		if now.Sub(e.lastSeen) <= u.cfg.TTL {
			out = append(out, discovery.PeerInfo{ID: id, Addr: e.addr})
		}
	}
	return out, nil
}

// Run opens a broadcast-capable UDP socket, sends an immediate announcement,
// then re-announces every Interval and receives announcements from peers until
// ctx is cancelled. It returns ctx.Err() when done.
func (u *UDPBroadcast) Run(ctx context.Context) error {
	conn, err := newBroadcastConn(u.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("udpbroadcast: listen %s: %w", u.cfg.ListenAddr, err)
	}
	return u.run(ctx, conn)
}

// run is the testable inner loop. It takes ownership of conn and closes it
// when ctx is cancelled or an unrecoverable error occurs.
func (u *UDPBroadcast) run(ctx context.Context, conn net.PacketConn) error {
	// Ensure the conn is closed on any exit path, including early returns.
	// The ctx.Done() case also closes it explicitly to unblock ReadFrom; the
	// second close (via defer) returns a benign error that we discard.
	defer func() { _ = conn.Close() }()

	bcastAddr, err := net.ResolveUDPAddr("udp4", u.cfg.BroadcastAddr)
	if err != nil {
		return fmt.Errorf("udpbroadcast: resolve %q: %w", u.cfg.BroadcastAddr, err)
	}

	// send encodes and dispatches one announcement. Errors are best-effort:
	// a missed send is recovered by the next tick.
	send := func() {
		msg, encErr := json.Marshal(announcement{
			ID:   string(u.cfg.NodeID),
			Addr: u.cfg.Addr,
		})
		if encErr != nil {
			return // impossible: struct only contains string fields
		}
		_, _ = conn.WriteTo(msg, bcastAddr) // best-effort; transient errors are acceptable
	}

	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		buf := make([]byte, maxPacketSize)
		for {
			n, _, readErr := conn.ReadFrom(buf)
			if readErr != nil {
				return // conn was closed; exit cleanly
			}
			var ann announcement
			if unmarshalErr := json.Unmarshal(buf[:n], &ann); unmarshalErr != nil ||
				ann.ID == "" || ann.Addr == "" {
				continue // malformed or incomplete packet; skip
			}
			id := raft.NodeID(ann.ID)
			if id == u.cfg.NodeID {
				continue // ignore own announcements
			}
			u.mu.Lock()
			u.peers[id] = peerEntry{addr: ann.Addr, lastSeen: time.Now()}
			u.mu.Unlock()
		}
	}()

	send() // immediate seed on startup

	ticker := time.NewTicker(u.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = conn.Close() // unblocks ReadFrom in the receive goroutine
			<-recvDone       // wait for the goroutine to exit before returning
			return ctx.Err()
		case <-ticker.C:
			send()
		}
	}
}

// newBroadcastConn creates a UDP socket with SO_BROADCAST set so it can send
// to subnet broadcast addresses (e.g. 255.255.255.255).
func newBroadcastConn(listenAddr string) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: setBroadcast}
	return lc.ListenPacket(context.Background(), "udp4", listenAddr)
}

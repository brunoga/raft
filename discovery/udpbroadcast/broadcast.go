// Package udpbroadcast provides a UDP-broadcast-based implementation of
// discovery.Discovery for local-area-network Raft cluster peer discovery.
//
// Each node periodically sends a small JSON announcement containing its NodeID
// and gRPC address over UDP to the configured broadcast address. Nodes receive
// announcements from peers and maintain a live peer table; entries that have
// not been refreshed within the configured TTL are excluded from Discover
// results.
//
// # Security
//
// UDP broadcast is unauthenticated by default: any host that can put a packet
// on the wire can claim any NodeID and any address. Set [Config.Secret] to a
// cluster-wide shared secret so every announcement is authenticated with an
// HMAC over the node ID, address and a timestamped nonce; announcements that
// are unsigned, wrongly signed, stale, or replayed are dropped. Without a
// secret this transport is only safe on a network you fully trust.
//
// Even with a secret, a node that is already a cluster member can change the
// address it advertises. Rebinding a known member's address is refused (and
// logged) unless [Config.AllowAddressChange] is set, so a rogue announcement
// cannot silently redirect traffic for an existing member.
//
// # Typical usage
//
//	b, err := udpbroadcast.New(&udpbroadcast.Config{
//	    NodeID: cfg.ID,
//	    Addr:   "192.168.1.10:7001",
//	    Secret: []byte(os.Getenv("RAFT_DISCOVERY_SECRET")),
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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/discovery"
)

const (
	defaultBroadcastPort = 9199
	defaultInterval      = 2 * time.Second
	defaultReplayWindow  = 30 * time.Second
	maxPacketSize        = 1024 // NodeID + Addr + nonce + HMAC, with room to spare
	nonceSize            = 16
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

	// Secret is the cluster-wide shared secret used to authenticate
	// announcements. When set, outbound announcements are signed with an HMAC
	// over (id, addr, timestamp, nonce) and inbound announcements are accepted
	// only if they carry a matching signature, a timestamp within ReplayWindow
	// of local time, and a nonce not seen before.
	//
	// When empty, announcements are neither signed nor verified: any host on
	// the network can inject a peer. Leave it empty only on a trusted network.
	Secret []byte

	// ReplayWindow bounds how far an announcement's timestamp may be from
	// local time and how long a nonce is remembered for replay detection.
	// It therefore also bounds the tolerated clock skew between nodes.
	// Defaults to 30s. Ignored when Secret is empty.
	ReplayWindow time.Duration

	// AllowAddressChange permits an announcement to replace the address
	// already recorded for a known NodeID. It is false by default: rebinding a
	// member's address is a traffic-redirection primitive, so it must be opted
	// into deliberately. Refused rebinds are logged at warn level; accepted
	// ones at info level.
	AllowAddressChange bool

	// Logger is used to log peer-discovery events (e.g. when a new peer is
	// first heard from). Defaults to slog.Default() when nil.
	Logger *slog.Logger
}

// announcement is the wire format for a single UDP broadcast message.
//
// TS, Nonce and MAC are populated only when a shared secret is configured;
// they are omitted otherwise so unauthenticated deployments keep the original
// two-field wire format.
type announcement struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`

	// TS is the sender's wall clock at announcement time, in Unix seconds.
	TS int64 `json:"ts,omitempty"`
	// Nonce is base64-encoded random bytes making each announcement unique.
	Nonce string `json:"nonce,omitempty"`
	// MAC is the base64-encoded HMAC-SHA256 over (ID, Addr, TS, Nonce).
	MAC string `json:"mac,omitempty"`
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
	cfg    Config
	logger *slog.Logger
	now    func() time.Time // overridable in tests

	mu    sync.RWMutex
	peers map[raft.NodeID]peerEntry

	// seenNonces remembers recently accepted nonces so a captured packet
	// cannot be replayed within the window. Guarded by nonceMu.
	nonceMu    sync.Mutex
	seenNonces map[string]time.Time
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
	if c.ReplayWindow <= 0 {
		c.ReplayWindow = defaultReplayWindow
	}
	// Copy the secret so a caller zeroing its buffer cannot disarm us midway.
	if len(c.Secret) > 0 {
		c.Secret = append([]byte(nil), c.Secret...)
	}
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if len(c.Secret) == 0 {
		logger.Warn("udpbroadcast: no shared secret configured; any host on this " +
			"network can announce itself as a cluster peer. Set Config.Secret " +
			"unless the network is fully trusted.")
	}
	return &UDPBroadcast{
		cfg:        c,
		logger:     logger,
		now:        time.Now,
		peers:      make(map[raft.NodeID]peerEntry),
		seenNonces: make(map[string]time.Time),
	}, nil
}

// Discover returns all peers heard from within the TTL window.
// It implements discovery.Discovery and is safe for concurrent use.
func (u *UDPBroadcast) Discover(_ context.Context) ([]discovery.PeerInfo, error) {
	now := u.now()
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
		msg, encErr := u.encodeAnnouncement()
		if encErr != nil {
			u.logger.Error("udpbroadcast: encode announcement", "err", encErr)
			return
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
			u.handlePacket(buf[:n])
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

// encodeAnnouncement builds the wire representation of this node's
// announcement, signing it when a shared secret is configured.
func (u *UDPBroadcast) encodeAnnouncement() ([]byte, error) {
	ann := announcement{
		ID:   string(u.cfg.NodeID),
		Addr: u.cfg.Addr,
	}
	if len(u.cfg.Secret) > 0 {
		nonce := make([]byte, nonceSize)
		if _, err := rand.Read(nonce); err != nil {
			return nil, fmt.Errorf("udpbroadcast: generate nonce: %w", err)
		}
		ann.TS = u.now().Unix()
		ann.Nonce = base64.RawStdEncoding.EncodeToString(nonce)
		ann.MAC = base64.RawStdEncoding.EncodeToString(
			sign(u.cfg.Secret, ann.ID, ann.Addr, ann.TS, ann.Nonce))
	}
	b, err := json.Marshal(ann)
	if err != nil {
		return nil, fmt.Errorf("udpbroadcast: marshal announcement: %w", err)
	}
	if len(b) > maxPacketSize {
		return nil, fmt.Errorf("udpbroadcast: announcement is %d bytes, exceeds the %d byte limit",
			len(b), maxPacketSize)
	}
	return b, nil
}

// handlePacket validates one received datagram and, if it is acceptable,
// records the announcing peer.
func (u *UDPBroadcast) handlePacket(pkt []byte) {
	var ann announcement
	if err := json.Unmarshal(pkt, &ann); err != nil || ann.ID == "" || ann.Addr == "" {
		return // malformed or incomplete packet; skip
	}
	id := raft.NodeID(ann.ID)
	if id == u.cfg.NodeID {
		return // ignore our own announcements
	}
	if err := u.authenticate(&ann); err != nil {
		u.logger.Warn("udpbroadcast: rejected announcement", "id", id, "err", err)
		return
	}
	u.recordPeer(id, ann.Addr)
}

// authenticate verifies an announcement's signature, freshness and uniqueness.
// It returns nil immediately when no shared secret is configured.
func (u *UDPBroadcast) authenticate(ann *announcement) error {
	if len(u.cfg.Secret) == 0 {
		return nil
	}
	if ann.MAC == "" || ann.Nonce == "" || ann.TS == 0 {
		return fmt.Errorf("announcement is not signed")
	}
	got, err := base64.RawStdEncoding.DecodeString(ann.MAC)
	if err != nil {
		return fmt.Errorf("malformed signature: %w", err)
	}
	want := sign(u.cfg.Secret, ann.ID, ann.Addr, ann.TS, ann.Nonce)
	if !hmac.Equal(got, want) {
		return fmt.Errorf("signature mismatch")
	}

	skew := u.now().Sub(time.Unix(ann.TS, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > u.cfg.ReplayWindow {
		return fmt.Errorf("timestamp is %s away from local time, outside the %s replay window",
			skew.Round(time.Second), u.cfg.ReplayWindow)
	}
	if !u.claimNonce(ann.Nonce) {
		return fmt.Errorf("nonce has already been used")
	}
	return nil
}

// claimNonce records nonce as used and reports whether it was previously
// unseen. Entries older than the replay window are evicted on the way through.
func (u *UDPBroadcast) claimNonce(nonce string) bool {
	now := u.now()
	u.nonceMu.Lock()
	defer u.nonceMu.Unlock()

	for n, seen := range u.seenNonces {
		if now.Sub(seen) > 2*u.cfg.ReplayWindow {
			delete(u.seenNonces, n)
		}
	}
	if _, dup := u.seenNonces[nonce]; dup {
		return false
	}
	u.seenNonces[nonce] = now
	return true
}

// recordPeer inserts or refreshes the entry for id. An address change for an
// already-known peer is applied only when Config.AllowAddressChange is set;
// either way it is logged, because silently rebinding a member redirects all
// Raft traffic for that member.
func (u *UDPBroadcast) recordPeer(id raft.NodeID, addr string) {
	now := u.now()

	u.mu.Lock()
	prev, known := u.peers[id]
	switch {
	case !known, prev.addr == addr, u.cfg.AllowAddressChange:
		u.peers[id] = peerEntry{addr: addr, lastSeen: now}
	default:
		// Leave the entry untouched. Liveness is deliberately not refreshed
		// either: a rejected announcement is no evidence that the peer at the
		// address we know is still up.
	}
	u.mu.Unlock()

	switch {
	case !known:
		u.logger.Info("udpbroadcast: discovered peer", "id", id, "addr", addr)
	case prev.addr == addr:
		// Steady state: nothing worth logging.
	case u.cfg.AllowAddressChange:
		u.logger.Info("udpbroadcast: peer address changed",
			"id", id, "from", prev.addr, "to", addr)
	default:
		u.logger.Warn("udpbroadcast: refused peer address change; "+
			"set Config.AllowAddressChange to permit it",
			"id", id, "known", prev.addr, "announced", addr)
	}
}

// sign computes the HMAC-SHA256 over the announcement fields. Each field is
// length-prefixed so no combination of values can produce the same input as a
// different combination.
func sign(secret []byte, id, addr string, ts int64, nonce string) []byte {
	mac := hmac.New(sha256.New, secret)
	writeField := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		_, _ = mac.Write(n[:])
		_, _ = mac.Write([]byte(s))
	}
	writeField("udpbroadcast-v1")
	writeField(id)
	writeField(addr)
	var tsBytes [8]byte
	binary.BigEndian.PutUint64(tsBytes[:], uint64(ts))
	_, _ = mac.Write(tsBytes[:])
	writeField(nonce)
	return mac.Sum(nil)
}

// newBroadcastConn creates a UDP socket with SO_BROADCAST set so it can send
// to subnet broadcast addresses (e.g. 255.255.255.255).
func newBroadcastConn(listenAddr string) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: setBroadcast}
	return lc.ListenPacket(context.Background(), "udp4", listenAddr)
}

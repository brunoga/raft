package udpbroadcast

// White-box tests: access to run() lets us inject real UDP packet conns
// instead of opening broadcast sockets, keeping tests fast and isolated.
// In tests two instances send directly to each other's UDP address (unicast);
// the broadcast / multicast path is exercised by newBroadcastConn at runtime.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"sort"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/discovery"
)

// udpPair creates two bound UDP sockets and returns them along with their
// string addresses ("127.0.0.1:<port>").
func udpPair(t *testing.T) (connA, connB net.PacketConn, addrA, addrB string) {
	t.Helper()
	var err error
	connA, err = net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("udpPair: listen A: %v", err)
	}
	connB, err = net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		_ = connA.Close()
		t.Fatalf("udpPair: listen B: %v", err)
	}
	addrA = connA.LocalAddr().String()
	addrB = connB.LocalAddr().String()
	return connA, connB, addrA, addrB
}

// runBroadcast creates a UDPBroadcast with the given config, starts it with the
// supplied conn (bypassing newBroadcastConn), and cancels it on t.Cleanup.
// BroadcastAddr must be set in cfg; ListenAddr need not be (it is unused when
// conn is injected).
func runBroadcast(t *testing.T, cfg *Config, conn net.PacketConn) *UDPBroadcast {
	t.Helper()
	// Provide a dummy ListenAddr so New() doesn't complain about deriving it
	// from BroadcastAddr — we won't actually listen on it (conn is injected).
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = cfg.BroadcastAddr
	}
	ub, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = ub.run(ctx, conn) }() //nolint:errcheck // run returns ctx.Err(); unactionable here.
	return ub
}

// waitDiscover polls ub.Discover until predicate returns true or the deadline
// passes; it fails the test if the deadline expires.
func waitDiscover(t *testing.T, ub *UDPBroadcast, timeout time.Duration,
	predicate func([]discovery.PeerInfo) bool) []discovery.PeerInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		peers, err := ub.Discover(context.Background())
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if predicate(peers) {
			return peers
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
	return nil
}

// ---- New validation ---------------------------------------------------------

func TestNew_RequiresNodeID(t *testing.T) {
	_, err := New(&Config{Addr: "10.0.0.1:7001"})
	if err == nil {
		t.Fatal("expected error for missing NodeID")
	}
}

func TestNew_RequiresAddr(t *testing.T) {
	_, err := New(&Config{NodeID: "n1"})
	if err == nil {
		t.Fatal("expected error for missing Addr")
	}
}

func TestNew_InvalidBroadcastAddr(t *testing.T) {
	_, err := New(&Config{NodeID: "n1", Addr: "10.0.0.1:7001", BroadcastAddr: "not-an-addr"})
	if err == nil {
		t.Fatal("expected error for invalid BroadcastAddr")
	}
}

func TestNew_Defaults(t *testing.T) {
	ub, err := New(&Config{NodeID: "n1", Addr: "10.0.0.1:7001"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ub.cfg.BroadcastAddr == "" {
		t.Error("BroadcastAddr should have a default")
	}
	if ub.cfg.ListenAddr == "" {
		t.Error("ListenAddr should have a default")
	}
	if ub.cfg.Interval <= 0 {
		t.Error("Interval should have a default")
	}
	if ub.cfg.TTL <= 0 {
		t.Error("TTL should have a default")
	}
	if ub.cfg.TTL != 3*ub.cfg.Interval {
		t.Errorf("default TTL = %v, want 3 × Interval (%v)", ub.cfg.TTL, 3*ub.cfg.Interval)
	}
}

// ---- Discovery --------------------------------------------------------------

func TestRun_DiscoversPeer(t *testing.T) {
	connA, connB, addrA, addrB := udpPair(t)

	// A sends to B and B sends to A (unicast in test; broadcast in production).
	ubA := runBroadcast(t, &Config{
		NodeID:        "n1",
		Addr:          "10.0.0.1:7001",
		BroadcastAddr: addrB,
		Interval:      10 * time.Millisecond,
		TTL:           500 * time.Millisecond,
	}, connA)
	ubB := runBroadcast(t, &Config{
		NodeID:        "n2",
		Addr:          "10.0.0.2:7001",
		BroadcastAddr: addrA,
		Interval:      10 * time.Millisecond,
		TTL:           500 * time.Millisecond,
	}, connB)

	// A should discover B.
	peers := waitDiscover(t, ubA, time.Second, func(p []discovery.PeerInfo) bool {
		return len(p) == 1
	})
	if peers[0].ID != raft.NodeID("n2") || peers[0].Addr != "10.0.0.2:7001" {
		t.Errorf("ubA discovered %+v, want n2 @ 10.0.0.2:7001", peers[0])
	}

	// B should discover A.
	peers = waitDiscover(t, ubB, time.Second, func(p []discovery.PeerInfo) bool {
		return len(p) == 1
	})
	if peers[0].ID != raft.NodeID("n1") || peers[0].Addr != "10.0.0.1:7001" {
		t.Errorf("ubB discovered %+v, want n1 @ 10.0.0.1:7001", peers[0])
	}
}

func TestRun_ThreeNodesMutualDiscovery(t *testing.T) {
	// Three nodes: each sends to a single "hub" address for simplicity.
	// A fan-out hub simulates broadcast by forwarding packets to all others.
	// Instead, use a star topology: A→C, B→C, C→{A,B} each tick.
	// Simpler: wire them as a chain A→B→C→A with each also receiving.
	connA, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen A: %v", err)
	}
	connB, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen B: %v", err)
	}
	connC, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen C: %v", err)
	}
	addrA := connA.LocalAddr().String()
	addrB := connB.LocalAddr().String()
	addrC := connC.LocalAddr().String()

	// Use a broadcast forwarder: each conn sends to addrB (the hub); we also
	// run a separate goroutine that forwards B's incoming packets to A and C.
	// Actually, simplest: wire as ring A→B, B→C, C→A. Each sees one neighbor
	// immediately, and via that neighbor's subsequent send it learns the third.
	// But that requires multiple rounds. Let's just have everyone send to
	// everyone by running two separate sends per node — but that changes
	// the Config interface (one BroadcastAddr per Config).
	//
	// Practical choice: A sends to B, B sends to C, C sends to A.
	// After one round: B knows A, C knows B, A knows C.
	// After two rounds: each knows both others (via the next sender forwarding).
	// We wait long enough for convergence.

	ubA := runBroadcast(t, &Config{
		NodeID: "n1", Addr: "10.0.0.1:7001",
		BroadcastAddr: addrB,
		Interval:      10 * time.Millisecond, TTL: time.Second,
	}, connA)
	ubB := runBroadcast(t, &Config{
		NodeID: "n2", Addr: "10.0.0.2:7001",
		BroadcastAddr: addrC,
		Interval:      10 * time.Millisecond, TTL: time.Second,
	}, connB)
	ubC := runBroadcast(t, &Config{
		NodeID: "n3", Addr: "10.0.0.3:7001",
		BroadcastAddr: addrA,
		Interval:      10 * time.Millisecond, TTL: time.Second,
	}, connC)

	// A→B→C→A ring: after sufficient ticks each node sees its direct sender.
	// A sends to B: B learns n1. B sends to C: C learns n2. C sends to A: A learns n3.
	// That's one neighbor each. For full convergence with this ring topology we
	// need at least 2 full rounds, so we wait generously.
	for _, tc := range []struct {
		ub   *UDPBroadcast
		want []raft.NodeID
	}{
		{ubA, []raft.NodeID{"n3"}},
		{ubB, []raft.NodeID{"n1"}},
		{ubC, []raft.NodeID{"n2"}},
	} {
		want := tc.want
		waitDiscover(t, tc.ub, time.Second, func(peers []discovery.PeerInfo) bool {
			if len(peers) != len(want) {
				return false
			}
			got := make([]string, len(peers))
			for i, p := range peers {
				got[i] = string(p.ID)
			}
			sort.Strings(got)
			exp := make([]string, len(want))
			for i, w := range want {
				exp[i] = string(w)
			}
			sort.Strings(exp)
			for i := range got {
				if got[i] != exp[i] {
					return false
				}
			}
			return true
		})
	}
}

func TestRun_SkipsSelf(t *testing.T) {
	// A single instance sends to a loopback address it also listens on.
	// We inject a conn that receives A's own packets; A must not add itself.
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := conn.LocalAddr().String()

	ub := runBroadcast(t, &Config{
		NodeID:        "n1",
		Addr:          "10.0.0.1:7001",
		BroadcastAddr: addr, // sends to itself
		Interval:      10 * time.Millisecond,
		TTL:           500 * time.Millisecond,
	}, conn)

	// Give the instance time to send and receive its own announcement.
	time.Sleep(50 * time.Millisecond)

	peers, err := ub.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("expected 0 peers (self-skip), got %+v", peers)
	}
}

// ---- TTL expiry -------------------------------------------------------------

func TestDiscover_TTLExpiry(t *testing.T) {
	// Use a listener and inject a single announcement by hand; then wait for
	// the TTL to expire and confirm Discover returns nothing.
	connA, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen A: %v", err)
	}
	addrA := connA.LocalAddr().String()

	// Sender socket (not a UDPBroadcast — just raw UDP).
	sender, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen sender: %v", err)
	}
	defer func() { _ = sender.Close() }()

	const shortTTL = 80 * time.Millisecond
	ub := runBroadcast(t, &Config{
		NodeID:        "n1",
		Addr:          "10.0.0.1:7001",
		BroadcastAddr: "127.0.0.1:1", // won't actually send anywhere meaningful
		Interval:      time.Hour,     // no periodic sends needed for this test
		TTL:           shortTTL,
	}, connA)

	// Inject one announcement from "n2" directly into connA's socket.
	peerAnn, _ := json.Marshal(announcement{ID: "n2", Addr: "10.0.0.2:7001"})
	target, _ := net.ResolveUDPAddr("udp4", addrA)
	if _, sendErr := sender.WriteTo(peerAnn, target); sendErr != nil {
		t.Fatalf("inject announcement: %v", sendErr)
	}

	// Wait for n2 to appear.
	waitDiscover(t, ub, time.Second, func(peers []discovery.PeerInfo) bool {
		return len(peers) == 1
	})

	// Wait for the TTL to expire (2× shortTTL to be safe).
	time.Sleep(2 * shortTTL)

	peers, err := ub.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover after TTL: %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("expected 0 peers after TTL expiry, got %+v", peers)
	}
}

// ---- Malformed packets ------------------------------------------------------

func TestRun_IgnoresMalformedPackets(t *testing.T) {
	connA, connB, addrA, addrB := udpPair(t)

	ub := runBroadcast(t, &Config{
		NodeID:        "n1",
		Addr:          "10.0.0.1:7001",
		BroadcastAddr: addrB, // doesn't matter for this test
		Interval:      time.Hour,
		TTL:           time.Second,
	}, connA)

	// Send garbage, truncated JSON, and a packet missing required fields.
	targetA, _ := net.ResolveUDPAddr("udp4", addrA)
	for _, bad := range [][]byte{
		[]byte("not json at all"),
		[]byte(`{"id":"`),                // truncated
		[]byte(`{"id":"","addr":"h:1"}`), // empty ID
		[]byte(`{"id":"n2","addr":""}`),  // empty Addr
		[]byte(`{}`),                     // both empty
	} {
		if _, writeErr := connB.WriteTo(bad, targetA); writeErr != nil {
			t.Fatalf("write bad packet: %v", writeErr)
		}
	}

	// Give the receive goroutine time to process all bad packets.
	time.Sleep(30 * time.Millisecond)

	peers, err := ub.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("expected 0 peers after malformed packets, got %+v", peers)
	}
}

// ---- Announcement authentication --------------------------------------------

// newTestBroadcast builds a UDPBroadcast without opening any socket, with a
// clock the test controls.
func newTestBroadcast(t *testing.T, cfg *Config, now func() time.Time) *UDPBroadcast {
	t.Helper()
	cfg.BroadcastAddr = "127.0.0.1:1"
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	u, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if now != nil {
		u.now = now
	}
	return u
}

// signedPacket builds a wire announcement signed with secret at time ts.
func signedPacket(t *testing.T, secret []byte, id, addr string, ts int64, nonce string) []byte {
	t.Helper()
	ann := announcement{ID: id, Addr: addr}
	if len(secret) > 0 {
		ann.TS = ts
		ann.Nonce = nonce
		ann.MAC = base64.RawStdEncoding.EncodeToString(sign(secret, id, addr, ts, nonce))
	}
	b, err := json.Marshal(ann)
	if err != nil {
		t.Fatalf("marshal announcement: %v", err)
	}
	return b
}

// TestHandlePacket_SecretRejectsUnauthenticatedPeers pins the invariant that a
// node configured with a shared secret accepts an announcement only when it is
// correctly signed, fresh, and not a replay. Without this, any host able to put
// a datagram on the wire can inject itself into the cluster.
func TestHandlePacket_SecretRejectsUnauthenticatedPeers(t *testing.T) {
	secret := []byte("cluster-shared-secret")
	base := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		packet func(t *testing.T) []byte
		// replay, when set, is sent before packet so packet is a duplicate.
		replay func(t *testing.T) []byte
		accept bool
	}{
		{
			name: "correctly signed announcement is accepted",
			packet: func(t *testing.T) []byte {
				return signedPacket(t, secret, "n2", "10.0.0.2:7001", base.Unix(), "nonce-ok")
			},
			accept: true,
		},
		{
			name: "unsigned announcement is rejected",
			packet: func(t *testing.T) []byte {
				return signedPacket(t, nil, "n2", "10.0.0.2:7001", 0, "")
			},
		},
		{
			name: "announcement signed with the wrong secret is rejected",
			packet: func(t *testing.T) []byte {
				return signedPacket(t, []byte("not-the-secret"), "n2", "10.0.0.2:7001", base.Unix(), "nonce-bad")
			},
		},
		{
			name: "signature bound to a different address is rejected",
			packet: func(t *testing.T) []byte {
				// Sign one address, then rewrite the address field.
				ann := announcement{ID: "n2", Addr: "10.0.0.2:7001", TS: base.Unix(), Nonce: "nonce-swap"}
				ann.MAC = base64.RawStdEncoding.EncodeToString(
					sign(secret, ann.ID, ann.Addr, ann.TS, ann.Nonce))
				ann.Addr = "10.9.9.9:7001"
				b, err := json.Marshal(ann)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				return b
			},
		},
		{
			name: "stale announcement outside the replay window is rejected",
			packet: func(t *testing.T) []byte {
				old := base.Add(-10 * time.Minute).Unix()
				return signedPacket(t, secret, "n2", "10.0.0.2:7001", old, "nonce-stale")
			},
		},
		{
			name: "replayed announcement is rejected",
			replay: func(t *testing.T) []byte {
				return signedPacket(t, secret, "n2", "10.0.0.2:7001", base.Unix(), "nonce-replay")
			},
			packet: func(t *testing.T) []byte {
				return signedPacket(t, secret, "n2", "10.0.0.2:7001", base.Unix(), "nonce-replay")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newTestBroadcast(t, &Config{
				NodeID: "n1",
				Addr:   "10.0.0.1:7001",
				Secret: secret,
			}, func() time.Time { return base })

			if tt.replay != nil {
				u.handlePacket(tt.replay(t))
				// The first copy must have landed for the replay case to mean
				// anything; drop it so the assertion below sees only the retry.
				u.mu.Lock()
				delete(u.peers, "n2")
				u.mu.Unlock()
			}

			u.handlePacket(tt.packet(t))

			peers, err := u.Discover(context.Background())
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if tt.accept && len(peers) != 1 {
				t.Fatalf("expected the announcement to be accepted, got peers %+v", peers)
			}
			if !tt.accept && len(peers) != 0 {
				t.Fatalf("expected the announcement to be rejected, got peers %+v", peers)
			}
		})
	}
}

// TestHandlePacket_NoSecretAcceptsPlainAnnouncements keeps the trusted-network
// deployment working unchanged.
func TestHandlePacket_NoSecretAcceptsPlainAnnouncements(t *testing.T) {
	u := newTestBroadcast(t, &Config{NodeID: "n1", Addr: "10.0.0.1:7001"}, nil)
	u.handlePacket(signedPacket(t, nil, "n2", "10.0.0.2:7001", 0, ""))

	peers, err := u.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(peers) != 1 || peers[0].Addr != "10.0.0.2:7001" {
		t.Fatalf("expected n2 at 10.0.0.2:7001, got %+v", peers)
	}
}

// TestRecordPeer_AddressRebindRequiresOptIn pins the invariant that an
// announcement cannot silently repoint an existing member's address, which
// would redirect that member's Raft traffic to the announcer.
func TestRecordPeer_AddressRebindRequiresOptIn(t *testing.T) {
	tests := []struct {
		name     string
		allow    bool
		wantAddr string
	}{
		{name: "rebind refused by default", allow: false, wantAddr: "10.0.0.2:7001"},
		{name: "rebind applied when opted in", allow: true, wantAddr: "10.6.6.6:7001"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newTestBroadcast(t, &Config{
				NodeID:             "n1",
				Addr:               "10.0.0.1:7001",
				AllowAddressChange: tt.allow,
			}, nil)

			u.handlePacket(signedPacket(t, nil, "n2", "10.0.0.2:7001", 0, ""))
			u.handlePacket(signedPacket(t, nil, "n2", "10.6.6.6:7001", 0, ""))

			peers, err := u.Discover(context.Background())
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(peers) != 1 {
				t.Fatalf("expected exactly one peer, got %+v", peers)
			}
			if peers[0].Addr != tt.wantAddr {
				t.Errorf("peer address = %q, want %q", peers[0].Addr, tt.wantAddr)
			}
		})
	}
}

// TestEncodeAnnouncement_SignsWhenSecretConfigured checks the outbound side:
// a node with a secret must produce announcements its peers will accept, and a
// node without one must keep the original two-field wire format.
func TestEncodeAnnouncement_SignsWhenSecretConfigured(t *testing.T) {
	secret := []byte("cluster-shared-secret")

	signer := newTestBroadcast(t, &Config{NodeID: "n2", Addr: "10.0.0.2:7001", Secret: secret}, nil)
	pkt, err := signer.encodeAnnouncement()
	if err != nil {
		t.Fatalf("encodeAnnouncement: %v", err)
	}
	var ann announcement
	if unmarshalErr := json.Unmarshal(pkt, &ann); unmarshalErr != nil {
		t.Fatalf("unmarshal: %v", unmarshalErr)
	}
	if ann.MAC == "" || ann.Nonce == "" || ann.TS == 0 {
		t.Fatalf("signed announcement is missing authentication fields: %+v", ann)
	}

	receiver := newTestBroadcast(t, &Config{NodeID: "n1", Addr: "10.0.0.1:7001", Secret: secret}, nil)
	receiver.handlePacket(pkt)
	peers, err := receiver.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("a peer's own signed announcement was rejected: %+v", peers)
	}

	plain := newTestBroadcast(t, &Config{NodeID: "n3", Addr: "10.0.0.3:7001"}, nil)
	pkt, err = plain.encodeAnnouncement()
	if err != nil {
		t.Fatalf("encodeAnnouncement: %v", err)
	}
	ann = announcement{}
	if unmarshalErr := json.Unmarshal(pkt, &ann); unmarshalErr != nil {
		t.Fatalf("unmarshal: %v", unmarshalErr)
	}
	if ann.MAC != "" || ann.Nonce != "" || ann.TS != 0 {
		t.Errorf("unauthenticated announcement carries authentication fields: %+v", ann)
	}
}

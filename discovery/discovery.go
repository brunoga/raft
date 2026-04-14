// Package discovery defines the Discovery interface and related types for
// dynamic peer address resolution in a Raft cluster.
//
// In static deployments nodes are registered once via Transport.AddPeer; in
// dynamic environments (containers, Kubernetes, cloud auto-scaling) peer
// addresses change at runtime. Discovery decouples address resolution from Raft
// semantics: the raft core never sees it, only the transport layer does.
//
// # Peer removal
//
// The DiscoveryAgent intentionally never removes peers. A peer that disappears
// from a Discover result could be a transient DNS hiccup, a rolling restart, or
// a genuine departure — they are indistinguishable at the network level. Actual
// removal must go through the Raft membership-change protocol (RemoveServer).
// The agent only adds; operators remove.
package discovery

import (
	"context"
	"log/slog"
	"time"

	"github.com/brunoga/raft"
)

// PeerInfo identifies a single Raft peer.
type PeerInfo struct {
	ID   raft.NodeID
	Addr string // host:port
}

// Discovery resolves the current set of cluster peers.
// Implementations must be safe to call concurrently.
type Discovery interface {
	Discover(ctx context.Context) ([]PeerInfo, error)
}

// PeerAdder is the subset of Transport needed by DiscoveryAgent.
// GRPCTransport satisfies this interface directly.
type PeerAdder interface {
	AddPeer(id raft.NodeID, addr string)
}

// DiscoveryAgent bridges a Discovery implementation and a PeerAdder (typically
// a GRPCTransport). It calls Discover once on start to seed the peer table, then
// re-polls every interval to pick up newly arrived peers.
//
// The agent never removes peers; see the package-level documentation for the
// rationale.
type DiscoveryAgent struct {
	discovery Discovery
	transport PeerAdder
	interval  time.Duration
	logger    *slog.Logger
}

// NewAgent creates a DiscoveryAgent. interval is how often to re-poll after the
// initial seed; a value of zero disables background polling (one-shot seed only).
func NewAgent(d Discovery, t PeerAdder, interval time.Duration) *DiscoveryAgent {
	return &DiscoveryAgent{
		discovery: d,
		transport: t,
		interval:  interval,
		logger:    slog.Default().With("component", "discovery"),
	}
}

// Run seeds the peer table immediately and then re-polls on interval until ctx
// is cancelled. It returns ctx.Err() when the context is done.
// Transient Discover errors are logged and retried; they do not stop the agent.
func (a *DiscoveryAgent) Run(ctx context.Context) error {
	a.poll(ctx)

	if a.interval <= 0 {
		<-ctx.Done()
		return ctx.Err()
	}

	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			a.poll(ctx)
		}
	}
}

// poll calls Discover once and registers any new peers with the transport.
func (a *DiscoveryAgent) poll(ctx context.Context) {
	peers, err := a.discovery.Discover(ctx)
	if err != nil {
		a.logger.Warn("discover error (will retry)", "err", err)
		return
	}
	for _, p := range peers {
		a.transport.AddPeer(p.ID, p.Addr)
	}
}

// Static returns a Discovery that always resolves to the given fixed peer list.
// It performs no I/O and is safe for concurrent use. It is the recommended
// implementation for static clusters and unit tests.
func Static(peers []PeerInfo) Discovery {
	// Copy to prevent the caller from mutating the slice after construction.
	cp := make([]PeerInfo, len(peers))
	copy(cp, peers)
	return staticDiscovery(cp)
}

type staticDiscovery []PeerInfo

func (s staticDiscovery) Discover(_ context.Context) ([]PeerInfo, error) {
	return []PeerInfo(s), nil
}

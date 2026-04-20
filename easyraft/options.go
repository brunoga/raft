package easyraft

import (
	"crypto/tls"
	"log/slog"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/discovery"
	"github.com/prometheus/client_golang/prometheus"
)

// Config holds the configuration for an EasyRaft node.
type Config struct {
	ID        raft.NodeID
	RaftAddr  string
	HTTPAddr  string
	DataDir   string
	Peers     map[raft.NodeID]string
	Logger    *slog.Logger
	SnapCount uint64

	Discovery         discovery.Discovery
	DiscoveryInterval time.Duration
	TLS               *tls.Config
	PromRegisterer    prometheus.Registerer

	// JoinAddrs are HTTP addresses of existing cluster nodes to contact on
	// startup. The joining node POSTs its ID and Raft address to each seed in
	// turn until one succeeds. The seed registers this node with the Raft
	// cluster and returns the current peer list so the joiner can seed its
	// own transport. Set via [WithJoinAddr].
	JoinAddrs []string

	// Raft timing overrides. Zero means use the easyraft defaults
	// (100 ms tick/heartbeat, 1–2 s election timeout).
	TickInterval       time.Duration
	HeartbeatInterval  time.Duration
	ElectionTimeoutMin time.Duration
	ElectionTimeoutMax time.Duration
}

// Option configures an EasyRaft node.
type Option func(*Config)

// WithID sets the Raft node ID.
func WithID(id raft.NodeID) Option {
	return func(c *Config) { c.ID = id }
}

// WithRaftAddr sets the listen address for Raft RPCs (e.g., ":7001").
func WithRaftAddr(addr string) Option {
	return func(c *Config) { c.RaftAddr = addr }
}

// WithHTTPAddr sets the optional listen address for the HTTP API (e.g., ":8001").
func WithHTTPAddr(addr string) Option {
	return func(c *Config) { c.HTTPAddr = addr }
}

// WithDataDir sets the directory for persistent log and snapshots.
func WithDataDir(dir string) Option {
	return func(c *Config) { c.DataDir = dir }
}

// WithPeers adds a static list of initial peers. The map keys are node IDs
// and the values are host:port addresses for Raft RPC (e.g. "127.0.0.1:7002").
// WithPeers may be called multiple times; maps are merged.
// Peers are pre-populated as known Raft members, so discovery will not issue
// redundant AddServer calls for them.
func WithPeers(peers map[raft.NodeID]string) Option {
	return func(c *Config) {
		if c.Peers == nil {
			c.Peers = make(map[raft.NodeID]string)
		}
		for id, addr := range peers {
			c.Peers[id] = addr
		}
	}
}

// WithLogger sets a custom logger.
func WithLogger(logger *slog.Logger) Option {
	return func(c *Config) { c.Logger = logger }
}

// WithSnapCount sets the number of log entries between snapshots.
func WithSnapCount(count uint64) Option {
	return func(c *Config) {
		c.SnapCount = count
	}
}

// WithDiscovery enables dynamic peer discovery. EasyRaft polls d every
// interval: for each returned peer it calls tr.AddPeer (transport) and, if the
// peer is not already a known cluster member, node.AddServer (Raft membership).
// AddServer calls that fail because this node is not the leader are retried on
// the next poll. A zero interval defaults to 30 s.
//
// Static peers from [WithPeers] are treated as already-known members and will
// not trigger AddServer calls.
func WithDiscovery(d discovery.Discovery, interval time.Duration) Option {
	return func(c *Config) {
		c.Discovery = d
		c.DiscoveryInterval = interval
	}
}

// WithJoinAddr sets the HTTP address(es) of existing cluster nodes to contact
// on startup for cluster joining. The joining node will POST its ID and Raft
// address to each seed in turn until one succeeds; the seed will call
// AddServer on behalf of the joiner and return the current peer list.
//
// Requires that the seed nodes have [WithHTTPAddr] configured. May be called
// multiple times; addresses are appended.
func WithJoinAddr(addrs ...string) Option {
	return func(c *Config) {
		c.JoinAddrs = append(c.JoinAddrs, addrs...)
	}
}

// WithTLS sets the TLS configuration for Raft RPCs.
func WithTLS(tlsCfg *tls.Config) Option {
	return func(c *Config) {
		c.TLS = tlsCfg
	}
}

// WithRaftTiming overrides the Raft protocol timing parameters.
// Zero values are ignored and the easyraft defaults are used instead
// (100 ms tick/heartbeat, 1 s / 2 s election timeout).
func WithRaftTiming(tick, heartbeat, electionMin, electionMax time.Duration) Option {
	return func(c *Config) {
		c.TickInterval = tick
		c.HeartbeatInterval = heartbeat
		c.ElectionTimeoutMin = electionMin
		c.ElectionTimeoutMax = electionMax
	}
}

// WithPrometheus sets the Prometheus registerer.
func WithPrometheus(reg prometheus.Registerer) Option {
	return func(c *Config) {
		c.PromRegisterer = reg
	}
}

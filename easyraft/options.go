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

// WithPeers adds a static list of initial peers.
// Each peer should be in the form "id=address" (e.g. "n2=127.0.0.1:7002").
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

// WithDiscovery sets a custom discovery mechanism.
func WithDiscovery(d discovery.Discovery, interval time.Duration) Option {
	return func(c *Config) {
		c.Discovery = d
		c.DiscoveryInterval = interval
	}
}

// WithTLS sets the TLS configuration for Raft RPCs.
func WithTLS(tlsCfg *tls.Config) Option {
	return func(c *Config) {
		c.TLS = tlsCfg
	}
}

// WithPrometheus sets the Prometheus registerer.
func WithPrometheus(reg prometheus.Registerer) Option {
	return func(c *Config) {
		c.PromRegisterer = reg
	}
}

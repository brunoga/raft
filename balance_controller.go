package raft

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// NodeProvider abstracts a single physical node's contribution to the global
// rebalancing view. It can report the status of all groups it manages and
// execute leadership transfers on their behalf.
//
// *Manager satisfies this interface out of the box.
type NodeProvider interface {
	// StatusAll returns a point-in-time snapshot of every group on this node.
	StatusAll() []GroupStatus
	// TransferGroupLeadership transfers leadership of groupID to the Raft node
	// identified by to.
	TransferGroupLeadership(ctx context.Context, groupID uint64, to NodeID) error
}

// BalanceController periodically builds a global view from a set of
// NodeProviders, computes a rebalance plan using the configured Balancer, and
// executes transfers. It runs one iteration per Interval until the context is
// cancelled.
//
// At most one transfer per group is in-flight at a time; groups with a pending
// transfer are skipped in subsequent intervals until the transfer completes (or
// fails). Failed transfers are logged and retried on the next interval.
type BalanceController struct {
	providers map[NodeID]NodeProvider // physID → provider
	balancer  Balancer
	interval  time.Duration
	logger    *slog.Logger

	mu       sync.Mutex
	inflight map[uint64]bool // groupID → transfer in-flight
}

// NewBalanceController creates a BalanceController that rebalances leaders
// across providers every interval using b. providers maps each physical-node
// identifier (used as the view key in Balancer.Plan) to the NodeProvider for
// that node.
func NewBalanceController(providers map[NodeID]NodeProvider, b Balancer, interval time.Duration) *BalanceController {
	return &BalanceController{
		providers: providers,
		balancer:  b,
		interval:  interval,
		logger:    slog.Default(),
		inflight:  make(map[uint64]bool),
	}
}

// Run runs the balance control loop until ctx is cancelled. It blocks until the
// loop exits and all in-flight transfer goroutines have returned.
func (c *BalanceController) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

// runOnce collects the global view, computes a plan, and fires goroutines for
// each transfer that is not already in-flight.
func (c *BalanceController) runOnce(ctx context.Context) {
	// Collect status from every physical node.
	view := make(map[NodeID][]GroupStatus, len(c.providers))
	for physID, p := range c.providers {
		view[physID] = p.StatusAll()
	}

	plan := c.balancer.Plan(view)
	if len(plan) == 0 {
		return
	}

	// Build a reverse index: Raft NodeID → physID, for quick provider lookup.
	nodeToPhys := make(map[NodeID]NodeID)
	for physID, statuses := range view {
		for _, s := range statuses {
			nodeToPhys[s.NodeID] = physID
		}
	}

	for _, tr := range plan {
		// Skip if a transfer for this group is already in-flight.
		c.mu.Lock()
		if c.inflight[tr.GroupID] {
			c.mu.Unlock()
			continue
		}
		c.inflight[tr.GroupID] = true
		c.mu.Unlock()

		// Locate the provider that currently hosts the leader.
		physID, ok := nodeToPhys[tr.From]
		if !ok {
			c.mu.Lock()
			delete(c.inflight, tr.GroupID)
			c.mu.Unlock()
			c.logger.Warn("balance: leader node not found in view; skipping",
				"group", tr.GroupID, "from", tr.From)
			continue
		}
		provider, ok := c.providers[physID]
		if !ok {
			c.mu.Lock()
			delete(c.inflight, tr.GroupID)
			c.mu.Unlock()
			c.logger.Warn("balance: no provider for physical node; skipping",
				"group", tr.GroupID, "physID", physID)
			continue
		}

		go func(prov NodeProvider, t Transfer) {
			defer func() {
				c.mu.Lock()
				delete(c.inflight, t.GroupID)
				c.mu.Unlock()
			}()
			if err := prov.TransferGroupLeadership(ctx, t.GroupID, t.To); err != nil {
				c.logger.Warn("balance: transfer failed; will retry next interval",
					"group", t.GroupID, "from", t.From, "to", t.To, "err", err)
			}
		}(provider, tr)
	}
}

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
	// Implementations that reach the node over a network must honour ctx: a
	// provider that never answers otherwise stalls rebalancing for the whole
	// cluster, not just for its own node.
	StatusAll(ctx context.Context) []GroupStatus
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

	statusTimeout   time.Duration
	transferTimeout time.Duration
	cooldown        time.Duration

	transfers sync.WaitGroup

	mu       sync.Mutex
	inflight map[uint64]bool      // groupID → transfer in-flight
	lastMove map[uint64]time.Time // groupID → when its last transfer finished
}

// BalanceOption configures a BalanceController.
type BalanceOption func(*BalanceController)

// WithBalanceLogger sets the logger used for rebalancing decisions.
func WithBalanceLogger(l *slog.Logger) BalanceOption {
	return func(c *BalanceController) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithStatusTimeout bounds how long one rebalancing round waits for a node to
// report its groups. A node that does not answer in time is left out of that
// round rather than holding up the whole cluster's rebalancing.
//
// Default: 5 seconds.
func WithStatusTimeout(d time.Duration) BalanceOption {
	return func(c *BalanceController) {
		if d > 0 {
			c.statusTimeout = d
		}
	}
}

// WithTransferTimeout bounds a single leadership transfer. Without a bound, a
// transfer to a node that has stopped answering never returns, and that group
// is excluded from rebalancing for the lifetime of the process.
//
// Default: 30 seconds.
func WithTransferTimeout(d time.Duration) BalanceOption {
	return func(c *BalanceController) {
		if d > 0 {
			c.transferTimeout = d
		}
	}
}

// WithGroupCooldown sets how long a group is left alone after a transfer
// attempt before it may be moved again.
//
// Rebalancing decisions are made from a view assembled one node at a time, and
// a cluster may be running more than one controller. Without a cooldown, two
// views that disagree can hand leadership back and forth between the same two
// nodes indefinitely, and each move costs an election.
//
// Default: 5 × the rebalancing interval.
func WithGroupCooldown(d time.Duration) BalanceOption {
	return func(c *BalanceController) {
		if d > 0 {
			c.cooldown = d
		}
	}
}

// NewBalanceController creates a BalanceController that rebalances leaders
// across providers every interval using b. providers maps each physical-node
// identifier (used as the view key in Balancer.Plan) to the NodeProvider for
// that node.
func NewBalanceController(providers map[NodeID]NodeProvider, b Balancer, interval time.Duration, opts ...BalanceOption) *BalanceController {
	c := &BalanceController{
		providers:       providers,
		balancer:        b,
		interval:        interval,
		logger:          slog.Default(),
		statusTimeout:   5 * time.Second,
		transferTimeout: 30 * time.Second,
		cooldown:        5 * interval,
		inflight:        make(map[uint64]bool),
		lastMove:        make(map[uint64]time.Time),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Run runs the balance control loop until ctx is cancelled. It blocks until the
// loop exits and all in-flight transfer goroutines have returned.
func (c *BalanceController) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	defer c.transfers.Wait()
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
	// Collect status from every physical node, in parallel and under a bound.
	// Sequential unbounded collection lets one unreachable node stall
	// rebalancing for every group in the cluster.
	statusCtx, cancel := context.WithTimeout(ctx, c.statusTimeout)
	defer cancel()

	var (
		viewMu sync.Mutex
		wg     sync.WaitGroup
	)
	view := make(map[NodeID][]GroupStatus, len(c.providers))
	for physID, p := range c.providers {
		wg.Add(1)
		go func(id NodeID, prov NodeProvider) {
			defer wg.Done()
			statuses := prov.StatusAll(statusCtx)
			if statuses == nil {
				return
			}
			viewMu.Lock()
			view[id] = statuses
			viewMu.Unlock()
		}(physID, p)
	}
	wg.Wait()

	if len(view) < len(c.providers) {
		// Planning from a partial view moves leaders towards nodes that merely
		// failed to answer. Wait for a round where everyone reports.
		c.logger.Warn("balance: skipping round, not every node reported",
			"reported", len(view), "expected", len(c.providers))
		return
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

	now := time.Now()
	for _, tr := range plan {
		// Skip if a transfer for this group is already in-flight, or if it was
		// moved recently: repeatedly moving the same group costs an election
		// each time and is the shape oscillation takes when two views disagree.
		c.mu.Lock()
		if c.inflight[tr.GroupID] {
			c.mu.Unlock()
			continue
		}
		if last, ok := c.lastMove[tr.GroupID]; ok && now.Sub(last) < c.cooldown {
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

		c.transfers.Add(1)
		go func(prov NodeProvider, t Transfer) {
			defer c.transfers.Done()
			defer func() {
				c.mu.Lock()
				delete(c.inflight, t.GroupID)
				c.lastMove[t.GroupID] = time.Now()
				c.mu.Unlock()
			}()
			transferCtx, cancelTransfer := context.WithTimeout(ctx, c.transferTimeout)
			defer cancelTransfer()
			if err := prov.TransferGroupLeadership(transferCtx, t.GroupID, t.To); err != nil {
				c.logger.Warn("balance: transfer failed; will retry after the cooldown",
					"group", t.GroupID, "from", t.From, "to", t.To, "err", err)
			}
		}(provider, tr)
	}
}

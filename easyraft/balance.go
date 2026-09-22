package easyraft

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/brunoga/raft/v2"
)

// balancePath is where a Manager serves the two endpoints a remote balance
// controller needs, under the reserved prefix so nothing a caller can create
// collides with it.
const balancePath = "/__balance"

// credentialTransport adds the Authorization header to every request a
// balance provider makes.
//
// The provider in the engine takes an http.Client and nothing else, which is
// the right shape: a round tripper is how a client carries a credential, and
// putting a header parameter on the constructor would have made every other
// caller pass an empty one.
type credentialTransport struct {
	credential string
	base       http.RoundTripper
}

func (t *credentialTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Cloned, because a RoundTripper may not modify the request it is given.
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", t.credential)
	return t.base.RoundTrip(clone)
}

// startBalancing launches the leader balance controller, if one is
// configured. The returned function blocks until it has stopped.
func (m *Manager) startBalancing() (wait func(), err error) {
	hosts := m.cfg.BalanceHosts
	if len(hosts) == 0 {
		return func() {}, nil
	}

	self := raft.HostID(m.cfg.ID)
	if _, ok := hosts[self]; !ok {
		return nil, fmt.Errorf("easyraft: WithLeaderBalancing lists %d hosts but not this one "+
			"(%q); a controller planning from a view that leaves out its own node moves "+
			"leadership away from it every round", len(hosts), self)
	}
	if len(hosts) < 2 {
		return nil, fmt.Errorf("easyraft: WithLeaderBalancing needs at least two hosts to " +
			"balance between")
	}
	if m.cfg.HTTPAddr == "" {
		return nil, fmt.Errorf("easyraft: WithLeaderBalancing needs WithHTTPAddr: the other " +
			"hosts reach this one's balance endpoints over HTTP, and without a listener " +
			"they would leave it out of every round")
	}

	interval := m.cfg.BalanceInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	client := &http.Client{Timeout: 10 * time.Second}
	if m.cfg.HTTPCredential != "" {
		client.Transport = &credentialTransport{
			credential: m.cfg.HTTPCredential,
			base:       http.DefaultTransport,
		}
	}

	providers := make(map[raft.HostID]raft.NodeProvider, len(hosts))
	for host, addr := range hosts {
		if host == self {
			// Asked directly. A node that went through its own HTTP server to
			// learn about itself would drop out of a round whenever that
			// server was busy, and plan its own leaders away.
			providers[host] = m
			continue
		}
		base, ok := balanceBaseURL(addr, m.cfg.HTTPTLS != nil)
		if !ok {
			return nil, fmt.Errorf("easyraft: WithLeaderBalancing: host %q has the address "+
				"%q, which is not a usable host:port", host, addr)
		}
		providers[host] = raft.NewHTTPNodeProvider(base, client)
	}

	opts := append([]raft.BalanceOption{raft.WithBalanceLogger(m.logger())}, m.cfg.BalanceOptions...)
	controller := raft.NewBalanceController(providers, raft.LeastLeadersBalancer{}, interval, opts...)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		controller.Run(m.stopCtx)
	}()
	m.logger().Info("easyraft: balancing group leaders",
		"hosts", len(hosts), "interval", interval)
	return wg.Wait, nil
}

// balanceBaseURL turns a host's address into the base URL its balance
// endpoints live at.
func balanceBaseURL(addr string, secure bool) (string, bool) {
	hostPort, scheme, ok := normalizeHostPortScheme(addr)
	if !ok {
		return "", false
	}
	if scheme == "" {
		scheme = "http"
		if secure {
			scheme = "https"
		}
	}
	return scheme + "://" + hostPort + balancePath, true
}

// handleBalanceStatus serves GET /__balance/status: every group on this node.
func (m *Manager) handleBalanceStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, m.StatusAll(r.Context()), m.logger())
}

// handleBalanceTransfer serves POST /__balance/transfer, moving one group's
// leadership.
func (m *Manager) handleBalanceTransfer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GroupID uint64      `json:"group_id"`
		To      raft.NodeID `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), m.logger())
		return
	}
	if req.To == "" {
		writeError(w, http.StatusBadRequest, "to is required", m.logger())
		return
	}

	if err := m.TransferGroupLeadership(r.Context(), req.GroupID, req.To); err != nil {
		writeError(w, statusForError(err), err.Error(), m.logger())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

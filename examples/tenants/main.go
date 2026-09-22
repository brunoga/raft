// tenants is a multi-tenant store where every tenant is its own Raft group.
//
// One node runs many groups through an [easyraft.Manager]. Tenants are
// isolated by construction: one tenant's writes queue behind that tenant's
// log, not behind everybody's, and a tenant whose state is being snapshotted
// does not stall the rest.
//
// # What this example demonstrates
//
// It is the only example built on the easyraft Manager, and it exists for
// three things that only make sense with one:
//
//   - [easyraft.WithLeaderBalancing]: groups elect leaders independently and
//     nothing coordinates them, so a host that stayed up while others
//     restarted ends up leading most of them. The leader does the
//     replication, serves the linearizable reads and takes every write, so
//     one host doing all of it is both the bottleneck and the failure that
//     hurts most. The controller moves leadership until the counts even out.
//   - [easyraft.WithSharedWAL]: every group on a host appends to one
//     write-ahead log, so a burst of writes across tenants costs one fsync
//     rather than one each. With a group per tenant that is the difference
//     between a disk that keeps up and one that does not.
//   - The data path the Manager already serves, at
//     /groups/{groupID}/{collection}/{key}. There are no CRUD routes here:
//     the Manager has them, and [easyraft.WithHTTPMux] puts this program's
//     own route on the same port.
//
// # HTTP API
//
// This program adds one route. Everything else is easyraft's.
//
//	GET /tenants                       where each tenant lives and who leads it
//	GET /__balance/status              every group on this node (easyraft)
//	PATCH /groups/{group}/items/{key}  write an item     (easyraft)
//	GET   /groups/{group}/items/{key}  read one          (easyraft)
//	GET   /groups/{group}/items        list a tenant's   (easyraft)
//
// Use [tenants/tenantctl] rather than composing those URLs by hand; it maps a
// tenant name to its group and follows the leader.
//
// # Usage
//
//	./tenants --id n1 --raft-addr :7001 --http-addr :8001 --data-dir /tmp/t/n1 \
//	          --groups 12 --peers n1=localhost:7001,n2=localhost:7002,n3=localhost:7003 \
//	          --balance-peers n1=localhost:8001,n2=localhost:8002,n3=localhost:8003
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/easyraft"
	tenantmap "github.com/brunoga/raft/v2/examples/tenants/tenants"
	"github.com/prometheus/client_golang/prometheus"
)

// tenantView is one line of GET /tenants.
type tenantView struct {
	Tenant string      `json:"tenant"`
	Group  uint64      `json:"group"`
	Leader raft.NodeID `json:"leader,omitempty"`
	Here   bool        `json:"led_here"`
}

type server struct {
	manager *easyraft.Manager
	groups  int
	// known is the set of tenant names this node has been told about, purely
	// so GET /tenants has something to list. It is local and not replicated:
	// the mapping is a pure function of the name, so nothing depends on this
	// being complete or agreed.
	known []string
}

// handleTenants serves GET /tenants: where each named tenant lives, and which
// node leads its group.
//
// The leader is read from this node's own view of each group, which is what a
// balance controller plans over too. A node that has just lost an election
// reports what it knows; nothing here needs to be authoritative.
func (s *server) handleTenants(w http.ResponseWriter, r *http.Request) {
	// Which groups this node leads comes from its own status; who leads the
	// rest comes from each group's view of its leader, which a follower knows
	// because that is who it hears from.
	ledHere := make(map[uint64]bool)
	for _, status := range s.manager.StatusAll(r.Context()) {
		if status.State == raft.Leader {
			ledHere[status.GroupID] = true
		}
	}
	leaders := make(map[uint64]raft.NodeID)
	for _, group := range s.manager.GroupIDs() {
		store, err := s.manager.GetStore(group)
		if err != nil {
			continue
		}
		leaders[group] = store.Leader()
	}

	out := make([]tenantView, 0, len(s.known))
	for _, tenant := range s.known {
		group, err := tenantmap.GroupFor(tenant, s.groups)
		if err != nil {
			continue
		}
		out = append(out, tenantView{
			Tenant: tenant,
			Group:  group,
			Leader: leaders[group],
			Here:   ledHere[group],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tenant < out[j].Tenant })

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Groups  int          `json:"groups"`
		Tenants []tenantView `json:"tenants"`
	}{Groups: s.groups, Tenants: out})
}

func main() {
	if err := run(); err != nil {
		slog.Error("tenants: exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	id := flag.String("id", "", "Raft node ID (required)")
	raftAddr := flag.String("raft-addr", ":7001", "Raft gRPC listen address")
	httpAddr := flag.String("http-addr", ":8001", "HTTP listen address")
	dataDir := flag.String("data-dir", "", "Persistent data directory (required)")
	groups := flag.Int("groups", 8, "Number of Raft groups to spread tenants over")
	peerList := flag.String("peers", "", "Comma-separated id=raft_addr for every node (required)")
	balanceList := flag.String("balance-peers", "",
		"Comma-separated id=http_addr for every node; enables leader balancing")
	balanceEvery := flag.Duration("balance-every", 15*time.Second, "How often to rebalance leaders")
	tenantList := flag.String("tenants", "", "Comma-separated tenant names to list on GET /tenants")
	flag.Parse()

	if *id == "" || *dataDir == "" || *peerList == "" {
		fmt.Fprintln(os.Stderr, "usage: tenants --id <id> --data-dir <dir> --peers id=addr,... "+
			"[--groups 8] [--balance-peers id=http,...]")
		return errors.New("--id, --data-dir and --peers are required")
	}
	if *groups < 1 {
		return fmt.Errorf("--groups must be at least 1, got %d", *groups)
	}

	peers, err := parsePeers(*peerList)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// One mux for this program's route and every route easyraft serves, so
	// there is one port. A Manager honours WithHTTPMux exactly as a Store
	// does.
	mux := http.NewServeMux()

	opts := []easyraft.Option{
		easyraft.WithID(raft.NodeID(*id)),
		easyraft.WithRaftAddr(*raftAddr),
		easyraft.WithHTTPAddr(*httpAddr),
		easyraft.WithHTTPMux(mux),
		easyraft.WithDataDir(*dataDir),
		easyraft.WithLogger(logger),
		easyraft.WithPrometheus(prometheus.DefaultRegisterer),
		// A local demo cluster: plaintext transport and open HTTP API, on purpose.
		easyraft.WithInsecureTransportAcknowledged(),
		easyraft.WithInsecureHTTPAcknowledged(),
		// One log for every group on this host: a burst of writes across
		// tenants costs one fsync rather than one per tenant.
		easyraft.WithSharedWAL(),
	}

	if *balanceList != "" {
		hosts, balanceErr := parseBalanceHosts(*balanceList)
		if balanceErr != nil {
			return balanceErr
		}
		opts = append(opts, easyraft.WithLeaderBalancing(hosts, *balanceEvery))
	}

	manager, err := easyraft.NewManager(opts...)
	if err != nil {
		return fmt.Errorf("cannot build manager: %w", err)
	}

	// One group per tenant bucket, numbered from one.
	for group := 1; group <= *groups; group++ {
		if _, addErr := manager.AddStore(uint64(group),
			easyraft.WithDataDir(filepath.Join(*dataDir, fmt.Sprintf("g%d", group))),
			easyraft.WithPeers(peers),
		); addErr != nil {
			return fmt.Errorf("add group %d: %w", group, addErr)
		}
	}

	srv := &server{manager: manager, groups: *groups}
	if *tenantList != "" {
		srv.known = strings.Split(*tenantList, ",")
	}
	mux.HandleFunc("GET /tenants", srv.handleTenants)

	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	httpSrv := &http.Server{
		Addr:        *httpAddr,
		Handler:     mux,
		BaseContext: func(net.Listener) context.Context { return baseCtx },
	}

	if startErr := manager.Start(); startErr != nil {
		_ = manager.Stop()
		return fmt.Errorf("cannot start: %w", startErr)
	}
	defer func() {
		if stopErr := manager.Stop(); stopErr != nil {
			logger.Error("tenants: unclean shutdown", "err", stopErr)
		}
	}()

	logger.Info("tenants started", "id", *id, "groups", *groups,
		"raft", *raftAddr, "http", *httpAddr, "balancing", *balanceList != "")

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	serveErr := make(chan error, 1)
	go func() {
		if serveError := httpSrv.ListenAndServe(); serveError != nil &&
			!errors.Is(serveError, http.ErrServerClosed) {
			serveErr <- serveError
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-sigCtx.Done():
		logger.Info("tenants: shutting down")
	}

	cancelBase()
	shutCtx, cancelShut := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShut()
	if shutErr := httpSrv.Shutdown(shutCtx); shutErr != nil {
		return fmt.Errorf("http shutdown: %w", shutErr)
	}
	return nil
}

// parsePeers reads "id=host:port,..." into the peer map every group shares.
func parsePeers(list string) (map[raft.NodeID]string, error) {
	out := make(map[raft.NodeID]string)
	for _, entry := range strings.Split(list, ",") {
		name, addr, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || name == "" || addr == "" {
			return nil, fmt.Errorf("--peers entry %q is not id=host:port", entry)
		}
		out[raft.NodeID(name)] = addr
	}
	return out, nil
}

// parseBalanceHosts reads "id=http_addr,..." into the host map the balance
// controller plans over. A host's ID is the node ID its Manager was built
// with, since one Manager is one physical node.
func parseBalanceHosts(list string) (map[raft.HostID]string, error) {
	out := make(map[raft.HostID]string)
	for _, entry := range strings.Split(list, ",") {
		name, addr, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || name == "" || addr == "" {
			return nil, fmt.Errorf("--balance-peers entry %q is not id=host:port", entry)
		}
		out[raft.HostID(name)] = addr
	}
	return out, nil
}

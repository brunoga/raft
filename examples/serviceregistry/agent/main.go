// agent registers one service instance with a serviceregistry cluster and
// holds the registration for as long as it runs.
//
// It is a separate program on purpose. A registry whose entries are written
// by the registry itself needs a registration protocol, a way to authenticate
// it, and something to expire entries nobody renewed. This one has none of
// that: the agent talks to the cluster from outside using easyraft/client,
// grants its own lease, writes its own entry under it, and keeps it alive.
// When the agent stops, so does the renewal, and the entry goes.
//
// # What happens when this process dies
//
// Nothing, which is the point. Kill it with SIGKILL and no code here runs:
// the lease simply stops being renewed, expires a few seconds later, and
// every replica deletes the entry at the same point in the log. Stop it with
// ^C and it revokes the lease on the way out, which removes the entry at once
// instead of leaving it to be noticed -- the right behaviour for a planned
// stop, and the reason the two paths are written differently below.
//
// # Usage
//
//	./agent --service api --id api-1 --addr 10.0.0.1:9000 \
//	        --endpoints localhost:8001,localhost:8002,localhost:8003
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/brunoga/raft/v2/easyraft"
	"github.com/brunoga/raft/v2/easyraft/client"
	registry "github.com/brunoga/raft/v2/examples/serviceregistry/registry"
)

func main() {
	if err := run(); err != nil {
		slog.Error("agent: exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	service := flag.String("service", "", "Service name (required)")
	id := flag.String("id", "", "Instance ID, unique within the service (required)")
	addr := flag.String("addr", "", "Address other services should reach this instance on (required)")
	endpoints := flag.String("endpoints", "localhost:8001",
		"Comma-separated HTTP addresses of registry nodes")
	ttl := flag.Duration("ttl", 10*time.Second,
		"How long the registration survives without a renewal")
	flag.Parse()

	if *service == "" || *id == "" || *addr == "" {
		fmt.Fprintln(os.Stderr, "usage: agent --service <name> --id <instance> --addr <host:port> "+
			"[--endpoints host:port,...] [--ttl 10s]")
		return errors.New("--service, --id and --addr are required")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Every node is given, not just one. The client finds whichever is leader
	// and follows it when it moves; a node that is down costs one connection
	// attempt.
	c, err := client.New(client.WithEndpoints(strings.Split(*endpoints, ",")...))
	if err != nil {
		return fmt.Errorf("cannot build client: %w", err)
	}
	instances := client.Collection[registry.Instance](c, registry.CollectionName)

	// ^C and SIGTERM end the registration deliberately; anything else ends it
	// by simply ceasing to renew.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	lease, err := c.GrantLease(ctx, *ttl)
	if err != nil {
		return fmt.Errorf("cannot grant a lease: %w", err)
	}

	key := registry.Key(*service, *id)
	// Upsert rather than Create: an agent restarting after a crash finds its
	// own entry still there, within the TTL, and re-registering has to be the
	// same call as registering.
	if err := instances.UpsertWithLease(ctx, key, registry.Instance{
		Service: *service,
		ID:      *id,
		Addr:    *addr,
		Meta:    map[string]string{"pid": fmt.Sprint(os.Getpid())},
	}, lease); err != nil {
		return fmt.Errorf("cannot register: %w", err)
	}
	logger.Info("registered", "key", key, "addr", *addr, "lease", uint64(lease), "ttl", *ttl)

	// One goroutine holds the registration. KeepAliveLoop renews every third
	// of the TTL, retries the failures worth retrying -- an election is a
	// normal few hundred milliseconds in the life of a cluster -- and returns
	// as soon as the lease is actually gone.
	loopDone := make(chan error, 1)
	go func() { loopDone <- c.KeepAliveLoop(ctx, lease) }()

	select {
	case <-ctx.Done():
		logger.Info("agent: shutting down, giving up the registration")
	case err := <-loopDone:
		if errors.Is(err, easyraft.ErrLeaseNotFound) {
			// The lease expired, which means the entry is already gone: this
			// process was partitioned from the cluster for longer than the
			// TTL, or paused. A fresh lease and a fresh registration is the
			// answer, not another renewal of something that no longer exists.
			return fmt.Errorf("registration lost: %w", err)
		}
		return fmt.Errorf("keep-alive stopped: %w", err)
	}

	// A planned stop revokes rather than waiting to be noticed. The context
	// above is already cancelled, so this needs one of its own.
	revokeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.RevokeLease(revokeCtx, lease); err != nil {
		// Not fatal: the lease expires on its own within the TTL. Worth
		// saying, because until it does the registry still lists an instance
		// that has gone.
		logger.Warn("agent: could not revoke the lease; the entry will expire instead",
			"err", err, "ttl", *ttl)
		return nil
	}
	logger.Info("agent: registration removed")
	return nil
}

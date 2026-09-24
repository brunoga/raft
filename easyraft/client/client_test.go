package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/easyraft"
	"github.com/brunoga/raft/v2/easyraft/client"
	"github.com/brunoga/raft/v2/easyraft/easyrafttest"
	"github.com/brunoga/raft/v2/internal/memnet"
)

type User struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

type Counter struct {
	Value uint64 `json:"value"`
}

// serving is a cluster whose nodes also serve HTTP, plus the addresses they
// serve on. The Raft traffic still runs over the in-memory network; only the
// client's own requests are real HTTP, which is the part under test.
type serving struct {
	cluster *easyrafttest.Cluster
	addrs   []string
	net     *memnet.Network
}

func startServing(t *testing.T, n int, extra ...easyraft.Option) *serving {
	t.Helper()
	// In memory rather than on real sockets, so these tests can run inside a
	// synctest bubble: a goroutine parked in Accept on a real socket is not
	// durably blocked, and one idle listener stops a bubble's clock for good.
	nw := memnet.NewNetwork()
	addrs := make([]string, n)
	lns := make([]net.Listener, n)
	for i := range addrs {
		addrs[i] = fmt.Sprintf("n%d:8080", i+1)
		lns[i] = nw.Listen(addrs[i])
	}
	c := easyrafttest.New(t, n, easyrafttest.Options{
		Store: append([]easyraft.Option{easyraft.WithInsecureHTTPAcknowledged()}, extra...),
		PerNode: func(i int) []easyraft.Option {
			return []easyraft.Option{
				easyraft.WithHTTPAddr(addrs[i]),
				easyraft.WithHTTPListener(lns[i]),
			}
		},
	})
	return &serving{cluster: c, addrs: addrs, net: nw}
}

// client returns a Client pointed at the endpoints given, by node index.
func (s *serving) client(t *testing.T, nodes ...int) *client.Client {
	t.Helper()
	var endpoints []string
	if len(nodes) == 0 {
		endpoints = s.addrs
	} else {
		for _, i := range nodes {
			endpoints = append(endpoints, s.addrs[i])
		}
	}
	c, err := client.New(
		client.WithEndpoints(endpoints...),
		client.WithHTTPClient(s.net.HTTPClient()),
	)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

// waitAdvertised waits until every node can route a write to the leader.
//
// A follower redirects using the leader's HTTP address, which the leader
// publishes through the log after it is elected. Until that has replicated, a
// follower answers 503 and says so. Every test here starts by writing through
// each node in turn, which both waits for that and checks the redirect path
// each test then relies on.
func waitAdvertised(t *testing.T, s *serving) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for i := range s.addrs {
		warm := client.Collection[User](s.client(t, i), "warmup")
		deadline := time.Now().Add(60 * time.Second)
		var last error
		for time.Now().Before(deadline) {
			last = warm.Upsert(ctx, "k", User{})
			if last == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if last != nil {
			t.Fatalf("node %d never routed a write to the leader: %v", i, last)
		}
	}
}

// TestClient_CRUD walks the whole single-key surface against a real cluster.
func TestClient_CRUD(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startServing(t, 3)
		s.cluster.Start()
		waitAdvertised(t, s)

		c := s.client(t)
		users := client.Collection[User](c, "users")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		if err := users.Create(ctx, "alice", User{Name: "Alice", Age: 30}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := users.Create(ctx, "alice", User{Name: "Again"}); !errors.Is(err, easyraft.ErrKeyExists) {
			t.Errorf("Create on an existing key: %v, want ErrKeyExists", err)
		}

		got, readErr := users.Read(ctx, "alice")
		if readErr != nil {
			t.Fatalf("Read: %v", readErr)
		}
		if got.Name != "Alice" || got.Age != 30 {
			t.Errorf("Read returned %+v", got)
		}
		if _, staleErr := users.ReadStale(ctx, "alice"); staleErr != nil {
			t.Errorf("ReadStale: %v", staleErr)
		}
		if _, missingErr := users.Read(ctx, "nobody"); !errors.Is(missingErr, easyraft.ErrKeyNotFound) {
			t.Errorf("Read of a missing key: %v, want ErrKeyNotFound", missingErr)
		}

		if err := users.Update(ctx, "alice", User{Name: "Alice", Age: 31}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if err := users.Update(ctx, "nobody", User{}); !errors.Is(err, easyraft.ErrKeyNotFound) {
			t.Errorf("Update of a missing key: %v, want ErrKeyNotFound", err)
		}
		if err := users.Upsert(ctx, "bob", User{Name: "Bob"}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}

		all, listErr := users.List(ctx)
		if listErr != nil {
			t.Fatalf("List: %v", listErr)
		}
		if len(all) != 2 {
			t.Errorf("List returned %d users: %v", len(all), all)
		}

		if err := users.Delete(ctx, "bob"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := users.Delete(ctx, "bob"); !errors.Is(err, easyraft.ErrKeyNotFound) {
			t.Errorf("Delete of a missing key: %v, want ErrKeyNotFound", err)
		}
	})
}

// TestClient_FindsAndFollowsTheLeader is the reason this package exists. The
// client is given one endpoint, and it is deliberately a follower.
func TestClient_FindsAndFollowsTheLeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startServing(t, 3)
		s.cluster.Start()
		waitAdvertised(t, s)

		follower := (s.cluster.LeaderIndex() + 1) % 3
		c := s.client(t, follower)
		users := client.Collection[User](c, "users")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		if err := users.Create(ctx, "alice", User{Name: "Alice"}); err != nil {
			t.Fatalf("a write through a follower did not reach the leader: %v", err)
		}
		if c.Leader() == "" {
			t.Error("the client did not record which endpoint answered as leader")
		}

		// Move leadership and write again through the same client, which now has
		// a stale hint and has to be corrected by a redirect.
		oldLeader := s.cluster.LeaderIndex()
		s.cluster.Partition(oldLeader)
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if idx := s.cluster.LeaderIndex(); idx != -1 && idx != oldLeader {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if idx := s.cluster.LeaderIndex(); idx == -1 || idx == oldLeader {
			t.Fatalf("no new leader was elected; leader index is %d", idx)
		}

		// The client was given only the partitioned node's address, so give it
		// every address for this half: what is under test is that it finds the
		// new leader, not that it can reach a node that is cut off.
		c = s.client(t)
		users = client.Collection[User](c, "users")
		if err := users.Upsert(ctx, "after", User{Name: "After"}); err != nil {
			t.Fatalf("a write after a leader change failed: %v", err)
		}
	})
}

// TestClient_SkipsAnEndpointThatIsNotThere checks that a dead address in the
// list costs a connection attempt and nothing else.
func TestClient_SkipsAnEndpointThatIsNotThere(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startServing(t, 3)
		s.cluster.Start()
		waitAdvertised(t, s)

		// An address the network knows nothing about. memnet answers a dial to
		// one with ErrNoListener, which is what connection-refused is here.
		const dead = "nobody:8080"
		c, newErr := client.New(
			client.WithEndpoints(append([]string{dead}, s.addrs...)...),
			client.WithRetry(2, 10*time.Millisecond),
			client.WithHTTPClient(s.net.HTTPClient()),
		)
		if newErr != nil {
			t.Fatal(newErr)
		}
		users := client.Collection[User](c, "users")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		if err := users.Create(ctx, "alice", User{Name: "Alice"}); err != nil {
			t.Fatalf("a write with one dead endpoint first failed: %v", err)
		}
		if _, err := users.Read(ctx, "alice"); err != nil {
			t.Fatalf("Read: %v", err)
		}

		// With nothing reachable at all, the failure says so and still carries
		// what went wrong.
		unreachable, err := client.New(
			client.WithEndpoints("nobody:8080", "nobody-else:8080"),
			client.WithRetry(2, 10*time.Millisecond),
			client.WithHTTPClient(s.net.HTTPClient()),
		)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Collection[User](unreachable, "users").Read(ctx, "alice")
		if !errors.Is(err, client.ErrNoEndpoint) {
			t.Errorf("read against nothing: %v, want ErrNoEndpoint", err)
		}
	})
}

// TestClient_ExactlyOnceSurvivesARepeat is the property a retry depends on:
// the same identity replayed applies the write once.
func TestClient_ExactlyOnceSurvivesARepeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startServing(t, 3)
		counters := easyrafttest.AddCollection[Counter](s.cluster, "counters")
		counters.RegisterMutation("increment", func(cur *Counter, args []byte) (*Counter, []byte, error) {
			cur.Value++
			return cur, []byte(fmt.Sprintf("%d", cur.Value)), nil
		})
		s.cluster.Start()
		waitAdvertised(t, s)

		c := s.client(t)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		session := easyraft.NewSession("worker-7")
		remote := client.Collection[Counter](c, "counters")

		// A create replayed under one identity is the same create, not a second
		// one: an ordinary repeat would be ErrKeyExists.
		id := session.Next()
		for range 3 {
			if err := remote.Exactly(id).Create(ctx, "hits", Counter{}); err != nil {
				t.Fatalf("replayed Create: %v", err)
			}
		}

		// A mutation replayed under one identity runs once, which an ordinary
		// repeat would not.
		mutID := session.Next()
		for range 4 {
			if _, err := remote.Exactly(mutID).Mutate(ctx, "hits", "increment", nil); err != nil {
				t.Fatalf("replayed Mutate: %v", err)
			}
		}
		got, readErr := remote.Read(ctx, "hits")
		if readErr != nil {
			t.Fatalf("Read: %v", readErr)
		}
		if got.Value != 1 {
			t.Errorf("the counter is %d after one mutation replayed four times", got.Value)
		}

		// A fresh identity is a fresh write.
		if _, err := remote.Exactly(session.Next()).Mutate(ctx, "hits", "increment", nil); err != nil {
			t.Fatalf("Mutate: %v", err)
		}
		got, readErr = remote.Read(ctx, "hits")
		if readErr != nil {
			t.Fatal(readErr)
		}
		if got.Value != 2 {
			t.Errorf("the counter is %d after a second distinct mutation", got.Value)
		}

		// A sequence number below one already recorded is refused rather than
		// applied out of order.
		old := easyraft.OnceID{ClientID: "worker-7", SeqNum: 1}
		staleErr := remote.Exactly(old).Create(ctx, "stale", Counter{})
		if !errors.Is(staleErr, raft.ErrObsoleteSeqNum) && !errors.Is(staleErr, easyraft.ErrKeyExists) {
			t.Errorf("a stale sequence number gave %v", staleErr)
		}
	})
}

// TestClient_RevisionsAndConditionalWrites covers compare-and-swap end to
// end, including the ETag the revision travels in.
func TestClient_RevisionsAndConditionalWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startServing(t, 3)
		s.cluster.Start()
		waitAdvertised(t, s)

		c := s.client(t)
		users := client.Collection[User](c, "users")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		if err := users.Create(ctx, "alice", User{Name: "Alice", Age: 30}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		value, rev, revErr := users.ReadRev(ctx, "alice")
		if revErr != nil {
			t.Fatalf("ReadRev: %v", revErr)
		}
		if rev == 0 {
			t.Fatal("ReadRev returned revision 0 for a key that was just written")
		}

		value.Age = 31
		if err := users.UpdateIf(ctx, "alice", value, rev); err != nil {
			t.Fatalf("UpdateIf: %v", err)
		}
		if err := users.UpdateIf(ctx, "alice", value, rev); !errors.Is(err, easyraft.ErrRevisionMismatch) {
			t.Errorf("a stale UpdateIf gave %v, want ErrRevisionMismatch", err)
		}

		// Zero is create-if-absent.
		if err := users.UpsertIf(ctx, "alice", User{}, 0); !errors.Is(err, easyraft.ErrRevisionMismatch) {
			t.Errorf("UpsertIf(0) on an existing key gave %v", err)
		}
		if err := users.UpsertIf(ctx, "fresh", User{Name: "Fresh"}, 0); err != nil {
			t.Errorf("UpsertIf(0) on an absent key: %v", err)
		}

		_, freshRev, freshErr := users.ReadRev(ctx, "fresh")
		if freshErr != nil {
			t.Fatal(freshErr)
		}
		if err := users.DeleteIf(ctx, "fresh", freshRev-1); !errors.Is(err, easyraft.ErrRevisionMismatch) {
			t.Errorf("a stale DeleteIf gave %v", err)
		}
		if err := users.DeleteIf(ctx, "fresh", freshRev); err != nil {
			t.Errorf("DeleteIf on the current revision: %v", err)
		}
	})
}

// TestClient_ScanPagesTheCollection checks that paging works through the
// headers the server puts the cursor in.
func TestClient_ScanPagesTheCollection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startServing(t, 3)
		s.cluster.Start()
		waitAdvertised(t, s)

		c := s.client(t)
		users := client.Collection[User](c, "users")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		var want []string
		for i := range 7 {
			key := fmt.Sprintf("session/%02d", i)
			if err := users.Create(ctx, key, User{Name: key}); err != nil {
				t.Fatalf("Create %s: %v", key, err)
			}
			want = append(want, key)
		}
		if err := users.Create(ctx, "other", User{Name: "other"}); err != nil {
			t.Fatal(err)
		}

		var walked []string
		opts := client.ScanOptions{Prefix: "session/", Limit: 3}
		for pages := 0; ; pages++ {
			if pages > 10 {
				t.Fatal("the scan did not end")
			}
			page, err := users.Scan(ctx, opts)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if len(page.Items) > 3 {
				t.Fatalf("a page held %d items under a limit of 3", len(page.Items))
			}
			for _, item := range page.Items {
				walked = append(walked, item.Key)
			}
			if page.Next == "" {
				if page.Revision == 0 {
					t.Error("a page carried no X-Raft-Revision")
				}
				break
			}
			opts.After = page.Next
		}
		if fmt.Sprint(walked) != fmt.Sprint(want) {
			t.Errorf("the scan walked %v, want %v", walked, want)
		}

		if _, err := users.Scan(ctx, client.ScanOptions{Limit: -1}); err == nil {
			t.Error("a negative limit was accepted")
		}
	})
}

// TestClient_Leases covers registering something that expires, through the
// client, including the loop that holds it.
func TestClient_Leases(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startServing(t, 3, easyraft.WithKeyLeaseSweepInterval(50*time.Millisecond))
		s.cluster.Start()
		waitAdvertised(t, s)

		c := s.client(t)
		services := client.Collection[User](c, "services")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Two seconds rather than a fraction of one. What is asserted below is
		// that the keep-alive loop holds the registration past its TTL, and a
		// 400ms lease only survives a machine that never stalls for 400ms.
		const leaseTTL = 2 * time.Second
		lease, grantErr := c.GrantLease(ctx, leaseTTL)
		if grantErr != nil {
			t.Fatalf("GrantLease: %v", grantErr)
		}
		if err := services.UpsertWithLease(ctx, "web-1", User{Name: "web-1"}, lease); err != nil {
			t.Fatalf("UpsertWithLease: %v", err)
		}

		info, infoErr := c.Lease(ctx, lease)
		if infoErr != nil {
			t.Fatalf("Lease: %v", infoErr)
		}
		if len(info.Keys) != 1 || info.Keys[0].Key != "web-1" {
			t.Errorf("the lease holds %v", info.Keys)
		}
		if leases, listErr := c.Leases(ctx); listErr != nil || len(leases) == 0 {
			t.Errorf("Leases returned %v, %v", leases, listErr)
		}

		loopCtx, stopLoop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- c.KeepAliveLoop(loopCtx, lease) }()

		// Held for longer than the lease, so nothing but the loop can explain the
		// registration surviving. The lease's own deadline is read first: if it
		// has moved forward, renewals are landing, and if the key has gone
		// anyway that is a real failure rather than a stalled machine.
		held := 3 * time.Second
		start := time.Now()
		time.Sleep(held)
		slept := time.Since(start)

		// The sleep used to be measured and the test skipped itself when the
		// machine had overrun it by more than the lease. Inside a bubble the
		// clock is fake and advances only when every goroutine is blocked, so
		// it cannot overrun and there is nothing left to excuse.
		if slept != held {
			t.Fatalf("a %v sleep took %v of fake time", held, slept)
		}
		if _, err := services.ReadStale(ctx, "web-1"); err != nil {
			t.Fatalf("the registration went while the keep-alive loop was running: %v", err)
		}
		renewed, renewErr := c.Lease(ctx, lease)
		if renewErr != nil {
			t.Fatalf("Lease after renewals: %v", renewErr)
		}
		if !renewed.ExpiresAt.After(info.ExpiresAt) {
			t.Errorf("the lease deadline did not move: %v then %v", info.ExpiresAt, renewed.ExpiresAt)
		}

		stopLoop()
		select {
		case loopErr := <-done:
			if !errors.Is(loopErr, context.Canceled) {
				t.Errorf("KeepAliveLoop returned %v, want context.Canceled", loopErr)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("KeepAliveLoop did not return")
		}

		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := services.ReadStale(ctx, "web-1"); errors.Is(err, easyraft.ErrKeyNotFound) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if _, err := services.ReadStale(ctx, "web-1"); !errors.Is(err, easyraft.ErrKeyNotFound) {
			t.Errorf("the registration outlived its lease: %v", err)
		}
		if _, err := c.Lease(ctx, lease); !errors.Is(err, easyraft.ErrLeaseNotFound) {
			t.Errorf("Lease after expiry: %v, want ErrLeaseNotFound", err)
		}
		// Revoking something already gone is the outcome asked for.
		if err := c.RevokeLease(ctx, lease); err != nil {
			t.Errorf("RevokeLease on an expired lease: %v", err)
		}
	})
}

// TestClient_BatchIsAtomic covers the multi-key path and its guard.
func TestClient_BatchIsAtomic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startServing(t, 3)
		s.cluster.Start()
		waitAdvertised(t, s)

		c := s.client(t)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		create, buildErr := client.Create("leases", "owner", "node-a")
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		if _, err := c.Batch(ctx, create); err != nil {
			t.Fatalf("Batch: %v", err)
		}

		leases := client.Collection[string](c, "leases")
		_, rev, revErr := leases.ReadRev(ctx, "owner")
		if revErr != nil {
			t.Fatal(revErr)
		}

		work, workErr := client.Upsert("work", "item", "done-by-a")
		if workErr != nil {
			t.Fatal(workErr)
		}
		if _, err := c.Batch(ctx, client.Check("leases", "owner", rev), work); err != nil {
			t.Fatalf("guarded batch: %v", err)
		}

		// The guard fails once the key moves, and nothing in the batch is written.
		if err := leases.Update(ctx, "owner", "node-b"); err != nil {
			t.Fatal(err)
		}
		other, err := client.Upsert("work", "other", "written")
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Batch(ctx, client.Check("leases", "owner", rev), other)
		if !errors.Is(err, easyraft.ErrRevisionMismatch) {
			t.Fatalf("a batch under a stale guard gave %v, want ErrRevisionMismatch", err)
		}
		if _, err := client.Collection[string](c, "work").Read(ctx, "other"); !errors.Is(err, easyraft.ErrKeyNotFound) {
			t.Errorf("the refused batch wrote its other operation: %v", err)
		}

		// An empty batch is nothing to do, not an error.
		if results, err := c.Batch(ctx); err != nil || results != nil {
			t.Errorf("empty Batch returned %v, %v", results, err)
		}
	})
}

// TestClient_ClusterInformation covers the read-only endpoints an operator
// tool needs.
func TestClient_ClusterInformation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startServing(t, 3)
		s.cluster.Start()
		waitAdvertised(t, s)

		c := s.client(t)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		if err := c.Health(ctx); err != nil {
			t.Errorf("Health: %v", err)
		}
		members, err := c.Members(ctx)
		if err != nil {
			t.Fatalf("Members: %v", err)
		}
		if len(members) != 3 {
			t.Errorf("Members returned %d nodes", len(members))
		}
		leaders := 0
		for _, m := range members {
			if m.Leader {
				leaders++
			}
		}
		if leaders != 1 {
			t.Errorf("Members reported %d leaders", leaders)
		}

		status, err := c.Status(ctx)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if status.Term == 0 {
			t.Error("Status reported term 0")
		}
	})
}

// TestClient_ReservedCollectionsAreRefused checks that the server's own
// namespace stays out of reach and reports itself as such.
func TestClient_ReservedCollectionsAreRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startServing(t, 1)
		s.cluster.Start()
		waitAdvertised(t, s)

		c := s.client(t)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		reserved := client.Collection[User](c, "__easyraft_metadata__")
		if err := reserved.Create(ctx, "k", User{}); !errors.Is(err, easyraft.ErrReservedCollection) {
			t.Errorf("writing a reserved collection gave %v, want ErrReservedCollection", err)
		}
		if _, err := reserved.Read(ctx, "k"); !errors.Is(err, easyraft.ErrReservedCollection) {
			t.Errorf("reading a reserved collection gave %v, want ErrReservedCollection", err)
		}
	})
}

// TestNew_RefusesAnUnusableEndpoint keeps a misconfiguration from becoming a
// request that silently goes somewhere else.
func TestNew_RefusesAnUnusableEndpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		if _, err := client.New(); err == nil {
			t.Error("New with no endpoints succeeded")
		}
		for _, bad := range []string{
			"",
			"   ",
			"ftp://host:1234",
			"http://host:8001/prefix",
			"http://host:8001?a=b",
			"http://user:pass@host:8001",
			"http://",
		} {
			if _, err := client.New(client.WithEndpoints(bad)); err == nil {
				t.Errorf("New(%q) succeeded", bad)
			}
		}
		for _, good := range []string{
			"host:8001",
			"http://host:8001",
			"https://host:8001",
			"http://host:8001/",
			"127.0.0.1:8001",
		} {
			c, err := client.New(client.WithEndpoints(good))
			if err != nil {
				t.Errorf("New(%q): %v", good, err)
				continue
			}
			if got := c.Endpoints(); len(got) != 1 || got[0] == "" {
				t.Errorf("New(%q) normalized to %v", good, got)
			}
		}
	})
}

// TestClient_AuthorizationIsSent checks the credential reaches the server,
// and that a rejected one reports itself as such rather than as a failure to
// reach anything.
func TestClient_AuthorizationIsSent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const token = "s3cret"
		nw := memnet.NewNetwork()
		const addr = "n1:8080"
		ln := nw.Listen(addr)
		c := easyrafttest.New(t, 1, easyrafttest.Options{
			PerNode: func(int) []easyraft.Option {
				return []easyraft.Option{
					easyraft.WithHTTPAddr(addr),
					easyraft.WithHTTPListener(ln),
					easyraft.WithBearerTokenAuth(token),
				}
			},
		})
		c.Start()

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		authorized, err := client.New(client.WithEndpoints(addr),
			client.WithBearerToken(token), client.WithHTTPClient(nw.HTTPClient()))
		if err != nil {
			t.Fatal(err)
		}
		users := client.Collection[User](authorized, "users")
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if createErr := users.Create(ctx, "alice", User{Name: "Alice"}); createErr == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if _, readErr := users.Read(ctx, "alice"); readErr != nil {
			t.Fatalf("an authorized read failed: %v", readErr)
		}

		anonymous, err := client.New(
			client.WithEndpoints(addr),
			client.WithRetry(1, time.Millisecond),
			client.WithHTTPClient(nw.HTTPClient()),
		)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Collection[User](anonymous, "users").Read(ctx, "alice")
		if !errors.Is(err, easyraft.ErrUnauthorized) && !errors.Is(err, easyraft.ErrForbidden) {
			t.Errorf("an unauthenticated read gave %v", err)
		}
	})
}

// TestClient_MutationResultsComeBack checks the one operation whose answer is
// not just a status code.
func TestClient_MutationResultsComeBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startServing(t, 1)
		counters := easyrafttest.AddCollection[Counter](s.cluster, "counters")
		counters.RegisterMutation("add", func(cur *Counter, args []byte) (*Counter, []byte, error) {
			var delta uint64
			if len(args) > 0 {
				if err := json.Unmarshal(args, &delta); err != nil {
					return nil, nil, err
				}
			}
			cur.Value += delta
			result, err := json.Marshal(cur.Value)
			return cur, result, err
		})
		s.cluster.Start()
		waitAdvertised(t, s)

		c := s.client(t)
		remote := client.Collection[Counter](c, "counters")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		if err := remote.Create(ctx, "hits", Counter{}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		raw, mutateErr := remote.Mutate(ctx, "hits", "add", 5)
		if mutateErr != nil {
			t.Fatalf("Mutate: %v", mutateErr)
		}
		var total uint64
		if decodeErr := json.Unmarshal(raw, &total); decodeErr != nil {
			t.Fatalf("decode mutation result %q: %v", raw, decodeErr)
		}
		if total != 5 {
			t.Errorf("the mutation returned %d, want 5", total)
		}

		// A conditional mutation, refused and then accepted.
		_, rev, readRevErr := remote.ReadRev(ctx, "hits")
		if readRevErr != nil {
			t.Fatal(readRevErr)
		}
		if _, err := remote.MutateIf(ctx, "hits", "add", 1, rev-1); !errors.Is(err, easyraft.ErrRevisionMismatch) {
			t.Errorf("a stale MutateIf gave %v", err)
		}
		if _, err := remote.MutateIf(ctx, "hits", "add", 1, rev); err != nil {
			t.Errorf("MutateIf on the current revision: %v", err)
		}
	})
}

// TestClient_BackupAndRestore covers moving a cluster's state over HTTP,
// which is how an operator takes a backup without a Go program embedded in
// the cluster.
func TestClient_BackupAndRestore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := startServing(t, 3)
		source.cluster.Start()
		waitAdvertised(t, source)

		sc := source.client(t)
		users := client.Collection[User](sc, "users")
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		for i := range 30 {
			if err := users.Create(ctx, fmt.Sprintf("user-%02d", i), User{Name: "u", Age: i}); err != nil {
				t.Fatalf("Create %d: %v", i, err)
			}
		}

		var backup bytes.Buffer
		revision, err := sc.Backup(ctx, &backup)
		if err != nil {
			t.Fatalf("Backup: %v", err)
		}
		if revision == 0 {
			t.Error("Backup reported revision 0")
		}
		if backup.Len() == 0 {
			t.Fatal("Backup wrote nothing")
		}

		// A stale backup from whichever node answers is also usable.
		var stale bytes.Buffer
		if _, staleErr := sc.BackupStale(ctx, &stale); staleErr != nil {
			t.Fatalf("BackupStale: %v", staleErr)
		}
		if stale.Len() == 0 {
			t.Error("BackupStale wrote nothing")
		}

		// Into a different cluster, through a client pointed at a follower, so
		// the restore has to be redirected before its body is sent.
		target := startServing(t, 3)
		target.cluster.Start()
		waitAdvertised(t, target)
		follower := (target.cluster.LeaderIndex() + 1) % 3
		tc := target.client(t, follower)
		targetUsers := client.Collection[User](tc, "users")

		if createErr := targetUsers.Create(ctx, "stranger", User{Name: "gone soon"}); createErr != nil {
			t.Fatal(createErr)
		}
		after, restoreErr := tc.Restore(ctx, bytes.NewReader(backup.Bytes()))
		if restoreErr != nil {
			t.Fatalf("Restore: %v", restoreErr)
		}
		if after == 0 {
			t.Error("Restore reported revision 0")
		}

		if _, strangerErr := targetUsers.ReadStale(ctx, "stranger"); !errors.Is(strangerErr, easyraft.ErrKeyNotFound) {
			t.Errorf("a key the backup did not contain survived the restore: %v", strangerErr)
		}
		for _, i := range []int{0, 15, 29} {
			got, readErr := targetUsers.Read(ctx, fmt.Sprintf("user-%02d", i))
			if readErr != nil {
				t.Fatalf("user-%02d is missing after the restore: %v", i, readErr)
			}
			if got.Age != i {
				t.Errorf("user-%02d came back as %+v", i, got)
			}
		}

		// A backup that is not a backup is refused and changes nothing.
		if _, badErr := tc.Restore(ctx, strings.NewReader("not a backup")); badErr == nil {
			t.Error("a body that is not a backup was accepted")
		}
		if _, stillThereErr := targetUsers.Read(ctx, "user-00"); stillThereErr != nil {
			t.Errorf("the refused restore removed the imported state: %v", stillThereErr)
		}
	})
}

package easyraft_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/easyraft"
)

// startRevisionNode brings up a one-node cluster with HTTP, ready to take
// writes. One node is the right size here: what is under test is the
// conditional-write contract as a caller sees it, which is decided in the
// state machine and is the same on one replica as on five.
func startRevisionNode(t *testing.T) (er *easyraft.EasyRaft[Counter], httpAddr string) {
	t.Helper()
	raftAddr, httpAddr := freePort(t), freePort(t)

	er, err := easyraft.New[Counter](
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(raftAddr),
		easyraft.WithHTTPAddr(httpAddr),
		easyraft.WithDataDir(filepath.Join(t.TempDir(), "n1")),
		easyraft.WithPeers(map[raft.NodeID]string{"n1": raftAddr}),
		easyraft.WithInsecureTransportAcknowledged(),
		easyraft.WithInsecureHTTPAcknowledged(),
	)
	if err != nil {
		t.Fatal(err)
	}
	er.RegisterMutation("add", func(c *Counter, args []byte) (*Counter, []byte, error) {
		var delta uint64
		if len(args) > 0 {
			if jsonErr := json.Unmarshal(args, &delta); jsonErr != nil {
				return nil, nil, jsonErr
			}
		}
		c.Value += delta
		return c, nil, nil
	})
	if startErr := er.Start(); startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}
	t.Cleanup(func() { _ = er.Stop() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if readyErr := er.Ready(ctx); readyErr != nil {
		t.Fatalf("Ready: %v", readyErr)
	}
	return er, httpAddr
}

// TestRevisions_CompareAndSwapThroughTheAPI walks the loop a caller actually
// writes: read a value with its revision, compute the next one, and write it
// back only if nothing moved in between.
func TestRevisions_CompareAndSwapThroughTheAPI(t *testing.T) {
	er, _ := startRevisionNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := er.Create(ctx, "c1", Counter{Value: 1}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A closure rather than a variable holding the error, so that every
	// assertion below reads as one statement and no err outlives the call
	// that produced it.
	readRev := func(key string) (Counter, uint64) {
		t.Helper()
		v, r, readErr := er.ReadRev(ctx, key)
		if readErr != nil {
			t.Fatalf("ReadRev %s: %v", key, readErr)
		}
		return v, r
	}

	value, rev := readRev("c1")
	if value.Value != 1 {
		t.Fatalf("ReadRev returned %d, want 1", value.Value)
	}
	if rev == 0 {
		t.Fatal("ReadRev returned revision 0 for a key that was just written")
	}

	if err := er.UpdateIf(ctx, "c1", Counter{Value: 2}, rev); err != nil {
		t.Fatalf("UpdateIf on the revision just read: %v", err)
	}

	// The same revision is now stale, and a second write on it is refused.
	if err := er.UpdateIf(ctx, "c1", Counter{Value: 99}, rev); !errors.Is(err, easyraft.ErrRevisionMismatch) {
		t.Fatalf("UpdateIf on a stale revision: %v, want ErrRevisionMismatch", err)
	}
	after, newRev := readRev("c1")
	if after.Value != 2 {
		t.Errorf("the refused write left %d behind, want 2", after.Value)
	}
	if newRev == rev {
		t.Error("the revision did not move after a successful conditional update")
	}

	// UpsertIf with zero is create-if-absent, and refuses an existing key.
	if err := er.UpsertIf(ctx, "c1", Counter{Value: 3}, 0); !errors.Is(err, easyraft.ErrRevisionMismatch) {
		t.Fatalf("UpsertIf(rev 0) on an existing key: %v, want ErrRevisionMismatch", err)
	}
	if err := er.UpsertIf(ctx, "brand-new", Counter{Value: 7}, 0); err != nil {
		t.Fatalf("UpsertIf(rev 0) on an absent key: %v", err)
	}

	// MutateIf runs the mutation only on a matching revision.
	delta, _ := json.Marshal(uint64(10))
	if _, err := er.MutateIf(ctx, "c1", "add", delta, rev); !errors.Is(err, easyraft.ErrRevisionMismatch) {
		t.Fatalf("MutateIf on a stale revision: %v, want ErrRevisionMismatch", err)
	}
	if _, err := er.MutateIf(ctx, "c1", "add", delta, newRev); err != nil {
		t.Fatalf("MutateIf on the current revision: %v", err)
	}
	mutated, mutatedRev := readRev("c1")
	if mutated.Value != 12 {
		t.Errorf("value after a conditional mutation is %d, want 12", mutated.Value)
	}

	// DeleteIf, refused and then accepted.
	if err := er.DeleteIf(ctx, "c1", newRev); !errors.Is(err, easyraft.ErrRevisionMismatch) {
		t.Fatalf("DeleteIf on a stale revision: %v, want ErrRevisionMismatch", err)
	}
	if err := er.DeleteIf(ctx, "c1", mutatedRev); err != nil {
		t.Fatalf("DeleteIf on the current revision: %v", err)
	}
	if _, err := er.Read(ctx, "c1"); !errors.Is(err, easyraft.ErrKeyNotFound) {
		t.Errorf("the key survived a matching DeleteIf: %v", err)
	}

	if er.Revision() == 0 {
		t.Error("Revision is 0 after a series of writes")
	}
}

// TestRevisions_ConcurrentIncrementsLoseNothing is the property the whole
// feature is for. Many writers run read-modify-write on one key with no
// mutation and no lock; every one of them either lands or is told to retry, so
// the total is exactly the number of increments.
func TestRevisions_ConcurrentIncrementsLoseNothing(t *testing.T) {
	er, _ := startRevisionNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := er.Create(ctx, "shared", Counter{Value: 0}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const writers, each = 4, 15
	errs := make(chan error, writers)
	for range writers {
		go func() {
			for range each {
				for {
					current, rev, err := er.ReadRev(ctx, "shared")
					if err != nil {
						errs <- err
						return
					}
					err = er.UpdateIf(ctx, "shared", Counter{Value: current.Value + 1}, rev)
					if err == nil {
						break
					}
					if !errors.Is(err, easyraft.ErrRevisionMismatch) {
						errs <- err
						return
					}
				}
			}
			errs <- nil
		}()
	}
	for range writers {
		if err := <-errs; err != nil {
			t.Fatalf("writer: %v", err)
		}
	}

	final, err := er.Read(ctx, "shared")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if final.Value != writers*each {
		t.Errorf("counter is %d after %d increments; a conditional write overwrote another",
			final.Value, writers*each)
	}
}

// TestRevisions_TxnCheckGuardsTheBatch covers the transaction guard: a batch
// conditional on a key it does not write.
func TestRevisions_TxnCheckGuardsTheBatch(t *testing.T) {
	er, _ := startRevisionNode(t)
	store := er.Store()
	leases := easyraft.AddCollection[string](store, "leases")
	work := easyraft.AddCollection[string](store, "work")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if createErr := leases.Create(ctx, "owner", "node-a"); createErr != nil {
		t.Fatalf("Create lease: %v", createErr)
	}
	_, leaseRev, readErr := leases.ReadRev(ctx, "owner")
	if readErr != nil {
		t.Fatalf("ReadRev lease: %v", readErr)
	}

	// guarded runs one transaction that writes the work item only while the
	// lease is still at the revision given.
	guarded := func(guard uint64, value string) error {
		_, txnErr := store.Txn(ctx, func(tx *easyraft.Txn) error {
			if checkErr := tx.CheckRev("leases", "owner", guard); checkErr != nil {
				return checkErr
			}
			return tx.Upsert("work", "item", value)
		})
		return txnErr
	}

	if txnErr := guarded(leaseRev, "done-by-a"); txnErr != nil {
		t.Fatalf("guarded transaction under a held lease: %v", txnErr)
	}

	// The lease moves to another owner, so the guard no longer holds.
	if updateErr := leases.Update(ctx, "owner", "node-b"); updateErr != nil {
		t.Fatalf("lease handover: %v", updateErr)
	}
	if txnErr := guarded(leaseRev, "done-by-a-again"); !errors.Is(txnErr, easyraft.ErrRevisionMismatch) {
		t.Fatalf("guarded transaction under a lost lease: %v, want ErrRevisionMismatch", txnErr)
	}
	got, itemErr := work.Read(ctx, "item")
	if itemErr != nil {
		t.Fatalf("Read work item: %v", itemErr)
	}
	if got != "done-by-a" {
		t.Errorf("the refused transaction wrote %q", got)
	}

	// A conditional operation inside a transaction fails the whole batch too.
	_, txnErr := store.Txn(ctx, func(tx *easyraft.Txn) error {
		if upsertErr := tx.Upsert("work", "other", "written"); upsertErr != nil {
			return upsertErr
		}
		return tx.UpdateIf("leases", "owner", "node-c", leaseRev)
	})
	if !errors.Is(txnErr, easyraft.ErrRevisionMismatch) {
		t.Fatalf("transaction with a stale conditional update: %v, want ErrRevisionMismatch", txnErr)
	}
	if _, err := work.Read(ctx, "other"); !errors.Is(err, easyraft.ErrKeyNotFound) {
		t.Errorf("a rolled-back transaction left its first write behind: %v", err)
	}
}

// doRequest runs one request and returns its status, body and ETag.
func doRequest(t *testing.T, method, url, body string, header map[string]string) (status int, respBody, etag string) {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw), resp.Header.Get("ETag")
}

// TestRevisions_HTTPConditionalRequests checks that the same contract is
// reachable over HTTP through the headers that already mean it, so a client in
// any language gets compare-and-swap without a bespoke protocol.
func TestRevisions_HTTPConditionalRequests(t *testing.T) {
	_, httpAddr := startRevisionNode(t)
	base := "http://" + httpAddr + "/default/k"

	if status, body, _ := doRequest(t, http.MethodPost, base, `{"value":1}`, nil); status != http.StatusCreated {
		t.Fatalf("POST: %d %s", status, body)
	}

	status, _, etag := doRequest(t, http.MethodGet, base, "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET: %d", status)
	}
	if etag == "" {
		t.Fatal("GET returned no ETag")
	}
	if _, err := strconv.ParseUint(etag, 10, 64); err != nil {
		t.Fatalf("ETag %q is not a revision: %v", etag, err)
	}

	// The ETag handed back as If-Match applies.
	if status, body, _ := doRequest(t, http.MethodPut, base, `{"value":2}`,
		map[string]string{"If-Match": etag}); status != http.StatusNoContent {
		t.Fatalf("PUT with a current If-Match: %d %s", status, body)
	}
	// The same ETag is now stale: 412, and nothing is written.
	if status, _, _ := doRequest(t, http.MethodPut, base, `{"value":3}`,
		map[string]string{"If-Match": etag}); status != http.StatusPreconditionFailed {
		t.Fatalf("PUT with a stale If-Match: %d, want 412", status)
	}
	if _, body, _ := doRequest(t, http.MethodGet, base, "", nil); !strings.Contains(body, `"value":2`) {
		t.Errorf("the refused PUT changed the value: %s", body)
	}

	// A quoted ETag is the form an HTTP client is most likely to send back.
	_, _, current := doRequest(t, http.MethodGet, base, "", nil)
	if status, body, _ := doRequest(t, http.MethodPut, base, `{"value":4}`,
		map[string]string{"If-Match": `"` + current + `"`}); status != http.StatusNoContent {
		t.Fatalf("PUT with a quoted If-Match: %d %s", status, body)
	}

	// If-None-Match: * is create-if-absent.
	other := "http://" + httpAddr + "/default/fresh"
	if status, body, _ := doRequest(t, http.MethodPatch, other, `{"value":9}`,
		map[string]string{"If-None-Match": "*"}); status != http.StatusNoContent {
		t.Fatalf("PATCH with If-None-Match on an absent key: %d %s", status, body)
	}
	if status, _, _ := doRequest(t, http.MethodPatch, other, `{"value":10}`,
		map[string]string{"If-None-Match": "*"}); status != http.StatusPreconditionFailed {
		t.Fatalf("PATCH with If-None-Match on an existing key: %d, want 412", status)
	}

	// Anything this store cannot check is refused rather than applied.
	for _, bad := range []map[string]string{
		{"If-Match": "*"},
		{"If-Match": "not-a-number"},
		{"If-None-Match": `"7"`},
		{"If-Match": "1", "If-None-Match": "*"},
	} {
		if status, body, _ := doRequest(t, http.MethodPut, base, `{"value":5}`, bad); status != http.StatusBadRequest {
			t.Errorf("PUT with %v: %d %s, want 400", bad, status, body)
		}
	}

	// A conditional DELETE, refused and then accepted.
	_, _, current = doRequest(t, http.MethodGet, base, "", nil)
	stale := strconv.FormatUint(mustParse(t, current)-1, 10)
	if status, _, _ := doRequest(t, http.MethodDelete, base, "",
		map[string]string{"If-Match": stale}); status != http.StatusPreconditionFailed {
		t.Fatalf("DELETE with a stale If-Match: %d, want 412", status)
	}
	if status, body, _ := doRequest(t, http.MethodDelete, base, "",
		map[string]string{"If-Match": current}); status != http.StatusNoContent {
		t.Fatalf("DELETE with a current If-Match: %d %s", status, body)
	}
}

func mustParse(t *testing.T, s string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("parse revision %q: %v", s, err)
	}
	return v
}

// TestRevisions_HTTPBatchCheck exercises the guard through the batch endpoint,
// which is how a non-Go client gets a guarded transaction.
func TestRevisions_HTTPBatchCheck(t *testing.T) {
	_, httpAddr := startRevisionNode(t)

	if status, body, _ := doRequest(t, http.MethodPost,
		"http://"+httpAddr+"/leases/owner", `"node-a"`, nil); status != http.StatusCreated {
		t.Fatalf("create lease: %d %s", status, body)
	}
	_, _, etag := doRequest(t, http.MethodGet, "http://"+httpAddr+"/leases/owner", "", nil)
	rev := mustParse(t, etag)

	batch := func(guard uint64, value string) (int, string) {
		ops := []map[string]any{
			{"op": "check", "collection": "leases", "key": "owner", "if_rev": guard},
			{"op": "upsert", "collection": "work", "key": "item", "value": value},
		}
		body, err := json.Marshal(ops)
		if err != nil {
			t.Fatalf("encode batch: %v", err)
		}
		status, respBody, _ := doRequest(t, http.MethodPost, "http://"+httpAddr+"/batch", string(body), nil)
		return status, respBody
	}

	if status, body := batch(rev, "done-by-a"); status != http.StatusOK {
		t.Fatalf("guarded batch under a held lease: %d %s", status, body)
	}
	if status, body, _ := doRequest(t, http.MethodPut,
		"http://"+httpAddr+"/leases/owner", `"node-b"`, nil); status != http.StatusNoContent {
		t.Fatalf("lease handover: %d %s", status, body)
	}
	if status, body := batch(rev, "done-by-a-again"); status != http.StatusPreconditionFailed {
		t.Fatalf("guarded batch under a lost lease: %d %s, want 412", status, body)
	}
	if _, body, _ := doRequest(t, http.MethodGet, "http://"+httpAddr+"/work/item", "", nil); strings.TrimSpace(body) != `"done-by-a"` {
		t.Errorf("the refused batch wrote %s", body)
	}
}

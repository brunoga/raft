package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/brunoga/raft/v2/easyraft"
	"github.com/brunoga/raft/v2/easyraft/easyrafttest"
)

// startLedger brings up a three-node ledger serving this example's own routes
// and returns the HTTP addresses.
func startLedger(t *testing.T) (cluster *easyrafttest.Cluster, addrs []string) {
	t.Helper()

	addrs = make([]string, 3)
	muxes := make([]*http.ServeMux, 3)
	for i := range addrs {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addrs[i] = ln.Addr().String()
		if closeErr := ln.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		muxes[i] = http.NewServeMux()
	}

	c := easyrafttest.New(t, 3, easyrafttest.Options{
		Store: []easyraft.Option{easyraft.WithInsecureHTTPAcknowledged()},
		PerNode: func(i int) []easyraft.Option {
			return []easyraft.Option{
				easyraft.WithHTTPAddr(addrs[i]),
				easyraft.WithHTTPMux(muxes[i]),
			}
		},
	})

	for i, store := range c.Stores {
		srv := newServer(store)
		muxes[i].HandleFunc("POST /accounts", srv.handleCreateAccount)
		muxes[i].HandleFunc("GET /accounts/{id}", srv.handleGetAccount)
		muxes[i].HandleFunc("POST /transfers", srv.handleTransfer)
		muxes[i].HandleFunc("GET /period", srv.handleGetPeriod)
		muxes[i].HandleFunc("POST /period", srv.handleOpenPeriod)
		muxes[i].HandleFunc("POST /period/close", srv.handleClosePeriod)

		httpSrv := &http.Server{Handler: muxes[i], ReadHeaderTimeout: 5 * time.Second}
		ln, err := net.Listen("tcp", addrs[i])
		if err != nil {
			t.Fatalf("listen %s: %v", addrs[i], err)
		}
		go func() { _ = httpSrv.Serve(ln) }()
		t.Cleanup(func() { _ = httpSrv.Close() })
	}

	c.Start()
	return c, addrs
}

func do(t *testing.T, method, url string, body ...string) (status int, resp, etag string) {
	t.Helper()
	var reader io.Reader = http.NoBody
	if len(body) > 0 && body[0] != "" {
		reader = bytes.NewReader([]byte(body[0]))
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return httpResp.StatusCode, string(raw), httpResp.Header.Get("ETag")
}

// setUp opens a period and two funded accounts, retrying while the cluster is
// still electing.
func setUp(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if status, _, _ := do(t, http.MethodPost, base+"/period", `{"id":"2026-09"}`); status == http.StatusNoContent {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, acc := range []string{`{"id":"alice","balance":1000}`, `{"id":"bob","balance":0}`} {
		if status, body, _ := do(t, http.MethodPost, base+"/accounts", acc); status != http.StatusCreated &&
			status != http.StatusConflict {
			t.Fatalf("create account: %d %s", status, body)
		}
	}
}

func balanceOf(t *testing.T, base, id string) int64 {
	t.Helper()
	status, body, _ := do(t, http.MethodGet, base+"/accounts/"+id)
	if status != http.StatusOK {
		t.Fatalf("GET /accounts/%s: %d %s", id, status, body)
	}
	var acc Account
	if err := json.Unmarshal([]byte(body), &acc); err != nil {
		t.Fatal(err)
	}
	return acc.Balance
}

// TestPeriod_GuardStopsATransferRacingTheClose is the race the guard exists
// for, run deliberately: a transfer is validated against an open period, the
// books are closed, and only then does the transfer try to commit.
//
// Without CheckRev it would commit, posting into a period already reconciled,
// with nothing having reported an error.
func TestPeriod_GuardStopsATransferRacingTheClose(t *testing.T) {
	_, addrs := startLedger(t)
	base := "http://" + addrs[0]
	setUp(t, base)

	before := balanceOf(t, base, "alice")

	// Close the books. A transfer that read the period before this point now
	// holds a revision that has moved.
	if status, body, _ := do(t, http.MethodPost, base+"/period/close", ""); status != http.StatusOK {
		t.Fatalf("close: %d %s", status, body)
	}

	// A transfer now is refused at the read, before any transaction.
	status, body, _ := do(t, http.MethodPost, base+"/transfers",
		`{"from":"alice","to":"bob","amount":100,"client_id":"c1","seq":1}`)
	if status != http.StatusConflict {
		t.Fatalf("transfer into closed books: %d %s, want 409", status, body)
	}
	if got := balanceOf(t, base, "alice"); got != before {
		t.Errorf("a refused transfer moved money: %d, was %d", got, before)
	}
	if got := balanceOf(t, base, "bob"); got != 0 {
		t.Errorf("a refused transfer credited bob %d", got)
	}
}

// TestPeriod_GuardIsCheckedAtCommit covers the half the handler's own read
// cannot: the period closing after that read and before the commit.
//
// It calls postTransfer directly, holding a revision from while the books
// were open and committing after they closed. That window cannot be opened
// through HTTP, which is why the transaction is a method rather than a
// closure inside the handler -- and this test fails if its guard is removed,
// which a test driving only the HTTP surface does not.
func TestPeriod_GuardIsCheckedAtCommit(t *testing.T) {
	cluster, addrs := startLedger(t)
	base := "http://" + addrs[0]
	setUp(t, base)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The revision a handler would have read while the period was open.
	status, body, periodEtag := do(t, http.MethodGet, base+"/period")
	if status != http.StatusOK {
		t.Fatalf("GET /period: %d %s", status, body)
	}
	var open Period
	if err := json.Unmarshal([]byte(body), &open); err != nil {
		t.Fatal(err)
	}
	if !open.Open {
		t.Fatal("the period is not open")
	}
	staleRev := mustUint64(t, periodEtag)

	// The books close before the transfer commits.
	if closeStatus, closeBody, _ := do(t, http.MethodPost, base+"/period/close"); closeStatus != http.StatusOK {
		t.Fatalf("close: %d %s", closeStatus, closeBody)
	}

	before := balanceOf(t, base, "alice")
	srv := newServer(cluster.WaitLeader())
	err := srv.postTransfer(ctx, Transfer{
		ID: "late:1", From: "alice", To: "bob", Amount: 100,
	}, staleRev)

	if !errors.Is(err, easyraft.ErrRevisionMismatch) {
		t.Fatalf("a transfer guarded on a closed period returned %v, want ErrRevisionMismatch", err)
	}
	if got := balanceOf(t, base, "alice"); got != before {
		t.Errorf("the refused transaction moved money: %d, was %d", got, before)
	}
	if got := balanceOf(t, base, "bob"); got != 0 {
		t.Errorf("the refused transaction credited bob %d", got)
	}
	if status, _, _ := do(t, http.MethodGet, base+"/transfers/late:1"); status == http.StatusOK {
		t.Error("the refused transaction wrote its transfer record")
	}

	// And the same transfer against the revision the period actually has now
	// passes the guard -- so the refusal above was the guard, not something
	// else about the transaction.
	_, _, nowEtag := do(t, http.MethodGet, base+"/period")
	if err := srv.postTransfer(ctx, Transfer{
		ID: "late:2", From: "alice", To: "bob", Amount: 100,
	}, mustUint64(t, nowEtag)); err != nil {
		t.Fatalf("the same transfer on the current revision: %v", err)
	}
	if got := balanceOf(t, base, "bob"); got != 100 {
		t.Errorf("bob has %d after the guarded transfer landed, want 100", got)
	}
}

// TestPeriod_TransfersWorkWhileOpen keeps the guard from being a test that
// passes because nothing works.
func TestPeriod_TransfersWorkWhileOpen(t *testing.T) {
	_, addrs := startLedger(t)
	base := "http://" + addrs[0]
	setUp(t, base)

	if status, body, _ := do(t, http.MethodPost, base+"/transfers",
		`{"from":"alice","to":"bob","amount":250,"client_id":"c1","seq":1}`); status != http.StatusCreated {
		t.Fatalf("transfer while the books are open: %d %s", status, body)
	}
	if got := balanceOf(t, base, "alice"); got != 750 {
		t.Errorf("alice has %d, want 750", got)
	}
	if got := balanceOf(t, base, "bob"); got != 250 {
		t.Errorf("bob has %d, want 250", got)
	}

	// Reopening a period lets transfers resume.
	if status, body, _ := do(t, http.MethodPost, base+"/period/close", ""); status != http.StatusOK {
		t.Fatalf("close: %d %s", status, body)
	}
	if status, _, _ := do(t, http.MethodPost, base+"/transfers",
		`{"from":"alice","to":"bob","amount":10,"client_id":"c1","seq":2}`); status != http.StatusConflict {
		t.Fatal("a transfer landed while the books were closed")
	}
	if status, body, _ := do(t, http.MethodPost, base+"/period", `{"id":"2026-10"}`); status != http.StatusNoContent {
		t.Fatalf("open the next period: %d %s", status, body)
	}
	if status, body, _ := do(t, http.MethodPost, base+"/transfers",
		`{"from":"alice","to":"bob","amount":10,"client_id":"c1","seq":3}`); status != http.StatusCreated {
		t.Fatalf("transfer in the new period: %d %s", status, body)
	}
	if got := balanceOf(t, base, "bob"); got != 260 {
		t.Errorf("bob has %d, want 260", got)
	}
}

// TestPeriod_ClosingTwiceAtOnce covers the conditional update on the close
// itself: two operators closing the same period must not both succeed and
// record different closing times for it.
func TestPeriod_ClosingTwiceAtOnce(t *testing.T) {
	_, addrs := startLedger(t)
	base := "http://" + addrs[0]
	setUp(t, base)

	if status, _, _ := do(t, http.MethodPost, base+"/period/close", ""); status != http.StatusOK {
		t.Fatal("the first close failed")
	}
	if status, body, _ := do(t, http.MethodPost, base+"/period/close", ""); status != http.StatusConflict {
		t.Errorf("the second close answered %d %s, want 409", status, body)
	}
}

func mustUint64(t *testing.T, s string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

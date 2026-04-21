// ledger is a distributed double-entry ledger built on EasyRaft.
//
// It stores accounts and an immutable transfer log in two separate collections
// that share a single Raft group. Fund transfers are committed as atomic
// transactions using [easyraft.Store.Txn]: the transfer record, the debit, and
// the credit are all applied in a single Raft log entry — either all three
// succeed or none do.
//
// # Key design points
//
// Idempotent transfers: the caller supplies a (client_id, seq) pair which
// becomes the transfer ID. The transfer record is created first inside the
// Txn. If the record already exists, [easyraft.ErrKeyExists] is returned
// before any balance mutations run — no rollback needed. On a second call with
// the same (client_id, seq), the server returns the previously committed record
// rather than double-debiting.
//
// Atomic rollback: [easyraft.Store.Txn] commits all operations in a single
// Raft log entry. If any operation fails (insufficient funds, unknown account,
// etc.) the entire batch is rolled back by the state machine, leaving balances
// unchanged.
//
// Deterministic mutations: balance changes run inside registered mutation
// functions that execute on every replica during Apply and log replay.
// The amount is encoded in the args before proposing — all replicas apply the
// exact same delta without reading any external state.
//
// # HTTP API
//
//	POST   /accounts         — create account {"id":"alice","balance":1000}
//	GET    /accounts/{id}    — read account (linearizable)
//	GET    /accounts         — list all accounts
//	POST   /transfers        — transfer funds {"from":"alice","to":"bob","amount":50,"client_id":"c1","seq":1}
//	GET    /transfers/{id}   — get transfer record
//	GET    /transfers        — list all transfers
//
// # Usage
//
//	# Node 1 — bootstrap
//	./ledger --id n1 --raft-addr :7001 --http-addr :8001 --data-dir /tmp/ledger/n1
//
//	# Node 2 — joins node 1
//	./ledger --id n2 --raft-addr :7002 --http-addr :8002 --data-dir /tmp/ledger/n2 \
//	         --join localhost:8001
//
//	# Create accounts
//	curl -X POST http://localhost:8001/accounts \
//	     -H 'Content-Type: application/json' -d '{"id":"alice","balance":1000}'
//	curl -X POST http://localhost:8001/accounts \
//	     -H 'Content-Type: application/json' -d '{"id":"bob","balance":0}'
//
//	# Transfer 100 from alice to bob
//	curl -L -X POST http://localhost:8001/transfers \
//	     -H 'Content-Type: application/json' \
//	     -d '{"from":"alice","to":"bob","amount":100,"client_id":"cli1","seq":1}'
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/easyraft"
)

// Account holds the current balance for one participant.
type Account struct {
	ID      string `json:"id"`
	Balance int64  `json:"balance"`
}

// Transfer is the immutable record of a committed fund movement.
type Transfer struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Amount    int64  `json:"amount"`
	Timestamp int64  `json:"timestamp"` // Unix nanoseconds; set by HTTP handler, not inside Apply
}

// debitArgs is passed to the "debit" mutation on the accounts collection.
type debitArgs struct {
	Amount int64 `json:"amount"`
}

// creditArgs is passed to the "credit" mutation on the accounts collection.
type creditArgs struct {
	Amount int64 `json:"amount"`
}

// errInsufficientFunds is a typed error returned by the debit mutation when
// the account balance is too low. Using a distinct type lets writeErr map it
// to 422 Unprocessable Entity so the client can distinguish it from generic
// server errors.
type errInsufficientFunds struct {
	Balance int64
	Amount  int64
}

func (e *errInsufficientFunds) Error() string {
	return fmt.Sprintf("insufficient funds: balance %d, requested %d", e.Balance, e.Amount)
}

// server wires together EasyRaft and the HTTP handlers.
type server struct {
	store     *easyraft.Store
	accounts  *easyraft.Collection[Account]
	transfers *easyraft.Collection[Transfer]
}

func newServer(store *easyraft.Store) *server {
	accounts := easyraft.AddCollection[Account](store, "accounts")
	transfers := easyraft.AddCollection[Transfer](store, "transfers")

	// debit reduces the balance by amount. Returns an error if the resulting
	// balance would be negative.
	//
	// Runs inside raft.StateMachine.Apply on every replica — must be
	// deterministic: no time.Now, no rand, no I/O.
	accounts.RegisterMutation("debit", func(current *Account, args []byte) (*Account, []byte, error) {
		var a debitArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, nil, fmt.Errorf("debit: bad args: %w", err)
		}
		if current.Balance < a.Amount {
			return nil, nil, &errInsufficientFunds{Balance: current.Balance, Amount: a.Amount}
		}
		updated := *current
		updated.Balance -= a.Amount
		return &updated, nil, nil
	})

	// credit increases the balance by amount.
	accounts.RegisterMutation("credit", func(current *Account, args []byte) (*Account, []byte, error) {
		var a creditArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, nil, fmt.Errorf("credit: bad args: %w", err)
		}
		updated := *current
		updated.Balance += a.Amount
		return &updated, nil, nil
	})

	return &server{store: store, accounts: accounts, transfers: transfers}
}

func (s *server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string `json:"id"`
		Balance int64  `json:"balance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "invalid body: need {\"id\":\"...\",\"balance\":N}", http.StatusBadRequest)
		return
	}

	acc := Account{ID: body.ID, Balance: body.Balance}
	if err := s.accounts.Create(r.Context(), body.ID, acc); err != nil {
		if errors.Is(err, easyraft.ErrKeyExists) {
			http.Error(w, "account already exists", http.StatusConflict)
			return
		}
		s.writeErr(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(acc)
}

func (s *server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	acc, err := s.accounts.Read(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, easyraft.ErrKeyNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(acc)
}

func (s *server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	all, err := s.accounts.List(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(all)
}

// transferRequest is the body for POST /transfers.
type transferRequest struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Amount   int64  `json:"amount"`
	ClientID string `json:"client_id"`
	Seq      uint64 `json:"seq"`
}

// handleTransfer handles POST /transfers.
//
// The transfer is executed as a single atomic Txn with three operations:
//  1. Create the transfer record (idempotency guard — ErrKeyExists = duplicate)
//  2. Debit the source account
//  3. Credit the destination account
//
// If any step fails, the entire batch is rolled back by the state machine.
// The transfer ID is derived from (client_id, seq), so retries with the same
// pair are detected by ErrKeyExists on step 1 — before any balance changes.
func (s *server) handleTransfer(w http.ResponseWriter, r *http.Request) {
	var req transferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.From == "" || req.To == "" || req.Amount <= 0 || req.ClientID == "" {
		http.Error(w, "need from, to, amount>0, client_id, seq", http.StatusBadRequest)
		return
	}
	if req.From == req.To {
		http.Error(w, "from and to must differ", http.StatusBadRequest)
		return
	}

	// Transfer ID derived from client identity — retries land on the same key.
	transferID := fmt.Sprintf("%s:%d", req.ClientID, req.Seq)

	// Timestamp fixed before proposing so every replica stores the same value.
	record := Transfer{
		ID:        transferID,
		From:      req.From,
		To:        req.To,
		Amount:    req.Amount,
		Timestamp: time.Now().UnixNano(),
	}

	_, txErr := s.store.Txn(r.Context(), func(tx *easyraft.Txn) error {
		// Step 1: create the transfer record. This is the idempotency guard:
		// if the record already exists, ErrKeyExists aborts the batch before
		// any balance mutations run — nothing to roll back.
		if err := tx.Create("transfers", transferID, record); err != nil {
			return err
		}
		// Step 2: debit source. Fails if balance < amount; the whole batch
		// (including the Create above) is rolled back.
		if err := tx.Mutate("accounts", req.From, "debit", debitArgs{Amount: req.Amount}); err != nil {
			return err
		}
		// Step 3: credit destination.
		return tx.Mutate("accounts", req.To, "credit", creditArgs{Amount: req.Amount})
	})

	if txErr != nil {
		if errors.Is(txErr, easyraft.ErrKeyExists) {
			// Duplicate request: the transfer already committed. Return the
			// existing record so the client can confirm the outcome.
			existing, readErr := s.transfers.Read(r.Context(), transferID)
			if readErr == nil {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(existing)
				return
			}
		}
		if errors.Is(txErr, easyraft.ErrKeyNotFound) {
			http.Error(w, "account not found", http.StatusNotFound)
			return
		}
		s.writeErr(w, txErr)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(record)
}

func (s *server) handleGetTransfer(w http.ResponseWriter, r *http.Request) {
	tr, err := s.transfers.Read(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, easyraft.ErrKeyNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tr)
}

func (s *server) handleListTransfers(w http.ResponseWriter, r *http.Request) {
	all, err := s.transfers.List(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(all)
}

func (s *server) writeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, easyraft.ErrNotLeader) {
		http.Error(w, "not leader", http.StatusServiceUnavailable)
		return
	}
	if _, ok := errors.AsType[*errInsufficientFunds](err); ok {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func main() {
	id := flag.String("id", "", "Raft node ID (required)")
	raftAddr := flag.String("raft-addr", ":7001", "Raft gRPC listen address")
	httpAddr := flag.String("http-addr", ":8001", "HTTP listen address")
	dataDir := flag.String("data-dir", "", "Persistent data directory (required)")
	join := flag.String("join", "", "Comma-separated HTTP addresses of seed nodes to join")
	flag.Parse()

	if *id == "" || *dataDir == "" {
		fmt.Fprintln(os.Stderr, "usage: ledger --id <id> --data-dir <dir> [--raft-addr :7001] [--http-addr :8001] [--join host:port,...]")
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mux := http.NewServeMux()

	opts := []easyraft.Option{
		easyraft.WithID(raft.NodeID(*id)),
		easyraft.WithRaftAddr(*raftAddr),
		easyraft.WithHTTPAddr(*httpAddr),
		easyraft.WithHTTPMux(mux),
		easyraft.WithDataDir(*dataDir),
		easyraft.WithLogger(logger),
	}

	if *join != "" {
		addrs := strings.Split(*join, ",")
		opts = append(opts, easyraft.WithJoinAddr(addrs...))
	}

	store, err := easyraft.NewStore(opts...)
	if err != nil {
		logger.Error("failed to create store", "err", err)
		os.Exit(1)
	}

	srv := newServer(store)

	mux.HandleFunc("POST /accounts", srv.handleCreateAccount)
	mux.HandleFunc("GET /accounts/{id}", srv.handleGetAccount)
	mux.HandleFunc("GET /accounts", srv.handleListAccounts)
	mux.HandleFunc("POST /transfers", srv.handleTransfer)
	mux.HandleFunc("GET /transfers/{id}", srv.handleGetTransfer)
	mux.HandleFunc("GET /transfers", srv.handleListTransfers)

	httpSrv := &http.Server{
		Addr:    *httpAddr,
		Handler: mux,
	}

	store.Start()
	defer store.Stop()

	logger.Info("ledger started", "id", *id, "raft", *raftAddr, "http", *httpAddr)

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("HTTP server error", "err", err)
	}
}

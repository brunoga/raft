// Package client provides a high-level Go client for the ledger service.
//
// The client handles leader discovery and automatic redirection transparently:
// any node address can be used as a seed, and write requests are routed to the
// current leader. The leader address is cached and revalidated on each failure.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/brunoga/raft/examples/internal/exampleutil"
)

// Sentinel errors returned by the client.
var (
	// ErrNotFound is returned when the requested account or transfer does not exist.
	ErrNotFound = errors.New("not found")

	// ErrConflict is returned when creating an account that already exists.
	ErrConflict = errors.New("already exists")

	// ErrInsufficientFunds is returned when a transfer would make the source
	// account balance negative.
	ErrInsufficientFunds = errors.New("insufficient funds")

	// ErrNotLeader is returned when the request reaches a follower and no
	// redirect is available. This is transient; retrying usually succeeds.
	ErrNotLeader = exampleutil.ErrNotLeader
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
	Timestamp int64  `json:"timestamp"` // Unix nanoseconds
}

// Client is a thread-safe, high-level client for the ledger service.
type Client struct {
	inner *exampleutil.Client
}

// New returns a new ledger client. addrs should be the HTTP addresses of one or
// more cluster nodes (e.g. "http://localhost:8001"). At least one reachable
// address is required; the rest are used as fallbacks.
func New(addrs []string) *Client {
	c := exampleutil.NewClient(addrs)
	c.ErrorMapper = func(status int, body string) error {
		switch status {
		case http.StatusNotFound:
			return ErrNotFound
		case http.StatusConflict:
			return ErrConflict
		case http.StatusUnprocessableEntity:
			return fmt.Errorf("%s: %w", body, ErrInsufficientFunds)
		}
		return nil
	}
	return &Client{inner: c}
}

// CreateAccount creates a new account with the given initial balance.
// Returns ErrConflict if an account with that ID already exists.
func (c *Client) CreateAccount(ctx context.Context, id string, balance int64) (Account, error) {
	body, err := json.Marshal(map[string]any{"id": id, "balance": balance})
	if err != nil {
		return Account{}, err
	}
	var acc Account
	if err := c.inner.Do(ctx, 0, http.MethodPost, "/accounts", body, &acc, isTerminal); err != nil {
		return Account{}, err
	}
	return acc, nil
}

// GetAccount returns the current state of an account with linearizable
// consistency. Returns ErrNotFound if no such account exists.
func (c *Client) GetAccount(ctx context.Context, id string) (Account, error) {
	var acc Account
	if err := c.inner.Do(ctx, 0, http.MethodGet, "/accounts/"+id, nil, &acc, isTerminal); err != nil {
		return Account{}, err
	}
	return acc, nil
}

// ListAccounts returns all accounts with linearizable consistency.
func (c *Client) ListAccounts(ctx context.Context) (map[string]Account, error) {
	var all map[string]Account
	if err := c.inner.Do(ctx, 0, http.MethodGet, "/accounts", nil, &all, isTerminal); err != nil {
		return nil, err
	}
	return all, nil
}

// Transfer moves amount from one account to another atomically.
//
// The (clientID, seq) pair is the idempotency key: if the network drops the
// response after the transfer commits, retrying with the same pair returns the
// previously committed Transfer record without re-executing the operation.
//
// Returns ErrNotFound if either account does not exist.
// Returns ErrInsufficientFunds if the source account balance is too low.
// Returns ErrConflict if the (clientID, seq) transfer record already exists
// (which should not happen in well-behaved callers — the client returns the
// existing record instead of this error in the normal duplicate-retry case).
func (c *Client) Transfer(ctx context.Context, from, to string, amount int64, clientID string, seq uint64) (Transfer, error) {
	body, err := json.Marshal(map[string]any{
		"from":      from,
		"to":        to,
		"amount":    amount,
		"client_id": clientID,
		"seq":       seq,
	})
	if err != nil {
		return Transfer{}, err
	}
	var tr Transfer
	if err := c.inner.Do(ctx, 0, http.MethodPost, "/transfers", body, &tr, isTerminal); err != nil {
		return Transfer{}, err
	}
	return tr, nil
}

// GetTransfer returns a transfer record by its ID. IDs have the form
// "clientID:seq". Returns ErrNotFound if no such record exists.
func (c *Client) GetTransfer(ctx context.Context, id string) (Transfer, error) {
	var tr Transfer
	if err := c.inner.Do(ctx, 0, http.MethodGet, "/transfers/"+id, nil, &tr, isTerminal); err != nil {
		return Transfer{}, err
	}
	return tr, nil
}

// ListTransfers returns all transfer records with linearizable consistency.
func (c *Client) ListTransfers(ctx context.Context) (map[string]Transfer, error) {
	var all map[string]Transfer
	if err := c.inner.Do(ctx, 0, http.MethodGet, "/transfers", nil, &all, isTerminal); err != nil {
		return nil, err
	}
	return all, nil
}

// isTerminal reports whether err should stop the retry loop immediately.
// Network errors and ErrNotLeader are retried; domain errors are not.
func isTerminal(err error) bool {
	return errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrConflict) ||
		errors.Is(err, ErrInsufficientFunds)
}

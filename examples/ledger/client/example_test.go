package client_test

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/brunoga/raft/examples/ledger/client"
)

func Example() {
	c := client.New([]string{
		"http://localhost:8001",
		"http://localhost:8002",
		"http://localhost:8003",
	})

	ctx := context.Background()

	// Create two accounts.
	alice, err := c.CreateAccount(ctx, "alice", 1000)
	if err != nil {
		log.Fatalf("create alice: %v", err)
	}
	fmt.Printf("alice balance: %d\n", alice.Balance)

	_, err = c.CreateAccount(ctx, "bob", 0)
	if err != nil {
		log.Fatalf("create bob: %v", err)
	}

	// Transfer 100 from alice to bob.
	// The (clientID, seq) pair makes the operation idempotent: if the
	// response is lost and the caller retries, the same transfer record is
	// returned without re-debiting alice.
	tr, err := c.Transfer(ctx, "alice", "bob", 100, "my-client", 1)
	if err != nil {
		if errors.Is(err, client.ErrInsufficientFunds) {
			log.Fatal("alice doesn't have enough funds")
		}
		log.Fatalf("transfer: %v", err)
	}
	fmt.Printf("transfer %s: %s → %s  amount=%d\n", tr.ID, tr.From, tr.To, tr.Amount)

	// Read balances.
	alice, err = c.GetAccount(ctx, "alice")
	if err != nil {
		log.Fatalf("get alice: %v", err)
	}
	bob, err := c.GetAccount(ctx, "bob")
	if err != nil {
		log.Fatalf("get bob: %v", err)
	}
	fmt.Printf("alice balance: %d\n", alice.Balance)
	fmt.Printf("bob balance:   %d\n", bob.Balance)

	// Idempotent retry: same (clientID, seq) returns the original record.
	tr2, err := c.Transfer(ctx, "alice", "bob", 100, "my-client", 1)
	if err != nil {
		log.Fatalf("retry: %v", err)
	}
	fmt.Printf("retry returned same transfer: %v\n", tr2.ID == tr.ID)
}

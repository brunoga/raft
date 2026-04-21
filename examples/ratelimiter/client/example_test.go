package client_test

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/brunoga/raft/examples/ratelimiter/client"
)

func Example() {
	c := client.New([]string{
		"http://localhost:8001",
		"http://localhost:8002",
		"http://localhost:8003",
	})

	ctx := context.Background()

	// Create (or update) a quota for an API key.
	err := c.SetQuota(ctx, "api-key-xyz", client.Quota{
		MaxTokens:     100,
		CurrentTokens: 100,
		RefillRate:    10, // 10 tokens per second
	})
	if err != nil {
		log.Fatalf("set quota: %v", err)
	}

	// Consume 5 tokens — typical call site for each incoming API request.
	resp, err := c.Take(ctx, "api-key-xyz", 5)
	if err != nil {
		if errors.Is(err, client.ErrNotFound) {
			log.Fatal("quota not configured for this key")
		}
		log.Fatalf("take: %v", err)
	}

	if resp.Allowed {
		fmt.Printf("request allowed, %d tokens remaining\n", resp.Remains)
	} else {
		fmt.Printf("rate limited, %d tokens remaining\n", resp.Remains)
	}

	// Read back the current state (linearizable).
	q, err := c.GetQuota(ctx, "api-key-xyz", false)
	if err != nil {
		log.Fatalf("get quota: %v", err)
	}
	fmt.Printf("quota: max=%d current=%d refill=%d/s\n",
		q.MaxTokens, q.CurrentTokens, q.RefillRate)
}

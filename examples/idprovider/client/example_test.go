package client_test

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/brunoga/raft/examples/idprovider/client"
)

func Example() {
	// clientID must be unique per client process for exactly-once semantics.
	c := client.New([]string{
		"http://localhost:8001",
		"http://localhost:8002",
		"http://localhost:8003",
	}, "worker-1")

	ctx := context.Background()

	// Create a domain for order IDs.
	if err := c.CreateDomain(ctx, "orders"); err != nil {
		log.Fatalf("create domain: %v", err)
	}

	// Allocate a batch of 10 IDs. The (clientID, seq) pair is stable, so if
	// the reply is lost and the caller retries, the same range is returned.
	res, err := c.Next(ctx, "orders", 10)
	if err != nil {
		if errors.Is(err, client.ErrDomainNotFound) {
			log.Fatal("domain does not exist")
		}
		log.Fatalf("next: %v", err)
	}
	fmt.Printf("allocated IDs [%d, %d)\n", res.Start, res.Start+res.Count)

	// Check current high-water mark (stale read is fine for monitoring).
	info, err := c.Current(ctx, "orders", true)
	if err != nil {
		log.Fatalf("current: %v", err)
	}
	fmt.Printf("orders high-water mark: %d\n", info.Current)
}

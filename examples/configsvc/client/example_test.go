package client_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/brunoga/raft/examples/configsvc/client"
)

func Example() {
	// Initialize the client with node addresses.
	c := client.New([]string{
		"http://localhost:8001",
		"http://localhost:8002",
		"http://localhost:8003",
	})

	ctx := context.Background()

	// Set a configuration value.
	err := c.Set(ctx, "db.host", "localhost")
	if err != nil {
		log.Fatalf("failed to set config: %v", err)
	}

	// Get a configuration value (linearizable).
	entry, err := c.Get(ctx, "db.host", false)
	if err != nil {
		log.Fatalf("failed to get config: %v", err)
	}
	fmt.Printf("db.host: %s (version: %d)\n", entry.Value, entry.Version)

	// Watch for changes.
	watchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	events, err := c.Watch(watchCtx, "db.host")
	if err != nil {
		log.Fatalf("failed to watch: %v", err)
	}

	go func() {
		for ev := range events {
			if ev.Deleted {
				fmt.Printf("Event: %s deleted\n", ev.Key)
			} else {
				fmt.Printf("Event: %s = %s\n", ev.Key, ev.Value.Value)
			}
		}
	}()

	// Update the value to see the event.
	time.Sleep(100 * time.Millisecond)
	_ = c.Set(ctx, "db.host", "db.example.com")

	time.Sleep(100 * time.Millisecond)
}

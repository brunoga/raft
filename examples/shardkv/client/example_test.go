package client_test

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/brunoga/raft/examples/shardkv/client"
)

func Example() {
	// numShards must match the --shards flag used when starting the cluster.
	c := client.New([]string{
		"http://localhost:8001",
		"http://localhost:8002",
		"http://localhost:8003",
	}, 4)

	ctx := context.Background()

	// Store a value. The client hashes the key to determine which shard owns
	// it and routes the request to that shard's leader automatically.
	if err := c.Put(ctx, "user:42:name", "Alice"); err != nil {
		log.Fatalf("put: %v", err)
	}

	// Retrieve the value.
	name, err := c.Get(ctx, "user:42:name")
	if err != nil {
		if errors.Is(err, client.ErrKeyNotFound) {
			log.Fatal("key not found")
		}
		log.Fatalf("get: %v", err)
	}
	fmt.Printf("user:42:name = %s\n", name)

	// Inspect shard health on one of the nodes.
	shards, err := c.Shards(ctx, "http://localhost:8001")
	if err != nil {
		log.Fatalf("shards: %v", err)
	}
	for _, s := range shards {
		fmt.Printf("shard %d: %s (term=%d)\n", s.GroupID, s.State, s.Term)
	}
}

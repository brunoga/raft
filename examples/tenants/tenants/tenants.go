// Package tenants holds what the multi-tenant node and its client both have
// to agree on: which Raft group a tenant lives in, and what is stored there.
package tenants

import (
	"fmt"
	"hash/fnv"
)

// CollectionName is the collection each tenant's items live in. Every tenant
// has its own Raft group, so the collection name is the same in all of them
// and the group is what keeps tenants apart.
const CollectionName = "items"

// Item is one stored value.
type Item struct {
	Value string `json:"value"`
}

// GroupFor maps a tenant name to the Raft group that holds it.
//
// Group IDs start at one. A Manager routes every inbound RPC by the group ID
// it carries, and zero is what a single-group node's RPCs carry -- so a group
// numbered zero would never be reachable, and easyraft refuses to create one.
// That is why this is 1 + hash%groups rather than hash%groups.
//
// The mapping is a hash rather than a table because a table has to be kept in
// step across every node and client; a pure function of the name cannot drift.
// The cost is that changing the group count moves tenants, which is a
// migration rather than a config change -- see the README.
func GroupFor(tenant string, groups int) (uint64, error) {
	if groups < 1 {
		return 0, fmt.Errorf("tenants: %d groups is not a cluster", groups)
	}
	if tenant == "" {
		return 0, fmt.Errorf("tenants: a tenant needs a name")
	}
	h := fnv.New64a()
	// Hash.Write never returns an error, which is why its result is dropped
	// here rather than checked and rewrapped into something a caller could
	// never act on.
	_, _ = h.Write([]byte(tenant))
	return 1 + h.Sum64()%uint64(groups), nil
}

// shardkv-repl is an interactive REPL client for the shardkv example.
//
// Usage:
//
//	shardkv-repl [--nodes http://localhost:8001,...] [--shards 4]
//
// Commands:
//
//	put <key> <value>    set a key
//	get <key>            get a key
//	delete <key>         delete a key
//	shards [addr]        show shard status on one node (defaults to first node)
//	stats                show shard status across all nodes
//	help                 show this help
//	exit / quit          exit
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/brunoga/raft/examples/internal/exampleutil"
	"github.com/brunoga/raft/examples/shardkv/client"
)

func main() {

	nodes := flag.String("nodes",
		"http://localhost:8001,http://localhost:8002,http://localhost:8003",
		"comma-separated list of cluster node HTTP addresses")
	numShards := flag.Uint64("shards", 4, "number of shard groups (must match --shards on the server)")
	flag.Parse()

	addrs := strings.Split(*nodes, ",")
	c := client.New(addrs, *numShards)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Printf("shardkv REPL (%d shards) — type 'help' for commands\n", *numShards)

	for {
		fmt.Print("shardkv> ")
		if !scanner.Scan() {
			break
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "put":
			if len(fields) < 3 {
				fmt.Fprintln(os.Stderr, "usage: put <key> <value>")
				continue
			}
			key, value := fields[1], strings.Join(fields[2:], " ")
			if err := c.Put(ctx, key, value); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			fmt.Println("ok")

		case "get":
			if len(fields) != 2 {
				fmt.Fprintln(os.Stderr, "usage: get <key>")
				continue
			}
			val, err := c.Get(ctx, fields[1])
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			fmt.Printf("%s = %q\n", fields[1], val)

		case "delete":
			if len(fields) != 2 {
				fmt.Fprintln(os.Stderr, "usage: delete <key>")
				continue
			}
			if err := c.Delete(ctx, fields[1]); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			fmt.Println("deleted")

		case "shards":
			addr := addrs[0]
			if len(fields) == 2 {
				addr = fields[1]
			} else if len(fields) > 2 {
				fmt.Fprintln(os.Stderr, "usage: shards [addr]")
				continue
			}
			statuses, err := c.Shards(ctx, addr)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			sort.Slice(statuses, func(i, j int) bool {
				return statuses[i].GroupID < statuses[j].GroupID
			})
			for _, s := range statuses {
				fmt.Printf("  shard=%-3d  %-12s  term=%-3d  last_applied=%d\n",
					s.GroupID, s.State, s.Term, s.LastApplied)
			}

		case "stats":
			if len(fields) > 1 {
				fmt.Fprintln(os.Stderr, "usage: stats")
				continue
			}
			showStats(ctx, addrs)

		case "help":
			printHelp()

		case "exit", "quit":
			return

		default:
			fmt.Fprintf(os.Stderr, "unknown command %q — type 'help'\n", fields[0])
		}

		if ctx.Err() != nil {
			return
		}
	}
}

// showStats prints shard leader placement across all physical nodes.
func showStats(ctx context.Context, addrs []string) {
	fmt.Printf("=== Shard Status (all nodes) ===\n\n")

	type shardStatus struct {
		GroupID     uint64 `json:"group_id"`
		State       string `json:"state"`
		Term        uint64 `json:"term"`
		LastApplied uint64 `json:"last_applied"`
	}

	// Collect per-node shard status from /raft/status (Manager.StatusAll).
	type nodeReport struct {
		addr   string
		shards []shardStatus
		nodeID string
		err    error
	}

	reports := make([]nodeReport, len(addrs))
	for i, addr := range addrs {
		addr = strings.TrimSuffix(addr, "/")
		var shards []shardStatus
		err := exampleutil.FetchJSON(ctx, addr+"/raft/status", &shards)

		// Also fetch static config for the node ID.
		nodeID := addr
		var cfg struct {
			ID string `json:"id"`
		}
		if exampleutil.FetchJSON(ctx, addr+"/status", &cfg) == nil && cfg.ID != "" {
			nodeID = cfg.ID
		}

		reports[i] = nodeReport{addr: addr, shards: shards, nodeID: nodeID, err: err}
	}

	sort.Slice(reports, func(i, j int) bool { return reports[i].addr < reports[j].addr })

	// Build a map: shard group ID → leader node ID.
	leaderOf := make(map[uint64]string)
	for _, r := range reports {
		for _, s := range r.shards {
			if s.State == "Leader" {
				leaderOf[s.GroupID] = r.nodeID
			}
		}
	}

	// Print per-node table.
	for _, r := range reports {
		if r.err != nil {
			fmt.Printf("Node %s (%s): unreachable (%v)\n\n", r.nodeID, r.addr, r.err)
			continue
		}
		fmt.Printf("Node %s (%s):\n", r.nodeID, r.addr)
		sort.Slice(r.shards, func(i, j int) bool { return r.shards[i].GroupID < r.shards[j].GroupID })
		for _, s := range r.shards {
			fmt.Printf("  shard=%-3d  %-12s  term=%-3d  last_applied=%d\n",
				s.GroupID, s.State, s.Term, s.LastApplied)
		}
		fmt.Println()
	}

	// Print leader summary.
	if len(leaderOf) > 0 {
		fmt.Println("Leader Placement:")
		gids := make([]uint64, 0, len(leaderOf))
		for gid := range leaderOf {
			gids = append(gids, gid)
		}
		sort.Slice(gids, func(i, j int) bool { return gids[i] < gids[j] })
		for _, gid := range gids {
			fmt.Printf("  shard=%-3d  leader=%s\n", gid, leaderOf[gid])
		}
		fmt.Print("\n")
	}

	// Raft metrics from the first reachable node.
	fmt.Println("Raft Metrics:")
	for _, r := range reports {
		if r.err != nil {
			continue
		}
		if err := exampleutil.FetchRaftMetrics(ctx, r.addr+"/metrics"); err != nil {
			fmt.Fprintln(os.Stderr, "  metrics:", err)
		}
		break
	}
}

func printHelp() {

	fmt.Print(`commands:
  put <key> <value>    set a key
  get <key>            get a key
  delete <key>         delete a key
  shards [addr]        show shard status on one node (defaults to first node)
  stats                show shard status across all nodes with leader placement
  help                 show this help
  exit / quit          exit
`)
}

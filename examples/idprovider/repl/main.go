// idprovider-repl is an interactive REPL client for the idprovider example.
//
// Usage:
//
//	idprovider-repl [--nodes http://localhost:8001,...] [--client-id myrepl]
//
// Commands:
//
//	create <domain>              create an ID domain
//	delete <domain>              delete an ID domain
//	list                         list all domains (linearizable)
//	list-stale                   list all domains (stale)
//	next <domain> [count]        allocate IDs from a domain
//	current <domain>             current high-water mark (linearizable)
//	current-stale <domain>       current high-water mark (stale)
//	stats                        show status of all cluster nodes
//	help                         show this help
//	exit / quit                  exit
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/brunoga/raft/examples/idprovider/client"
	"github.com/brunoga/raft/examples/internal/exampleutil"
)

func main() {

	nodes := flag.String("nodes",
		"http://localhost:8001,http://localhost:8002,http://localhost:8003",
		"comma-separated list of cluster node HTTP addresses")
	clientID := flag.String("client-id", "idprovider-repl",
		"stable client ID used for idempotent allocations")
	flag.Parse()

	addrs := strings.Split(*nodes, ",")
	c := client.New(addrs, *clientID)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("idprovider REPL — type 'help' for commands")

	for {
		fmt.Print("idprovider> ")
		if !scanner.Scan() {
			break
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "create":
			if len(fields) != 2 {
				fmt.Fprintln(os.Stderr, "usage: create <domain>")
				continue
			}
			if err := c.CreateDomain(ctx, fields[1]); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			fmt.Println("created")

		case "delete":
			if len(fields) != 2 {
				fmt.Fprintln(os.Stderr, "usage: delete <domain>")
				continue
			}
			if err := c.DeleteDomain(ctx, fields[1]); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			fmt.Println("deleted")

		case "list":
			domains, err := c.ListDomains(ctx, false)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			printDomains(domains)

		case "list-stale":
			domains, err := c.ListDomains(ctx, true)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			printDomains(domains)

		case "next":
			if len(fields) < 2 || len(fields) > 3 {
				fmt.Fprintln(os.Stderr, "usage: next <domain> [count]")
				continue
			}
			count := uint64(1)
			if len(fields) == 3 {
				n, err := strconv.ParseUint(fields[2], 10, 64)
				if err != nil || n == 0 {
					fmt.Fprintln(os.Stderr, "error: count must be a positive integer")
					continue
				}
				count = n
			}
			res, err := c.Next(ctx, fields[1], count)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			fmt.Printf("allocated [%d, %d)  (start=%d count=%d)\n",
				res.Start, res.Start+res.Count, res.Start, res.Count)

		case "current":
			if len(fields) != 2 {
				fmt.Fprintln(os.Stderr, "usage: current <domain>")
				continue
			}
			info, err := c.Current(ctx, fields[1], false)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			fmt.Printf("%s: next=%d\n", info.Domain, info.Current)

		case "current-stale":
			if len(fields) != 2 {
				fmt.Fprintln(os.Stderr, "usage: current-stale <domain>")
				continue
			}
			info, err := c.Current(ctx, fields[1], true)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			fmt.Printf("%s: next=%d (stale)\n", info.Domain, info.Current)

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

// showStats polls every node's /status and /metrics for a cluster-wide view.
func showStats(ctx context.Context, addrs []string) {
	fmt.Printf("=== Cluster Status ===\n\n")

	type nodeStatus struct {
		ID          string `json:"id"`
		State       string `json:"state"`
		Leader      string `json:"leader"`
		LastApplied uint64 `json:"last_applied"`
	}

	type result struct {
		addr   string
		status nodeStatus
		err    error
	}

	results := make([]result, len(addrs))
	for i, addr := range addrs {
		var ns nodeStatus
		err := exampleutil.FetchJSON(ctx, strings.TrimSuffix(addr, "/")+"/status", &ns)
		results[i] = result{addr: addr, status: ns, err: err}
	}

	sort.Slice(results, func(i, j int) bool { return results[i].addr < results[j].addr })

	for _, r := range results {
		if r.err != nil {
			fmt.Printf("  %-30s  unreachable (%v)\n", r.addr, r.err)
			continue
		}
		s := r.status
		leaderInfo := ""
		if s.Leader != "" && s.Leader != s.ID {
			leaderInfo = fmt.Sprintf("  leader=%s", s.Leader)
		}
		fmt.Printf("  %-30s  id=%-6s  %-12s  last_applied=%-6d%s\n",
			r.addr, s.ID, s.State, s.LastApplied, leaderInfo)
	}
	fmt.Println()

	// Show Raft metrics from the first reachable node.
	fmt.Println("Raft Metrics:")
	for _, r := range results {
		if r.err != nil {
			continue
		}
		if err := exampleutil.FetchRaftMetrics(ctx, strings.TrimSuffix(r.addr, "/")+"/metrics"); err != nil {
			fmt.Fprintln(os.Stderr, "  metrics:", err)
		}
		break
	}
}

func printDomains(domains map[string]uint64) {

	if len(domains) == 0 {
		fmt.Println("(empty)")
		return
	}
	keys := make([]string, 0, len(domains))
	for k := range domains {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-30s  next=%d\n", k, domains[k])
	}
}

func printHelp() {
	fmt.Print(`commands:
  create <domain>              create an ID domain
  delete <domain>              delete an ID domain
  list                         list all domains (linearizable)
  list-stale                   list all domains (stale)
  next <domain> [count]        allocate IDs from a domain
  current <domain>             current high-water mark (linearizable)
  current-stale <domain>       current high-water mark (stale)
  stats                        show status of all cluster nodes
  help                         show this help
  exit / quit                  exit
`)
}

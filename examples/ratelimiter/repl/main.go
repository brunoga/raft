// ratelimiter-repl is an interactive REPL client for the ratelimiter example.
//
// Usage:
//
//	ratelimiter-repl [--nodes http://localhost:8001,...]
//
// Commands:
//
//	create <key> <max-tokens> <refill-rate>  create a quota (tokens/s)
//	set <key> <max-tokens> <refill-rate>     create or update a quota
//	get <key>                                get quota state (linearizable)
//	get-stale <key>                          get quota state (stale)
//	delete <key>                             delete a quota
//	take <key> [n]                           consume n tokens (default 1)
//	stats [addr]                             show cluster status and Raft metrics
//	help                                     show this help
//	exit / quit                              exit
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/brunoga/raft/examples/internal/exampleutil"
	"github.com/brunoga/raft/examples/ratelimiter/client"
)

func main() {

	nodes := flag.String("nodes",
		"http://localhost:8001,http://localhost:8002,http://localhost:8003",
		"comma-separated list of cluster node HTTP addresses")
	flag.Parse()

	addrs := strings.Split(*nodes, ",")
	c := client.New(addrs)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("ratelimiter REPL — type 'help' for commands")

	for {
		fmt.Print("ratelimiter> ")
		if !scanner.Scan() {
			break
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "create":
			if len(fields) != 4 {
				fmt.Fprintln(os.Stderr, "usage: create <key> <max-tokens> <refill-rate>")
				continue
			}
			q, ok := parseQuota(fields[2], fields[3])
			if !ok {
				continue
			}
			if err := c.CreateQuota(ctx, fields[1], q); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			fmt.Println("created")

		case "set":
			if len(fields) != 4 {
				fmt.Fprintln(os.Stderr, "usage: set <key> <max-tokens> <refill-rate>")
				continue
			}
			q, ok := parseQuota(fields[2], fields[3])
			if !ok {
				continue
			}
			if err := c.SetQuota(ctx, fields[1], q); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			fmt.Println("ok")

		case "get":
			if len(fields) != 2 {
				fmt.Fprintln(os.Stderr, "usage: get <key>")
				continue
			}
			q, err := c.GetQuota(ctx, fields[1], false)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			printQuota(fields[1], q)

		case "get-stale":
			if len(fields) != 2 {
				fmt.Fprintln(os.Stderr, "usage: get-stale <key>")
				continue
			}
			q, err := c.GetQuota(ctx, fields[1], true)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			printQuota(fields[1], q)

		case "delete":
			if len(fields) != 2 {
				fmt.Fprintln(os.Stderr, "usage: delete <key>")
				continue
			}
			if err := c.DeleteQuota(ctx, fields[1]); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			fmt.Println("deleted")

		case "take":
			if len(fields) < 2 || len(fields) > 3 {
				fmt.Fprintln(os.Stderr, "usage: take <key> [n]")
				continue
			}
			n := int64(1)
			if len(fields) == 3 {
				v, err := strconv.ParseInt(fields[2], 10, 64)
				if err != nil || v <= 0 {
					fmt.Fprintln(os.Stderr, "error: n must be a positive integer")
					continue
				}
				n = v
			}
			res, err := c.Take(ctx, fields[1], n)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			if res.Allowed {
				fmt.Printf("allowed  remains=%d\n", res.Remains)
			} else {
				fmt.Printf("denied   remains=%d\n", res.Remains)
			}

		case "stats":
			addr := addrs[0]
			if len(fields) == 2 {
				addr = fields[1]
			} else if len(fields) > 2 {
				fmt.Fprintln(os.Stderr, "usage: stats [addr]")
				continue
			}
			showStats(ctx, addr)

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

func showStats(ctx context.Context, addr string) {
	exampleutil.ShowNodeStats(ctx, addr)
}

func parseQuota(maxStr, refillStr string) (client.Quota, bool) {

	max, err := strconv.ParseInt(maxStr, 10, 64)
	if err != nil || max <= 0 {
		fmt.Fprintln(os.Stderr, "error: max-tokens must be a positive integer")
		return client.Quota{}, false
	}
	refill, err := strconv.ParseInt(refillStr, 10, 64)
	if err != nil || refill <= 0 {
		fmt.Fprintln(os.Stderr, "error: refill-rate must be a positive integer")
		return client.Quota{}, false
	}
	return client.Quota{
		MaxTokens:     max,
		CurrentTokens: max,
		RefillRate:    refill,
	}, true
}

func printQuota(key string, q client.Quota) {
	fmt.Printf("  %-30s  tokens=%d/%d  refill=%d/s\n",
		key, q.CurrentTokens, q.MaxTokens, q.RefillRate)
}

func printHelp() {
	fmt.Print(`commands:
  create <key> <max-tokens> <refill-rate>  create a quota (tokens/s)
  set <key> <max-tokens> <refill-rate>     create or update a quota
  get <key>                                get quota state (linearizable)
  get-stale <key>                          get quota state (stale)
  delete <key>                             delete a quota
  take <key> [n]                           consume n tokens (default 1)
  stats [addr]                             show cluster status and Raft metrics
  help                                     show this help
  exit / quit                              exit
`)
}

// configsvc-repl is an interactive REPL client for the configsvc example.
//
// Usage:
//
//	configsvc-repl [--nodes http://localhost:8001,...]
//
// Commands:
//
//	set <key> <value>   upsert a config entry
//	get <key>           linearizable read
//	get-stale <key>     stale (local) read
//	delete <key>        remove a config entry
//	list                list all entries (linearizable)
//	list-stale          list all entries (stale)
//	watch [key]         stream change events; press Enter to stop
//	stats [addr]        show cluster status and Raft metrics
//	help                show this help
//	exit / quit         exit
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/brunoga/raft/examples/configsvc/client"
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
	fmt.Println("configsvc REPL — type 'help' for commands")

	for {
		fmt.Print("configsvc> ")
		if !scanner.Scan() {
			break
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "set":
			if len(fields) < 3 {
				fmt.Fprintln(os.Stderr, "usage: set <key> <value>")
				continue
			}
			key, value := fields[1], strings.Join(fields[2:], " ")
			if err := c.Set(ctx, key, value); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			fmt.Println("ok")

		case "get":
			if len(fields) != 2 {
				fmt.Fprintln(os.Stderr, "usage: get <key>")
				continue
			}
			entry, err := c.Get(ctx, fields[1], false)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			fmt.Printf("%s = %q  (version %d)\n", fields[1], entry.Value, entry.Version)

		case "get-stale":
			if len(fields) != 2 {
				fmt.Fprintln(os.Stderr, "usage: get-stale <key>")
				continue
			}
			entry, err := c.Get(ctx, fields[1], true)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			fmt.Printf("%s = %q  (version %d, stale)\n", fields[1], entry.Value, entry.Version)

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

		case "list":
			all, err := c.List(ctx, false)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			printEntries(all)

		case "list-stale":
			all, err := c.List(ctx, true)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			printEntries(all)

		case "watch":
			key := ""
			if len(fields) == 2 {
				key = fields[1]
			} else if len(fields) > 2 {
				fmt.Fprintln(os.Stderr, "usage: watch [key]")
				continue
			}
			watchCtx, cancelWatch := context.WithCancel(ctx)
			ch, err := c.Watch(watchCtx, key)
			if err != nil {
				cancelWatch()
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			fmt.Println("watching — press Enter to stop")
			done := make(chan struct{})
			go func() {
				defer close(done)
				for ev := range ch {
					switch ev.Type {
					case "snapshot":
						fmt.Printf("  snapshot  %s = %q\n", ev.Key, ev.Value.Value)
					case "change":
						fmt.Printf("  change    %s = %q\n", ev.Key, ev.Value.Value)
					case "delete":
						fmt.Printf("  delete    %s\n", ev.Key)
					default:
						fmt.Printf("  %s  %s\n", ev.Type, ev.Key)
					}
				}
			}()
			scanner.Scan()
			cancelWatch()
			<-done
			fmt.Println("watch stopped")

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

// showStats prints cluster members, node status, and Raft metrics from addr.
func showStats(ctx context.Context, addr string) {
	addr = strings.TrimSuffix(addr, "/")
	fmt.Printf("=== Stats: %s ===\n\n", addr)

	// Cluster members.
	type memberInfo struct {
		ID       string `json:"id"`
		RaftAddr string `json:"raft_addr"`
		Leader   bool   `json:"leader"`
		Voter    bool   `json:"voter"`
	}
	type membersResp struct {
		Members []memberInfo `json:"members"`
	}
	var mr membersResp
	if err := fetchJSON(ctx, addr+"/members", &mr); err != nil {
		fmt.Fprintln(os.Stderr, "  members:", err)
	} else {
		fmt.Println("Cluster Members:")
		sort.Slice(mr.Members, func(i, j int) bool { return mr.Members[i].ID < mr.Members[j].ID })
		for _, m := range mr.Members {
			role := "follower"
			if m.Leader {
				role = "leader "
			}
			fmt.Printf("  %-12s  %s  %s\n", m.ID, role, m.RaftAddr)
		}
		fmt.Println()
	}

	// Node status.
	type nodeStatus struct {
		NodeID      string `json:"node_id"`
		State       string `json:"state"`
		Term        uint64 `json:"term"`
		LastApplied uint64 `json:"last_applied"`
	}
	var ns nodeStatus
	if err := fetchJSON(ctx, addr+"/status", &ns); err != nil {
		fmt.Fprintln(os.Stderr, "  status:", err)
	} else {
		fmt.Println("Node Status:")
		fmt.Printf("  id=%-12s  state=%-12s  term=%-4d  last_applied=%d\n",
			ns.NodeID, ns.State, ns.Term, ns.LastApplied)
		fmt.Println()
	}

	// Raft metrics (Prometheus text format).
	fmt.Println("Raft Metrics:")
	if err := fetchRaftMetrics(ctx, addr+"/metrics"); err != nil {
		fmt.Fprintln(os.Stderr, "  metrics:", err)
	}
}

// fetchJSON GETs url and decodes the JSON response body into dst.
func fetchJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

// fetchRaftMetrics GETs a Prometheus /metrics endpoint and prints raft_ lines.
func fetchRaftMetrics(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	found := false
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, "raft_") {
			continue
		}
		found = true
		// Format: metric_name{labels} value  or  metric_name value
		name, rest, hasLabels := strings.Cut(line, "{")
		if hasLabels {
			labels, value, _ := strings.Cut(rest, "} ")
			fmt.Printf("  %-42s {%s}  %s\n", name, labels, strings.TrimSpace(value))
		} else {
			parts := strings.Fields(line)
			if len(parts) == 2 {
				fmt.Printf("  %-42s  %s\n", parts[0], parts[1])
			}
		}
	}
	if !found {
		fmt.Println("  (no raft_ metrics — Prometheus not configured on this node)")
	}
	return scanner.Err()
}

func printEntries(all map[string]client.ConfigEntry) {
	if len(all) == 0 {
		fmt.Println("(empty)")
		return
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := all[k]
		fmt.Printf("  %-30s  %q\n", k, e.Value)
	}
}

func printHelp() {
	fmt.Print(`commands:
  set <key> <value>   upsert a config entry
  get <key>           linearizable read
  get-stale <key>     stale (local) read
  delete <key>        remove a config entry
  list                list all entries (linearizable)
  list-stale          list all entries (stale)
  watch [key]         stream change events; press Enter to stop
  stats [addr]        show cluster status and Raft metrics
  help                show this help
  exit / quit         exit
`)
}

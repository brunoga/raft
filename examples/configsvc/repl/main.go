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
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/brunoga/raft/examples/configsvc/client"
	"github.com/brunoga/raft/examples/internal/exampleutil"
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
			addr, ok := exampleutil.ParseOptionalAddr(fields, addrs[0])
			if !ok {
				continue
			}
			exampleutil.ShowNodeStats(ctx, addr)

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

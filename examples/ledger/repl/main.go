// ledger-repl is an interactive REPL client for the ledger example.
//
// Usage:
//
//	ledger-repl [--nodes http://localhost:8001,...] [--client-id myrepl]
//
// Commands:
//
//	create-account <id> <balance>        create an account with initial balance
//	get-account <id>                     get account balance
//	list-accounts                        list all accounts
//	transfer <from> <to> <amount>        move funds between accounts
//	get-transfer <id>                    get a transfer record by ID
//	list-transfers                       list all transfers
//	stats [addr]                         show cluster status and Raft metrics
//	help                                 show this help
//	exit / quit                          exit
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
	"sync/atomic"
	"syscall"

	"github.com/brunoga/raft/examples/internal/exampleutil"
	"github.com/brunoga/raft/examples/ledger/client"
)

func main() {

	nodes := flag.String("nodes",
		"http://localhost:8001,http://localhost:8002,http://localhost:8003",
		"comma-separated list of cluster node HTTP addresses")
	clientID := flag.String("client-id", "ledger-repl",
		"stable client ID used as the idempotency key prefix for transfers")
	flag.Parse()

	addrs := strings.Split(*nodes, ",")
	c := client.New(addrs)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var seq atomic.Uint64

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("ledger REPL — type 'help' for commands")

	for {
		fmt.Print("ledger> ")
		if !scanner.Scan() {
			break
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "create-account":
			if len(fields) != 3 {
				fmt.Fprintln(os.Stderr, "usage: create-account <id> <balance>")
				continue
			}
			balance, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error: balance must be an integer")
				continue
			}
			acc, err := c.CreateAccount(ctx, fields[1], balance)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			printAccount(acc)

		case "get-account":
			if len(fields) != 2 {
				fmt.Fprintln(os.Stderr, "usage: get-account <id>")
				continue
			}
			acc, err := c.GetAccount(ctx, fields[1])
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			printAccount(acc)

		case "list-accounts":
			all, err := c.ListAccounts(ctx)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			if len(all) == 0 {
				fmt.Println("(empty)")
				continue
			}
			ids := make([]string, 0, len(all))
			for id := range all {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				printAccount(all[id])
			}

		case "transfer":
			if len(fields) != 4 {
				fmt.Fprintln(os.Stderr, "usage: transfer <from> <to> <amount>")
				continue
			}
			amount, err := strconv.ParseInt(fields[3], 10, 64)
			if err != nil || amount <= 0 {
				fmt.Fprintln(os.Stderr, "error: amount must be a positive integer")
				continue
			}
			s := seq.Add(1)
			tr, err := c.Transfer(ctx, fields[1], fields[2], amount, *clientID, s)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			printTransfer(tr)

		case "get-transfer":
			if len(fields) != 2 {
				fmt.Fprintln(os.Stderr, "usage: get-transfer <id>")
				continue
			}
			tr, err := c.GetTransfer(ctx, fields[1])
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			printTransfer(tr)

		case "list-transfers":
			all, err := c.ListTransfers(ctx)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			if len(all) == 0 {
				fmt.Println("(empty)")
				continue
			}
			ids := make([]string, 0, len(all))
			for id := range all {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				printTransfer(all[id])
			}

		case "stats":
			addr := addrs[0]
			if len(fields) == 2 {
				addr = fields[1]
			} else if len(fields) > 2 {
				fmt.Fprintln(os.Stderr, "usage: stats [addr]")
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

func printAccount(a client.Account) {

	fmt.Printf("  %-20s  balance=%d\n", a.ID, a.Balance)
}

func printTransfer(t client.Transfer) {
	fmt.Printf("  %-30s  %s → %s  amount=%d\n", t.ID, t.From, t.To, t.Amount)
}

func printHelp() {
	fmt.Print(`commands:
  create-account <id> <balance>        create an account with initial balance
  get-account <id>                     get account balance
  list-accounts                        list all accounts
  transfer <from> <to> <amount>        move funds between accounts
  get-transfer <id>                    get a transfer record by ID
  list-transfers                       list all transfers
  stats [addr]                         show cluster status and Raft metrics
  help                                 show this help
  exit / quit                          exit
`)
}

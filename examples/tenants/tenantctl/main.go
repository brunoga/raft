// tenantctl reads and writes one tenant's items in a tenants cluster.
//
// It exists to show the part a caller of a multi-group deployment has to do
// for itself: work out which Raft group a tenant lives in, and address that
// group. [client.WithGroup] is what turns an ordinary easyraft client into
// one pointed at a single group, after which every call is the same as
// against a single-group store.
//
//	tenantctl --tenant acme put greeting hello
//	tenantctl --tenant acme get greeting
//	tenantctl --tenant acme list
//	tenantctl --tenant acme where
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/brunoga/raft/v2/easyraft"
	"github.com/brunoga/raft/v2/easyraft/client"
	tenantmap "github.com/brunoga/raft/v2/examples/tenants/tenants"
)

func main() {
	if err := run(); err != nil {
		slog.Error("tenantctl: " + err.Error())
		os.Exit(1)
	}
}

func run() error {
	tenant := flag.String("tenant", "", "Tenant name (required)")
	endpoints := flag.String("endpoints", "localhost:8001",
		"Comma-separated HTTP addresses of cluster nodes")
	groups := flag.Int("groups", 8, "Number of groups the cluster was started with")
	timeout := flag.Duration("timeout", 10*time.Second, "Overall deadline for the command")
	flag.Parse()

	args := flag.Args()
	if *tenant == "" || len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: tenantctl --tenant <name> [--endpoints ...] [--groups N] "+
			"<put KEY VALUE | get KEY | list | where>")
		return errors.New("--tenant and a command are required")
	}

	// The whole of the tenant-to-group decision. It is a pure function of the
	// name, so this program and the cluster cannot disagree about it without
	// disagreeing about --groups.
	group, err := tenantmap.GroupFor(*tenant, *groups)
	if err != nil {
		return err
	}
	if args[0] == "where" {
		fmt.Printf("tenant %q is group %d of %d\n", *tenant, group, *groups)
		return nil
	}

	// WithGroup is the only difference from a single-group client. Every node
	// is given, not just one: the client finds whichever leads this group and
	// follows it when it moves.
	c, err := client.New(
		client.WithEndpoints(strings.Split(*endpoints, ",")...),
		client.WithGroup(group),
	)
	if err != nil {
		return err
	}
	items := client.Collection[tenantmap.Item](c, tenantmap.CollectionName)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	switch args[0] {
	case "put":
		if len(args) != 3 {
			return errors.New("put needs a key and a value")
		}
		// Upsert rather than Create: setting a value that is already there is
		// the same command as setting one that is not.
		if err := items.Upsert(ctx, args[1], tenantmap.Item{Value: args[2]}); err != nil {
			return fmt.Errorf("put %s: %w", args[1], err)
		}
		return nil

	case "get":
		if len(args) != 2 {
			return errors.New("get needs a key")
		}
		item, err := items.Read(ctx, args[1])
		if err != nil {
			if errors.Is(err, easyraft.ErrKeyNotFound) {
				return fmt.Errorf("%s: not found in tenant %q", args[1], *tenant)
			}
			return fmt.Errorf("get %s: %w", args[1], err)
		}
		fmt.Println(item.Value)
		return nil

	case "list":
		all, err := items.List(ctx)
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}
		for _, key := range sortedKeys(all) {
			fmt.Printf("%s\t%s\n", key, all[key].Value)
		}
		return nil

	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func sortedKeys(m map[string]tenantmap.Item) []string {
	return slices.Sorted(maps.Keys(m))
}

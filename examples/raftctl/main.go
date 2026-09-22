// Command raftctl is an operator tool for a Raft cluster that is not running.
//
// Everything it does happens against a stopped node's data directory, which is
// the only time these operations make sense: a node that is up does not need
// recovering, and its storage cannot be opened by anything else while it holds
// the lock.
//
//	raftctl inspect  --data-dir DIR                      what one node holds
//	raftctl compare  --data-dir DIR [--data-dir DIR ...] which survivor to keep
//	raftctl recover  --data-dir DIR --id ID ...          rebuild a dead cluster
//
// The interesting one is recover, which is how a cluster that has lost its
// quorum for good comes back. It is also the one operation in the library that
// can lose data, so it prints exactly what it did and refuses to run without
// --confirm.
//
// See README.md for the whole procedure.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/filestore"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "raftctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("a command is required")
	}
	switch args[0] {
	case "inspect":
		return cmdInspect(args[1:])
	case "compare":
		return cmdCompare(args[1:])
	case "recover":
		return cmdRecover(args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `raftctl — offline operator tool for a stopped Raft node

  raftctl inspect --data-dir DIR
        Report what one node's storage holds: its term, log bounds, snapshot,
        the membership it would restart with, and which of its entries are not
        provably committed.

  raftctl compare --data-dir DIR --data-dir DIR [...]
        Rank survivors by how up to date they are and name the one whose
        history a recovery should keep.

  raftctl recover --data-dir DIR --id ID [--learner ID ...] [options] --confirm
        Rewrite a stopped node's membership so it can elect itself, bringing
        back a cluster that lost its quorum for good. Destructive; see README.

Every command requires the node to be stopped. filestore holds an exclusive
lock on its directory, so running these against a live node fails rather than
corrupting anything.
`)
}

// openStore opens a data directory for inspection or recovery.
//
// The lock filestore takes is what makes this safe to hand to an operator: a
// directory a node is still using cannot be opened here at all, so the "make
// sure it is stopped" step of the procedure is checked rather than trusted.
func openStore(dir string) (*filestore.FileStore, error) {
	fs, err := filestore.Open(dir)
	if errors.Is(err, filestore.ErrLocked) {
		return nil, fmt.Errorf("%s is in use: stop the node before running this", dir)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	return fs, nil
}

// ---- inspect ----------------------------------------------------------------

func cmdInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	dir := fs.String("data-dir", "", "data directory of a stopped node (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("inspect: --data-dir is required")
	}

	info, err := inspect(*dir)
	if err != nil {
		return err
	}
	return printInfo(os.Stdout, *dir, &info)
}

func inspect(dir string) (raft.RecoveryInfo, error) {
	store, err := openStore(dir)
	if err != nil {
		return raft.RecoveryInfo{}, err
	}
	defer func() { _ = store.Close() }()
	return raft.InspectStorage(context.Background(), store)
}

// printer writes formatted output and remembers the first failure, so a broken
// pipe is reported once at the end rather than checked after every line or
// thrown away.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, args...)
}

func printInfo(w io.Writer, dir string, info *raft.RecoveryInfo) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	p := &printer{w: tw}
	p.printf("%s\n", dir)
	p.printf("  term\t%d\n", info.Term)
	if info.VotedFor != "" {
		p.printf("  voted for\t%s\n", info.VotedFor)
	}
	p.printf("  log\t%s\n", logRange(info))
	if info.SnapshotIndex > 0 {
		p.printf("  snapshot\tindex %d term %d\n", info.SnapshotIndex, info.SnapshotTerm)
	} else {
		p.printf("  snapshot\tnone\n")
	}
	p.printf("  members\t%s\n", describeMembers(info))
	p.printf("  committed through\t%d\n", info.KnownCommittedIndex)

	if from, to, ok := info.UncommittedBand(); ok {
		p.printf("  in doubt\t%d..%d (%d entries)\n", from, to, to-from+1)
	} else {
		p.printf("  in doubt\tnothing\n")
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if from, to, ok := info.UncommittedBand(); ok {
		p.printf("\n  Entries %d..%d either committed on nodes that are gone or were still\n"+
			"  in flight. Recovering this node either makes them part of the cluster's\n"+
			"  history or discards them; nothing here can tell which they were.\n", from, to)
	}
	if !info.MembersComplete {
		p.printf("\n  The membership above is what could be read from storage, and is not the\n" +
			"  whole of it: this node has no snapshot and no entry recording a complete\n" +
			"  configuration, so the rest lived only in the peer list it was started with.\n")
	}
	return p.err
}

func logRange(info *raft.RecoveryInfo) string {
	if info.LastIndex == 0 {
		return "empty"
	}
	if info.FirstIndex == 0 {
		return fmt.Sprintf("compacted through %d", info.LastIndex)
	}
	return fmt.Sprintf("%d..%d (last term %d)", info.FirstIndex, info.LastIndex, info.LastTerm)
}

func describeMembers(info *raft.RecoveryInfo) string {
	if len(info.Members) == 0 {
		return "none recorded"
	}
	parts := make([]string, 0, len(info.Members))
	for _, m := range info.Members {
		role := "learner"
		if m.Voter {
			role = "voter"
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", m.ID, role))
	}
	out := strings.Join(parts, ", ")
	if info.JointMembers != nil {
		out += " — joint reconfiguration in progress"
	}
	return out
}

// ---- compare ----------------------------------------------------------------

type survivor struct {
	dir  string
	info raft.RecoveryInfo
}

func cmdCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	var dirs stringList
	fs.Var(&dirs, "data-dir", "data directory of a stopped node (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(dirs) < 2 {
		return errors.New("compare: give --data-dir at least twice; " +
			"use inspect for a single node")
	}

	survivors := make([]survivor, 0, len(dirs))
	for _, dir := range dirs {
		info, err := inspect(dir)
		if err != nil {
			return err
		}
		survivors = append(survivors, survivor{dir: dir, info: info})
	}

	best := 0
	for i := range survivors {
		if survivors[i].info.MoreRecentThan(&survivors[best].info) {
			best = i
		}
	}

	for _, s := range survivors {
		if err := printInfo(os.Stdout, s.dir, &s.info); err != nil {
			return err
		}
		fmt.Println()
	}

	fmt.Printf("Recover %s.\n\n", survivors[best].dir)
	fmt.Print("It has the most complete log by the rule elections use: the highest last\n" +
		"term, and the longest log among those. Any other choice silently drops\n" +
		"whatever this one has and the chosen one does not.\n\n" +
		"Erase the storage of every other survivor before restarting them. Their\n" +
		"logs are divergent history now, and a node that comes back holding entries\n" +
		"the recovered node does not have can disrupt the cluster it is no longer\n" +
		"part of.\n")
	return nil
}

// ---- recover ----------------------------------------------------------------

func cmdRecover(args []string) error {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	dir := fs.String("data-dir", "", "data directory of the stopped node to recover (required)")
	id := fs.String("id", "", "that node's own ID (required)")
	var learners stringList
	fs.Var(&learners, "learner", "ID of a node to name as a non-voting member (repeatable)")
	knownCommitted := fs.Uint64("known-committed", 0,
		"highest index proven committed from outside storage, e.g. a durable state machine's applied index")
	discard := fs.Bool("discard-uncommitted", false,
		"discard entries that are not provably committed instead of keeping them")
	confirm := fs.Bool("confirm", false, "required: acknowledge that this rewrites the node's durable state")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *id == "" {
		return errors.New("recover: --data-dir and --id are required")
	}
	if !*confirm {
		return errors.New("recover: refusing to rewrite durable state without --confirm. " +
			"Run 'raftctl inspect --data-dir " + *dir + "' first and read what is in doubt")
	}

	store, err := openStore(*dir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	members := []raft.PeerConfig{{ID: raft.NodeID(*id), Voter: true}}
	for _, l := range learners {
		members = append(members, raft.PeerConfig{ID: raft.NodeID(l), Voter: false})
	}

	var opts []raft.RecoverOption
	if *knownCommitted > 0 {
		opts = append(opts, raft.WithKnownCommitted(raft.Index(*knownCommitted)))
	}
	if *discard {
		opts = append(opts, raft.DiscardUncommitted())
	}

	report, err := raft.RecoverCluster(context.Background(), store, raft.NodeID(*id), members, opts...)
	if err != nil {
		return err
	}

	return printReport(&report, *id)
}

func printReport(r *raft.RecoveryReport, id string) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	p := &printer{w: tw}
	p.printf("Recovered %s.\n", id)
	p.printf("  membership was\t%s\n", joinPeers(r.MembersBefore))
	p.printf("  membership now\t%s\n", joinPeers(r.MembersAfter))
	p.printf("  entry written at\tindex %d, term %d\n", r.Index, r.Term)
	p.printf("  committed through\t%d\n", r.KnownCommittedIndex)
	if r.PromotedFrom != 0 {
		p.printf("  promoted\t%d..%d\n", r.PromotedFrom, r.PromotedTo)
	}
	if r.DiscardedFrom != 0 {
		p.printf("  discarded\t%d..%d\n", r.DiscardedFrom, r.DiscardedTo)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	p.w = os.Stdout

	switch {
	case r.PromotedFrom != 0:
		p.printf("\nEntries %d..%d were not provably committed and are now part of this\n"+
			"cluster's history. A client told one of those writes had failed will find\n"+
			"that it succeeded. Keep this report.\n", r.PromotedFrom, r.PromotedTo)
	case r.DiscardedFrom != 0:
		p.printf("\nEntries %d..%d were not provably committed and have been discarded. A\n"+
			"client told one of those writes had succeeded will find that it did not.\n"+
			"Keep this report.\n", r.DiscardedFrom, r.DiscardedTo)
	default:
		p.printf("\nNothing was in doubt: every entry this node held was provably committed.\n")
	}

	p.printf("\nNext: erase every other survivor's data directory, start this node, and\n" +
		"add the others back as new members once it is serving.\n")
	return p.err
}

func joinPeers(peers []raft.PeerConfig) string {
	if len(peers) == 0 {
		return "none recorded"
	}
	parts := make([]string, 0, len(peers))
	for _, p := range peers {
		role := "learner"
		if p.Voter {
			role = "voter"
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", p.ID, role))
	}
	return strings.Join(parts, ", ")
}

// stringList collects a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

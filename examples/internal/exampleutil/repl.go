package exampleutil

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
)

// FetchJSON GETs a URL and decodes the JSON response into dst.
func FetchJSON(ctx context.Context, url string, dst any) error {
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

// ShowNodeStats prints cluster members, node status, and Raft metrics fetched
// from a single node address (scheme+host, no trailing slash required).
func ShowNodeStats(ctx context.Context, addr string) {
	addr = strings.TrimSuffix(addr, "/")
	fmt.Printf("=== Stats: %s ===\n\n", addr)

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
	if err := FetchJSON(ctx, addr+"/members", &mr); err != nil {
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

	type nodeStatus struct {
		NodeID      string `json:"node_id"`
		State       string `json:"state"`
		Term        uint64 `json:"term"`
		LastApplied uint64 `json:"last_applied"`
	}
	var ns nodeStatus
	if err := FetchJSON(ctx, addr+"/status", &ns); err != nil {
		fmt.Fprintln(os.Stderr, "  status:", err)
	} else {
		fmt.Println("Node Status:")
		fmt.Printf("  id=%-12s  state=%-12s  term=%-4d  last_applied=%d\n",
			ns.NodeID, ns.State, ns.Term, ns.LastApplied)
		fmt.Println()
	}

	fmt.Println("Raft Metrics:")
	if err := FetchRaftMetrics(ctx, addr+"/metrics"); err != nil {
		fmt.Fprintln(os.Stderr, "  metrics:", err)
	}
}

// FetchRaftMetrics GETs a Prometheus /metrics endpoint and prints lines
// starting with "raft_".
func FetchRaftMetrics(ctx context.Context, url string) error {
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
		fmt.Println("  (no raft_ metrics)")
	}
	return scanner.Err()
}

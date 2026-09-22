package easyraft_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brunoga/raft/v2/easyraft"
)

// TestScan_ThroughTheAPIAndHTTP walks a collection page by page through the
// typed API and then through the HTTP endpoint, and checks the two agree on
// where the page boundaries fall.
func TestScan_ThroughTheAPIAndHTTP(t *testing.T) {
	er, httpAddr := startRevisionNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Two namespaces in one collection, so a prefix has something to exclude.
	var sessions, other []string
	for i := range 7 {
		key := fmt.Sprintf("session/%02d", i)
		if err := er.Create(ctx, key, Counter{Value: uint64(i)}); err != nil {
			t.Fatalf("Create %s: %v", key, err)
		}
		sessions = append(sessions, key)
	}
	for i := range 3 {
		key := fmt.Sprintf("user/%02d", i)
		if err := er.Create(ctx, key, Counter{Value: 100}); err != nil {
			t.Fatalf("Create %s: %v", key, err)
		}
		other = append(other, key)
	}

	// The typed API, three at a time.
	var walked []string
	opts := easyraft.ScanOptions{Prefix: "session/", Limit: 3}
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("the scan did not end")
		}
		page, err := er.Scan(ctx, opts)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		for _, item := range page.Items {
			walked = append(walked, item.Key)
			if item.Revision == 0 {
				t.Errorf("%s came back with revision 0", item.Key)
			}
		}
		if page.Next == "" {
			break
		}
		opts.After = page.Next
	}
	if fmt.Sprint(walked) != fmt.Sprint(sessions) {
		t.Fatalf("the typed scan walked %v, want %v", walked, sessions)
	}

	// ListPrefix answers the same set in one call, and excludes the rest.
	got, err := er.ListPrefix(ctx, "session/")
	if err != nil {
		t.Fatalf("ListPrefix: %v", err)
	}
	if len(got) != len(sessions) {
		t.Errorf("ListPrefix returned %d items, want %d", len(got), len(sessions))
	}
	for _, key := range other {
		if _, unwanted := got[key]; unwanted {
			t.Errorf("ListPrefix(%q) returned %s", "session/", key)
		}
	}

	// The same walk over HTTP, following the Link header rather than building
	// the next URL.
	walked = walked[:0]
	next := "/default?prefix=" + neturl.QueryEscape("session/") + "&limit=3"
	for pages := 0; next != ""; pages++ {
		if pages > 10 {
			t.Fatal("the HTTP scan did not end")
		}
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+httpAddr+next, http.NoBody)
		if reqErr != nil {
			t.Fatal(reqErr)
		}
		resp, doErr := http.DefaultClient.Do(req)
		if doErr != nil {
			t.Fatal(doErr)
		}
		var page map[string]Counter
		decodeErr := json.NewDecoder(resp.Body).Decode(&page)
		link, cursor := resp.Header.Get("Link"), resp.Header.Get("X-Raft-Next-Cursor")
		revision := resp.Header.Get("X-Raft-Revision")
		_ = resp.Body.Close()
		if decodeErr != nil {
			t.Fatalf("decode page: %v", decodeErr)
		}
		if revision == "" {
			t.Error("a list response carried no X-Raft-Revision")
		}
		if len(page) > 3 {
			t.Fatalf("a page held %d items under limit=3", len(page))
		}
		keys := make([]string, 0, len(page))
		for key := range page {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		walked = append(walked, keys...)

		if cursor == "" {
			if link != "" {
				t.Errorf("no cursor but a Link header %q", link)
			}
			break
		}
		if !strings.Contains(link, `rel="next"`) {
			t.Fatalf("Link header %q does not name the next page", link)
		}
		next = strings.TrimSuffix(strings.TrimPrefix(link, "<"), `>; rel="next"`)
	}
	if fmt.Sprint(walked) != fmt.Sprint(sessions) {
		t.Errorf("the HTTP scan walked %v, want %v", walked, sessions)
	}

	// A list with no pagination still answers the whole collection, unchanged.
	status, body, _ := doRequest(t, http.MethodGet, "http://"+httpAddr+"/default", "", nil)
	if status != http.StatusOK {
		t.Fatalf("plain list: %d", status)
	}
	var whole map[string]Counter
	if err := json.Unmarshal([]byte(body), &whole); err != nil {
		t.Fatalf("decode the whole collection: %v", err)
	}
	if len(whole) != len(sessions)+len(other) {
		t.Errorf("the plain list returned %d items, want %d", len(whole), len(sessions)+len(other))
	}

	// A limit that is not a number is refused rather than ignored.
	if status, _, _ := doRequest(t, http.MethodGet,
		"http://"+httpAddr+"/default?limit=lots", "", nil); status != http.StatusBadRequest {
		t.Errorf("limit=lots answered %d, want 400", status)
	}
	if status, _, _ := doRequest(t, http.MethodGet,
		"http://"+httpAddr+"/default?limit=-1", "", nil); status != http.StatusBadRequest {
		t.Errorf("limit=-1 answered %d, want 400", status)
	}
}

package easyraft

import (
	"fmt"
	"testing"
)

// seedCollection fills a collection with the keys given, each at its own
// revision, so a scan has something ordered to walk.
func seedCollection(t *testing.T, s *Store, collection string, keys ...string) {
	t.Helper()
	for i, key := range keys {
		if _, err := applyAt(t, s, uint64(i+1), &command{
			Op: opUpsert, Collection: collection, Key: key, Value: jsonValue(t, key),
		}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
}

func scanKeys(t *testing.T, s *Store, collection string, opts ScanOptions) (keys []string, next string) {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.scanKeysLocked(collection, opts)
}

// TestScan_OrdersAndNarrows covers the two things a scan does before it
// paginates: it returns keys in order, and a prefix removes the rest.
func TestScan_OrdersAndNarrows(t *testing.T) {
	s := newTestStore(t, &config{})
	seedCollection(t, s, "c", "b/2", "a/1", "b/1", "c/1", "a/2", "b/10")

	got, next := scanKeys(t, s, "c", ScanOptions{})
	want := []string{"a/1", "a/2", "b/1", "b/10", "b/2", "c/1"}
	if next != "" {
		t.Errorf("an unpaginated scan returned the cursor %q", next)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("scan returned %v, want %v", got, want)
	}

	got, _ = scanKeys(t, s, "c", ScanOptions{Prefix: "b/"})
	want = []string{"b/1", "b/10", "b/2"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("prefix scan returned %v, want %v", got, want)
	}

	if got, _ := scanKeys(t, s, "c", ScanOptions{Prefix: "nothing"}); len(got) != 0 {
		t.Errorf("a prefix that matches nothing returned %v", got)
	}
	if got, _ := scanKeys(t, s, "missing", ScanOptions{}); len(got) != 0 {
		t.Errorf("a scan of a collection that does not exist returned %v", got)
	}
}

// TestScan_PagesCoverEveryKeyExactlyOnce walks a collection in pages of every
// size from one to one past its length, and checks each walk reassembles the
// whole collection in order with nothing skipped or repeated.
func TestScan_PagesCoverEveryKeyExactlyOnce(t *testing.T) {
	s := newTestStore(t, &config{})
	var all []string
	for i := range 10 {
		all = append(all, fmt.Sprintf("k%02d", i))
	}
	seedCollection(t, s, "c", all...)

	for limit := 1; limit <= len(all)+1; limit++ {
		var walked []string
		opts := ScanOptions{Limit: limit}
		for pages := 0; ; pages++ {
			if pages > len(all)+2 {
				t.Fatalf("limit %d: the scan did not end after %d pages", limit, pages)
			}
			keys, next := scanKeys(t, s, "c", opts)
			if len(keys) > limit {
				t.Fatalf("limit %d: a page held %d keys", limit, len(keys))
			}
			walked = append(walked, keys...)
			if next == "" {
				break
			}
			if next != keys[len(keys)-1] {
				t.Fatalf("limit %d: cursor %q is not the last key of the page %v", limit, next, keys)
			}
			opts.After = next
		}
		if fmt.Sprint(walked) != fmt.Sprint(all) {
			t.Errorf("limit %d walked %v, want %v", limit, walked, all)
		}
	}
}

// TestScan_LastFullPageCarriesNoCursor pins the reason the cursor is set from
// what was left behind rather than from the page being full. A caller that
// pages until the cursor is empty must not be sent back for a page that can
// only be empty.
func TestScan_LastFullPageCarriesNoCursor(t *testing.T) {
	s := newTestStore(t, &config{})
	seedCollection(t, s, "c", "a", "b", "c", "d")

	keys, next := scanKeys(t, s, "c", ScanOptions{Limit: 2, After: "b"})
	if fmt.Sprint(keys) != "[c d]" {
		t.Fatalf("page is %v, want [c d]", keys)
	}
	if next != "" {
		t.Errorf("a page that ended the collection carries the cursor %q", next)
	}
}

// TestScan_CursorIsAKeyNotAnOffset covers what a page boundary survives. A key
// inserted before the cursor after its page was read is missed, and one
// inserted after it is seen -- but nothing already returned comes back, which
// an offset could not promise.
func TestScan_CursorIsAKeyNotAnOffset(t *testing.T) {
	s := newTestStore(t, &config{})
	seedCollection(t, s, "c", "b", "d", "f")

	first, next := scanKeys(t, s, "c", ScanOptions{Limit: 2})
	if fmt.Sprint(first) != "[b d]" || next != "d" {
		t.Fatalf("first page %v cursor %q, want [b d] and d", first, next)
	}

	// Two writes between the pages: one before the cursor, one after.
	if _, err := applyAt(t, s, 50, &command{
		Op: opUpsert, Collection: "c", Key: "a", Value: jsonValue(t, "a"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyAt(t, s, 51, &command{
		Op: opUpsert, Collection: "c", Key: "e", Value: jsonValue(t, "e"),
	}); err != nil {
		t.Fatal(err)
	}

	second, _ := scanKeys(t, s, "c", ScanOptions{Limit: 2, After: next})
	if fmt.Sprint(second) != "[e f]" {
		t.Errorf("second page is %v, want [e f]: a key before the cursor must not reappear "+
			"and a key after it must be seen", second)
	}
}

// TestScan_DeletedCursorStillResumes checks the case a key-based cursor has to
// handle and an index-based one does not arise for: the key the cursor names
// is gone by the time the next page is asked for.
func TestScan_DeletedCursorStillResumes(t *testing.T) {
	s := newTestStore(t, &config{})
	seedCollection(t, s, "c", "a", "b", "c", "d")

	_, next := scanKeys(t, s, "c", ScanOptions{Limit: 2})
	if next != "b" {
		t.Fatalf("cursor is %q, want b", next)
	}
	if _, err := applyAt(t, s, 60, &command{Op: opDelete, Collection: "c", Key: "b"}); err != nil {
		t.Fatal(err)
	}

	rest, cursor := scanKeys(t, s, "c", ScanOptions{Limit: 2, After: next})
	if fmt.Sprint(rest) != "[c d]" || cursor != "" {
		t.Errorf("after deleting the cursor's own key the scan returned %v cursor %q, want [c d] and none",
			rest, cursor)
	}
}

// TestScan_TypedPageCarriesValuesAndRevisions checks the typed wrapper on top
// of the key selection: values decoded, revisions attached, order kept.
func TestScan_TypedPageCarriesValuesAndRevisions(t *testing.T) {
	s := newTestStore(t, &config{})
	seedCollection(t, s, "c", "a", "b", "c")
	coll := AddCollection[string](s, "c")

	page, err := coll.ScanStale(ScanOptions{Limit: 2})
	if err != nil {
		t.Fatalf("ScanStale: %v", err)
	}
	if len(page.Items) != 2 || page.Next != "b" {
		t.Fatalf("page holds %d items with cursor %q, want 2 and b", len(page.Items), page.Next)
	}
	for i, want := range []string{"a", "b"} {
		item := page.Items[i]
		if item.Key != want || item.Value != want {
			t.Errorf("item %d is %q=%q, want %q", i, item.Key, item.Value, want)
		}
		if item.Revision != uint64(i+1) {
			t.Errorf("item %q has revision %d, want %d", item.Key, item.Revision, i+1)
		}
	}

	rest, restErr := coll.ScanStale(ScanOptions{Limit: 2, After: page.Next})
	if restErr != nil {
		t.Fatalf("ScanStale page 2: %v", restErr)
	}
	if len(rest.Items) != 1 || rest.Items[0].Key != "c" || rest.Next != "" {
		t.Errorf("second page is %+v, want the single key c and no cursor", rest)
	}

	if _, err := coll.ScanStale(ScanOptions{Limit: -1}); err == nil {
		t.Error("a negative limit was accepted")
	}

	got, listErr := coll.ListPrefixStale("")
	if listErr != nil {
		t.Fatalf("ListPrefixStale: %v", listErr)
	}
	if len(got) != 3 {
		t.Errorf("ListPrefixStale returned %d items, want 3", len(got))
	}
}

// TestScan_WitnessRefuses keeps the scan paths in line with every other read:
// a witness stores no data, so it says so rather than answering empty.
func TestScan_WitnessRefuses(t *testing.T) {
	s := newTestStore(t, &config{Witness: true})
	coll := AddCollection[string](s, "c")

	if _, err := coll.ScanStale(ScanOptions{}); err != ErrWitness {
		t.Errorf("ScanStale on a witness: %v, want ErrWitness", err)
	}
	if _, err := coll.ListPrefixStale("x"); err != ErrWitness {
		t.Errorf("ListPrefixStale on a witness: %v, want ErrWitness", err)
	}
}

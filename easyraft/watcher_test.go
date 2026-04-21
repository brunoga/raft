package easyraft_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brunoga/raft/easyraft"
)

// sseEvent holds one parsed Server-Sent Event.
type sseEvent struct {
	Type string
	Data map[string]json.RawMessage
}

// readSSEEvents parses the SSE stream from body and sends events on the
// returned channel. The goroutine exits when the body is closed or EOF.
func readSSEEvents(body interface{ Read([]byte) (int, error) }) <-chan sseEvent {
	ch := make(chan sseEvent, 32)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(body)
		var eventType string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				eventType = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				raw := strings.TrimPrefix(line, "data: ")
				var data map[string]json.RawMessage
				if err := json.Unmarshal([]byte(raw), &data); err == nil {
					ch <- sseEvent{Type: eventType, Data: data}
				}
			}
		}
	}()
	return ch
}

// jsonString decodes a JSON-encoded string value from raw.
func jsonString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// jsonBool decodes a JSON-encoded bool value from raw.
func jsonBool(raw json.RawMessage) bool {
	var b bool
	_ = json.Unmarshal(raw, &b)
	return b
}

// --- Watcher unit tests (no Raft, no network) --------------------------------

func TestWatcher_Notify_AllSubscriber(t *testing.T) {
	w := easyraft.NewWatcher[string]()
	ch := w.Subscribe("")
	defer w.Unsubscribe("", ch)

	v := "hello"
	w.Notify("k1", &v, false)

	select {
	case ev := <-ch:
		if ev.Key != "k1" {
			t.Errorf("Key = %q, want %q", ev.Key, "k1")
		}
		if ev.Value == nil || *ev.Value != "hello" {
			t.Errorf("Value = %v, want %q", ev.Value, "hello")
		}
		if ev.Deleted {
			t.Error("Deleted = true, want false")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestWatcher_Notify_KeySubscriber(t *testing.T) {
	w := easyraft.NewWatcher[string]()
	ch := w.Subscribe("target")
	defer w.Unsubscribe("target", ch)

	// Event for a different key must not arrive.
	other := "other"
	w.Notify("other-key", &other, false)
	select {
	case ev := <-ch:
		t.Fatalf("received unexpected event for key %q", ev.Key)
	case <-time.After(50 * time.Millisecond):
	}

	// Event for the subscribed key must arrive.
	v := "value"
	w.Notify("target", &v, false)
	select {
	case ev := <-ch:
		if ev.Key != "target" {
			t.Errorf("Key = %q, want %q", ev.Key, "target")
		}
		if ev.Value == nil || *ev.Value != "value" {
			t.Errorf("Value = %v, want %q", ev.Value, "value")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for targeted event")
	}
}

func TestWatcher_Notify_DeleteEvent(t *testing.T) {
	w := easyraft.NewWatcher[string]()
	ch := w.Subscribe("")
	defer w.Unsubscribe("", ch)

	w.Notify("k1", nil, true)

	select {
	case ev := <-ch:
		if !ev.Deleted {
			t.Error("Deleted = false, want true")
		}
		if ev.Value != nil {
			t.Errorf("Value = %v, want nil on delete", ev.Value)
		}
		if ev.Key != "k1" {
			t.Errorf("Key = %q, want %q", ev.Key, "k1")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestWatcher_Unsubscribe(t *testing.T) {
	w := easyraft.NewWatcher[string]()
	ch := w.Subscribe("")
	w.Unsubscribe("", ch) // remove immediately

	v := "hello"
	w.Notify("k1", &v, false)

	select {
	case ev := <-ch:
		t.Fatalf("received event on unsubscribed channel: key=%q", ev.Key)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWatcher_FullChannelDrops(t *testing.T) {
	w := easyraft.NewWatcher[string]()
	ch := w.Subscribe("")
	defer w.Unsubscribe("", ch)

	// Fill the 64-entry buffer without draining.
	v := "x"
	for i := range 64 {
		_ = i
		w.Notify("k", &v, false)
	}

	// The 65th Notify must not block.
	done := make(chan struct{})
	go func() {
		w.Notify("k", &v, false)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Notify blocked on a full channel")
	}
}

func TestWatcher_MultipleSubscribers(t *testing.T) {
	w := easyraft.NewWatcher[string]()

	allCh := w.Subscribe("")         // receives everything
	keyCh := w.Subscribe("specific") // receives only "specific"
	defer w.Unsubscribe("", allCh)
	defer w.Unsubscribe("specific", keyCh)

	v := "val"
	w.Notify("other", &v, false)

	// allCh gets it, keyCh does not.
	select {
	case ev := <-allCh:
		if ev.Key != "other" {
			t.Errorf("allCh: Key = %q, want other", ev.Key)
		}
	case <-time.After(time.Second):
		t.Fatal("allCh timeout")
	}
	select {
	case ev := <-keyCh:
		t.Fatalf("keyCh received unexpected event for key %q", ev.Key)
	case <-time.After(20 * time.Millisecond):
	}

	w.Notify("specific", &v, false)

	// Both channels receive "specific".
	for _, ch := range []chan easyraft.ChangeEvent[string]{allCh, keyCh} {
		select {
		case ev := <-ch:
			if ev.Key != "specific" {
				t.Errorf("Key = %q, want specific", ev.Key)
			}
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for specific event")
		}
	}
}

// --- ServeSSE integration tests (real HTTP, no Raft) -------------------------

func TestWatcher_ServeSSE_SnapshotAndLiveEvents(t *testing.T) {
	w := easyraft.NewWatcher[string]()
	snapshot := map[string]string{"existing": "snap-value"}

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.ServeSSE(rw, r, "", snapshot)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	events := readSSEEvents(resp.Body)

	// Snapshot event.
	select {
	case ev := <-events:
		if ev.Type != "snapshot" {
			t.Errorf("event type = %q, want snapshot", ev.Type)
		}
		if jsonString(ev.Data["key"]) != "existing" {
			t.Errorf("snapshot key = %q, want existing", jsonString(ev.Data["key"]))
		}
		// value is embedded as the string value of ChangeEvent.Value (*string)
		if jsonString(ev.Data["value"]) != "snap-value" {
			t.Errorf("snapshot value = %q, want snap-value", jsonString(ev.Data["value"]))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for snapshot event")
	}

	// Live change event.
	v := "live-value"
	w.Notify("new-key", &v, false)
	select {
	case ev := <-events:
		if ev.Type != "change" {
			t.Errorf("event type = %q, want change", ev.Type)
		}
		if jsonString(ev.Data["key"]) != "new-key" {
			t.Errorf("key = %q, want new-key", jsonString(ev.Data["key"]))
		}
		if jsonString(ev.Data["value"]) != "live-value" {
			t.Errorf("value = %q, want live-value", jsonString(ev.Data["value"]))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for change event")
	}

	// Live delete event.
	w.Notify("new-key", nil, true)
	select {
	case ev := <-events:
		if ev.Type != "delete" {
			t.Errorf("event type = %q, want delete", ev.Type)
		}
		if jsonString(ev.Data["key"]) != "new-key" {
			t.Errorf("key = %q, want new-key", jsonString(ev.Data["key"]))
		}
		if !jsonBool(ev.Data["deleted"]) {
			t.Error("deleted = false, want true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for delete event")
	}
}

func TestWatcher_ServeSSE_KeyFilter(t *testing.T) {
	w := easyraft.NewWatcher[string]()
	snapshot := map[string]string{
		"target": "target-snap",
		"other":  "other-snap",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.ServeSSE(rw, r, "target", snapshot)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	events := readSSEEvents(resp.Body)

	// Only the "target" key should appear in the snapshot.
	select {
	case ev := <-events:
		if ev.Type != "snapshot" {
			t.Errorf("event type = %q, want snapshot", ev.Type)
		}
		if jsonString(ev.Data["key"]) != "target" {
			t.Errorf("snapshot key = %q, want target", jsonString(ev.Data["key"]))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for snapshot event")
	}

	// No second snapshot event (for "other") should arrive.
	select {
	case ev := <-events:
		t.Fatalf("unexpected second event: type=%q key=%q", ev.Type, jsonString(ev.Data["key"]))
	case <-time.After(50 * time.Millisecond):
	}

	// Live event for a different key must not arrive.
	v := "ignored"
	w.Notify("other", &v, false)
	select {
	case ev := <-events:
		t.Fatalf("received event for non-subscribed key %q", jsonString(ev.Data["key"]))
	case <-time.After(50 * time.Millisecond):
	}

	// Live event for the subscribed key must arrive.
	v2 := "target-live"
	w.Notify("target", &v2, false)
	select {
	case ev := <-events:
		if ev.Type != "change" {
			t.Errorf("event type = %q, want change", ev.Type)
		}
		if jsonString(ev.Data["key"]) != "target" {
			t.Errorf("key = %q, want target", jsonString(ev.Data["key"]))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for live event")
	}
}

func TestWatcher_ServeSSE_NonFlusher(t *testing.T) {
	w := easyraft.NewWatcher[string]()

	// httptest.NewRecorder does implement Flusher, so we need a custom one that doesn't.
	type nonFlusher struct{ http.ResponseWriter }

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.ServeSSE(nonFlusher{rw}, r, "", nil)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Should get 500 with "streaming not supported".
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

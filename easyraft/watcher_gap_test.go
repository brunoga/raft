package easyraft_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/brunoga/raft/easyraft"
)

// drain reads everything currently buffered on ch without blocking.
func drain[T any](ch <-chan easyraft.ChangeEvent[T]) []easyraft.ChangeEvent[T] {
	var out []easyraft.ChangeEvent[T]
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// TestWatcher_ReportsDroppedEventsAsAGap pins the invariant that a subscriber
// which falls behind is told so. Losing events silently leaves a consumer
// confidently wrong about the state it is tracking; a gap event tells it to
// resynchronise.
func TestWatcher_ReportsDroppedEventsAsAGap(t *testing.T) {
	w := easyraft.NewWatcher[Counter]()
	ch := w.Subscribe("")
	defer w.Unsubscribe("", ch)

	// Overrun the 64-event buffer without reading anything.
	const overrun = 200
	for i := 0; i < overrun; i++ {
		v := Counter{Value: uint64(i)}
		w.Notify("k", &v, false)
	}

	buffered := drain(ch)
	if len(buffered) == 0 {
		t.Fatal("no events were buffered at all")
	}
	for _, ev := range buffered {
		if ev.Gap {
			t.Fatal("a gap was reported before any event could be dropped and re-delivered")
		}
	}

	// With room again, the next event must be preceded by a gap marker.
	v := Counter{Value: 999}
	w.Notify("k", &v, false)

	after := drain(ch)
	if len(after) == 0 {
		t.Fatal("no events delivered after the subscriber caught up")
	}
	if !after[0].Gap {
		t.Fatalf("first event after catching up = %+v, want a gap marker", after[0])
	}
	if after[0].Key != "" || after[0].Value != nil {
		t.Errorf("gap event carries entry data: %+v", after[0])
	}
	if len(after) < 2 || after[1].Value == nil || after[1].Value.Value != 999 {
		t.Errorf("the event following the gap was not delivered: %+v", after)
	}
}

// TestWatcher_SequenceNumbersAreContiguous pins that a consumer can detect a
// discontinuity on its own, which is what makes the gap marker verifiable.
func TestWatcher_SequenceNumbersAreContiguous(t *testing.T) {
	w := easyraft.NewWatcher[Counter]()
	ch := w.Subscribe("")
	defer w.Unsubscribe("", ch)

	const events = 10
	for i := 0; i < events; i++ {
		v := Counter{Value: uint64(i)}
		w.Notify("k", &v, false)
	}

	got := drain(ch)
	if len(got) != events {
		t.Fatalf("received %d events, want %d", len(got), events)
	}
	for i, ev := range got {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("event %d has Seq %d, want %d", i, ev.Seq, i+1)
		}
	}
}

// TestWatcher_SubscribeContextReleasesTheSubscription pins that a subscription
// tied to a context is cleaned up when that context ends, so a caller who
// forgets Unsubscribe does not leak the channel and its registry slot.
func TestWatcher_SubscribeContextReleasesTheSubscription(t *testing.T) {
	w := easyraft.NewWatcher[Counter]()
	ctx, cancel := context.WithCancel(context.Background())
	ch := w.SubscribeContext(ctx, "")

	v := Counter{Value: 1}
	w.Notify("k", &v, false)

	select {
	case ev := <-ch:
		if ev.Value == nil || ev.Value.Value != 1 {
			t.Fatalf("event = %+v, want value 1", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered to the context-bound subscriber")
	}

	cancel()

	// The channel is closed once the subscription is removed.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("cancelling the context did not close the subscription channel")
		}
	}
}

// TestWatcher_ServeSSEFuncAnchorsTheSnapshot pins that the initial snapshot and
// the live stream line up. The snapshot is read as part of subscribing, with
// dispatch held off, so a client can never be handed a change event for a key
// whose snapshot value is already newer.
func TestWatcher_ServeSSEFuncAnchorsTheSnapshot(t *testing.T) {
	w := easyraft.NewWatcher[Counter]()

	snapshotRunning := make(chan struct{})
	releaseSnapshot := make(chan struct{})

	sse := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.ServeSSEFunc(rw, r, "", func() (map[string]Counter, error) {
			close(snapshotRunning)
			<-releaseSnapshot
			return map[string]Counter{"before": {Value: 1}}, nil
		})
	}))
	defer sse.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sse.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := sse.Client().Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	select {
	case <-snapshotRunning:
	case <-time.After(5 * time.Second):
		t.Fatal("the snapshot function was never called")
	}

	// A write that lands while the snapshot is being taken must be held until
	// the subscription exists, so it is delivered rather than lost in the gap
	// between reading state and subscribing.
	notified := make(chan struct{})
	go func() {
		defer close(notified)
		v := Counter{Value: 2}
		w.Notify("after", &v, false)
	}()

	select {
	case <-notified:
		t.Fatal("a change was dispatched while the snapshot was being taken: the stream is not anchored to it")
	case <-time.After(200 * time.Millisecond):
	}
	close(releaseSnapshot)

	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch never resumed after the snapshot completed")
	}

	events := readSSEEvents(resp.Body)

	var snapshotSeq uint64
	select {
	case ev := <-events:
		if ev.Type != "snapshot" {
			t.Fatalf("first event type = %q, want snapshot", ev.Type)
		}
		if got := jsonString(ev.Data["key"]); got != "before" {
			t.Fatalf("snapshot key = %q, want \"before\"", got)
		}
		if err := json.Unmarshal(ev.Data["seq"], &snapshotSeq); err != nil {
			t.Fatalf("snapshot event carries no sequence number: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot event")
	}

	select {
	case ev := <-events:
		if ev.Type != "change" {
			t.Fatalf("event type = %q, want change", ev.Type)
		}
		if got := jsonString(ev.Data["key"]); got != "after" {
			t.Fatalf("event key = %q, want \"after\"", got)
		}
		var seq uint64
		if err := json.Unmarshal(ev.Data["seq"], &seq); err != nil {
			t.Fatalf("change event carries no sequence number: %v", err)
		}
		if seq <= snapshotSeq {
			t.Errorf("change event Seq %d is not after the snapshot anchor %d", seq, snapshotSeq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the change dispatched after the snapshot never reached the stream")
	}
}

// TestStore_SlowHandlerDoesNotStarveOtherCollections pins that each collection
// gets its own dispatcher. A handler that blocks must delay only the
// collection it was registered for; before, one slow handler stalled every
// watcher in the store.
func TestStore_SlowHandlerDoesNotStarveOtherCollections(t *testing.T) {
	store, err := easyraft.NewStore(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithLogger(quietLogger()),
	)
	if err != nil {
		t.Fatal(err)
	}

	slow := easyraft.AddCollection[Counter](store, "slow")
	fast := easyraft.AddCollection[Counter](store, "fast")

	blocked := make(chan struct{})
	slowEntered := make(chan string, 1)
	fastSaw := make(chan string, 8)

	slow.OnChange(func(key string, _ *Counter, _ bool) {
		select {
		case slowEntered <- key:
		default:
		}
		<-blocked // hold this collection's dispatcher
	})
	fast.OnChange(func(key string, _ *Counter, _ bool) {
		fastSaw <- key
	})

	store.Start()
	defer func() {
		close(blocked)
		store.Stop()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := store.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	if err := slow.Create(ctx, "s1", Counter{Value: 1}); err != nil {
		t.Fatalf("write to the slow collection: %v", err)
	}
	select {
	case <-slowEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow handler never ran")
	}

	// With the slow collection's dispatcher wedged, the fast collection must
	// still be delivered.
	if err := fast.Create(ctx, "f1", Counter{Value: 1}); err != nil {
		t.Fatalf("write to the fast collection: %v", err)
	}
	select {
	case key := <-fastSaw:
		if key != "f1" {
			t.Errorf("fast handler saw key %q, want f1", key)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked handler on one collection starved another collection's watcher")
	}

	// And it keeps up across further writes.
	for i := 0; i < 5; i++ {
		if err := slow.Create(ctx, "s"+string(rune('a'+i)), Counter{}); err != nil {
			t.Fatalf("further slow write: %v", err)
		}
		key := "f" + string(rune('a'+i))
		if err := fast.Create(ctx, key, Counter{}); err != nil {
			t.Fatalf("further fast write: %v", err)
		}
		select {
		case got := <-fastSaw:
			if got != key {
				t.Errorf("fast handler saw %q, want %q", got, key)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("fast collection stalled on iteration %d", i)
		}
	}
}

package easyraft

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// subscriberQueueDepth is how many events one subscriber may fall behind
// before it starts losing them — and is told so with a gap event.
const subscriberQueueDepth = 64

// ChangeEvent is delivered to subscribers when a collection entry is created,
// updated, upserted, mutated, or deleted.
//
// Seq increases by one for every event the producer dispatches, so a consumer
// that remembers the last Seq it saw can tell contiguous delivery from a jump.
//
// Gap marks a synthetic event reporting that one or more real events were
// dropped before it, because the consumer was not keeping up. Key and Value
// are empty on a gap event; the only correct response is to re-read the
// collection, since the events behind the gap are not recoverable.
type ChangeEvent[T any] struct {
	Seq     uint64 `json:"seq"`
	Key     string `json:"key"`
	Value   *T     `json:"value,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	Gap     bool   `json:"gap,omitempty"`
}

// subscription is one registered channel plus the bookkeeping needed to tell
// it that it missed something.
type subscription[T any] struct {
	key string
	ch  chan ChangeEvent[T]

	// gapOwed records that an event was dropped for this subscriber and it
	// still has to be told. Guarded by the Watcher's mutex.
	gapOwed bool
}

// Watcher manages channel-based subscriptions to a [Collection]'s change
// stream. Wire it up by passing [Watcher.Notify] to [Collection.OnChange], or
// [Watcher.NotifyEvent] to [Collection.OnChangeEvent] so that store-level gaps
// reach subscribers too:
//
//	w := easyraft.NewWatcher[MyType]()
//	collection.OnChangeEvent(w.NotifyEvent)
//	ch := w.Subscribe("")        // all keys
//	ch := w.Subscribe("somekey") // one key
//
// A subscriber that cannot keep up loses events, but never silently: the next
// event it does receive is preceded by one with Gap set. Sequence numbers are
// assigned by the Watcher and are contiguous across everything it dispatches.
//
// Every [Watcher.Subscribe] must be paired with [Watcher.Unsubscribe] or the
// channel and its registry entry are retained for the lifetime of the Watcher.
// [Watcher.SubscribeContext] ties that cleanup to a context instead.
//
// A Watcher is safe for concurrent use from multiple goroutines.
type Watcher[T any] struct {
	mu    sync.Mutex
	seq   uint64
	all   []*subscription[T]
	byKey map[string][]*subscription[T]
}

// NewWatcher returns an initialised, empty Watcher.
func NewWatcher[T any]() *Watcher[T] {
	return &Watcher[T]{byKey: make(map[string][]*subscription[T])}
}

// Subscribe returns a buffered channel that receives [ChangeEvent] values for
// the given key. Pass key="" to receive events for all keys. The channel is
// buffered (64 events); a subscriber that falls further behind than that loses
// events and is told so by a [ChangeEvent] with Gap set.
//
// Call [Watcher.Unsubscribe] when done — it is mandatory, not advisory: an
// abandoned subscription keeps its channel and registry slot forever, and
// every subsequent event pays the cost of trying to deliver to it. Use
// [Watcher.SubscribeContext] to have that handled automatically.
func (w *Watcher[T]) Subscribe(key string) chan ChangeEvent[T] {
	sub := &subscription[T]{key: key, ch: make(chan ChangeEvent[T], subscriberQueueDepth)}
	w.mu.Lock()
	w.register(sub)
	w.mu.Unlock()
	return sub.ch
}

// SubscribeContext is [Watcher.Subscribe] with the subscription's lifetime
// bound to ctx: when ctx is done the subscription is removed and the channel
// is closed, so a consumer can simply range over it.
//
//	for ev := range w.SubscribeContext(r.Context(), key) {
//	    ...
//	}
//
// The channel is closed only by cancellation, so a receive on a closed channel
// means the context ended, never that the Watcher went away.
func (w *Watcher[T]) SubscribeContext(ctx context.Context, key string) <-chan ChangeEvent[T] {
	sub := &subscription[T]{key: key, ch: make(chan ChangeEvent[T], subscriberQueueDepth)}
	w.mu.Lock()
	w.register(sub)
	w.mu.Unlock()

	go func() {
		<-ctx.Done()
		w.mu.Lock()
		w.unregister(sub)
		w.mu.Unlock()
		close(sub.ch)
	}()
	return sub.ch
}

// register adds sub to the registry. The caller must hold w.mu.
func (w *Watcher[T]) register(sub *subscription[T]) {
	if sub.key == "" {
		w.all = append(w.all, sub)
		return
	}
	w.byKey[sub.key] = append(w.byKey[sub.key], sub)
}

// unregister removes sub from the registry. The caller must hold w.mu.
func (w *Watcher[T]) unregister(sub *subscription[T]) {
	if sub.key == "" {
		w.all = removeSubscription(w.all, sub)
		return
	}
	remaining := removeSubscription(w.byKey[sub.key], sub)
	if len(remaining) == 0 {
		delete(w.byKey, sub.key)
		return
	}
	w.byKey[sub.key] = remaining
}

// Unsubscribe removes ch from the registry. key must match the value passed to
// [Watcher.Subscribe]. Unsubscribing a channel that is not registered is a
// no-op, so it is safe to call from a defer on any exit path.
func (w *Watcher[T]) Unsubscribe(key string, ch chan ChangeEvent[T]) {
	w.mu.Lock()
	defer w.mu.Unlock()

	list := w.all
	if key != "" {
		list = w.byKey[key]
	}
	for _, sub := range list {
		if sub.ch == ch {
			w.unregister(sub)
			return
		}
	}
}

// Notify dispatches a change event to all matching subscribers. Its signature
// matches the callback accepted by [Collection.OnChange], so it can be
// registered directly:
//
//	collection.OnChange(w.Notify)
//
// Prefer wiring [Watcher.NotifyEvent] to [Collection.OnChangeEvent]: that path
// also carries the store's own gap reports through to subscribers.
//
// Notify never blocks. A subscriber whose channel is full loses the event and
// receives a gap event once there is room again.
func (w *Watcher[T]) Notify(key string, value *T, deleted bool) {
	w.NotifyEvent(ChangeEvent[T]{Key: key, Value: value, Deleted: deleted})
}

// NotifyEvent dispatches ev to all matching subscribers. Its signature matches
// the callback accepted by [Collection.OnChangeEvent]:
//
//	collection.OnChangeEvent(w.NotifyEvent)
//
// The Seq on ev is replaced by this Watcher's own counter so that subscribers
// see one contiguous sequence; ev.Gap is preserved, so a gap reported by the
// store reaches every subscriber.
//
// NotifyEvent never blocks.
func (w *Watcher[T]) NotifyEvent(ev ChangeEvent[T]) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.seq++
	ev.Seq = w.seq

	for _, sub := range w.all {
		w.deliver(sub, ev)
	}
	if ev.Key != "" {
		for _, sub := range w.byKey[ev.Key] {
			w.deliver(sub, ev)
		}
	}
}

// deliver offers ev to sub without blocking, emitting an owed gap event first.
// The caller must hold w.mu.
func (w *Watcher[T]) deliver(sub *subscription[T], ev ChangeEvent[T]) {
	if sub.gapOwed {
		select {
		case sub.ch <- ChangeEvent[T]{Seq: ev.Seq, Gap: true}:
			sub.gapOwed = false
		default:
			return // still full; the gap stays owed
		}
	}
	select {
	case sub.ch <- ev:
	default:
		sub.gapOwed = true
	}
}

// ServeSSE writes a Server-Sent Events stream to rw until r.Context() is done.
// It first sends a "snapshot" event for each entry in snapshot (filtered to key
// if key is non-empty), then streams live [ChangeEvent] values as "change",
// "delete" or "gap" events. Pass key="" to stream all keys.
//
// The snapshot passed here was read before the subscription existed, so a
// change that lands in between is visible in the snapshot and again in the
// stream, and an event may describe a state the snapshot already includes.
// [Watcher.ServeSSEFunc] closes that window by reading the snapshot as part of
// subscribing; prefer it for new code.
//
// SSE event data is the JSON encoding of [ChangeEvent[T]]:
//
//	event: change
//	data: {"seq":7,"key":"k","value":{...}}
//
//	event: delete
//	data: {"seq":8,"key":"k","deleted":true}
//
//	event: gap
//	data: {"seq":9,"gap":true}
func (w *Watcher[T]) ServeSSE(rw http.ResponseWriter, r *http.Request, key string, snapshot map[string]T) {
	w.ServeSSEFunc(rw, r, key, func() (map[string]T, error) { return snapshot, nil })
}

// ServeSSEFunc is [Watcher.ServeSSE] with the initial snapshot read as part of
// subscribing rather than beforehand.
//
// snapshot is called while the Watcher is holding off dispatch, so the stream
// is anchored to it: every event the client receives describes a change that
// happened after the snapshot was taken, and no event can describe a state the
// snapshot already reflects. The response headers are sent first, so a slow
// snapshot delays the stream's contents rather than the client's connect. Pass
// the collection's own read:
//
//	w.ServeSSEFunc(rw, r, key, configs.ListStale)
//
// snapshot must not call back into this Watcher, and should be quick — it runs
// with dispatch held off.
func (w *Watcher[T]) ServeSSEFunc(rw http.ResponseWriter, r *http.Request, key string, snapshot func() (map[string]T, error)) {
	flusher, ok := rw.(http.Flusher)
	if !ok {
		http.Error(rw, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Send the headers before doing any work, so the client sees the stream
	// open immediately rather than waiting on the snapshot.
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("Connection", "keep-alive")
	rw.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	sub := &subscription[T]{key: key, ch: make(chan ChangeEvent[T], subscriberQueueDepth)}

	entries, anchor, err := w.subscribeWithSnapshot(sub, snapshot)
	if err != nil {
		// The response is already committed, so report the failure in-band and
		// close the stream; the client can reconnect.
		writeSSEEvent(rw, "error", struct {
			Error string `json:"error"`
		}{Error: "snapshot unavailable"})
		flusher.Flush()
		return
	}
	defer func() {
		w.mu.Lock()
		w.unregister(sub)
		w.mu.Unlock()
	}()

	for k, v := range entries {
		if key != "" && k != key {
			continue
		}
		v := v // capture loop variable
		writeSSEEvent(rw, "snapshot", ChangeEvent[T]{Seq: anchor, Key: k, Value: &v})
	}
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-sub.ch:
			if !open {
				return
			}
			writeSSEEvent(rw, sseEventType(ev), ev)
			flusher.Flush()
		}
	}
}

// subscribeWithSnapshot reads snapshot and registers sub under one lock, so
// the event stream picks up exactly where the snapshot left off. It returns
// the snapshot, the sequence number it is anchored to, and any error from the
// snapshot function — in which case sub is not registered.
func (w *Watcher[T]) subscribeWithSnapshot(sub *subscription[T], snapshot func() (map[string]T, error)) (map[string]T, uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	entries, err := snapshot()
	if err != nil {
		return nil, 0, err
	}
	w.register(sub)
	return entries, w.seq, nil
}

// sseEventType names the SSE event for ev.
func sseEventType[T any](ev ChangeEvent[T]) string {
	switch {
	case ev.Gap:
		return "gap"
	case ev.Deleted:
		return "delete"
	default:
		return "change"
	}
}

// MarshalJSON keeps a gap event's JSON shape minimal: it carries no key or
// value, so emitting them would suggest the event describes an entry.
func (e ChangeEvent[T]) MarshalJSON() ([]byte, error) {
	type alias ChangeEvent[T] // avoid recursing into this method
	if e.Gap {
		return json.Marshal(alias{Seq: e.Seq, Gap: true})
	}
	b, err := json.Marshal(alias(e))
	if err != nil {
		return nil, fmt.Errorf("easyraft: encode change event: %w", err)
	}
	return b, nil
}

func removeSubscription[T any](subs []*subscription[T], target *subscription[T]) []*subscription[T] {
	for i, sub := range subs {
		if sub == target {
			return append(subs[:i], subs[i+1:]...)
		}
	}
	return subs
}

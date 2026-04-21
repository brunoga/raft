package easyraft

import (
	"net/http"
	"sync"
)

// ChangeEvent is delivered to subscribers when a collection entry is created,
// updated, upserted, mutated, or deleted.
type ChangeEvent[T any] struct {
	Key     string `json:"key"`
	Value   *T     `json:"value,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

// Watcher manages channel-based subscriptions to a [Collection]'s change
// stream. Wire it up by passing [Watcher.Notify] to [Collection.OnChange]:
//
//	w := easyraft.NewWatcher[MyType]()
//	collection.OnChange(w.Notify)
//	ch := w.Subscribe("")        // all keys
//	ch := w.Subscribe("somekey") // one key
//
// Events that arrive while a subscriber's channel is full are silently dropped.
// A Watcher is safe for concurrent use from multiple goroutines.
type Watcher[T any] struct {
	mu    sync.Mutex
	all   []chan ChangeEvent[T]
	byKey map[string][]chan ChangeEvent[T]
}

// NewWatcher returns an initialised, empty Watcher.
func NewWatcher[T any]() *Watcher[T] {
	return &Watcher[T]{byKey: make(map[string][]chan ChangeEvent[T])}
}

// Subscribe returns a buffered channel that receives [ChangeEvent] values for
// the given key. Pass key="" to receive events for all keys. The channel is
// buffered (64 events); callers should drain it promptly to avoid drops.
// Call [Watcher.Unsubscribe] when done to release the channel.
func (w *Watcher[T]) Subscribe(key string) chan ChangeEvent[T] {
	ch := make(chan ChangeEvent[T], 64)
	w.mu.Lock()
	if key == "" {
		w.all = append(w.all, ch)
	} else {
		w.byKey[key] = append(w.byKey[key], ch)
	}
	w.mu.Unlock()
	return ch
}

// Unsubscribe removes ch from the registry. key must match the value passed to
// [Watcher.Subscribe].
func (w *Watcher[T]) Unsubscribe(key string, ch chan ChangeEvent[T]) {
	w.mu.Lock()
	if key == "" {
		w.all = removeWatchCh(w.all, ch)
	} else {
		w.byKey[key] = removeWatchCh(w.byKey[key], ch)
	}
	w.mu.Unlock()
}

// Notify dispatches a change event to all matching subscribers. Its signature
// matches the callback accepted by [Collection.OnChange], so it can be
// registered directly:
//
//	collection.OnChange(w.Notify)
//
// Notify must not block; full subscriber channels silently drop the event.
func (w *Watcher[T]) Notify(key string, value *T, deleted bool) {
	ev := ChangeEvent[T]{Key: key, Value: value, Deleted: deleted}
	w.mu.Lock()
	targets := append(append([]chan ChangeEvent[T](nil), w.all...), w.byKey[key]...)
	w.mu.Unlock()
	for _, ch := range targets {
		select {
		case ch <- ev:
		default:
		}
	}
}

// ServeSSE writes a Server-Sent Events stream to rw until r.Context() is done.
// It first sends a "snapshot" event for each entry in snapshot (filtered to key
// if key is non-empty), then streams live [ChangeEvent] values as "change" or
// "delete" events. Pass key="" to stream all keys.
//
// SSE event data is the JSON encoding of [ChangeEvent[T]]:
//
//	event: change
//	data: {"key":"k","value":{...}}
//
//	event: delete
//	data: {"key":"k","deleted":true}
func (w *Watcher[T]) ServeSSE(rw http.ResponseWriter, r *http.Request, key string, snapshot map[string]T) {
	flusher, ok := rw.(http.Flusher)
	if !ok {
		http.Error(rw, "streaming not supported", http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("Connection", "keep-alive")
	rw.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	ch := w.Subscribe(key)
	defer w.Unsubscribe(key, ch)

	for k, v := range snapshot {
		if key != "" && k != key {
			continue
		}
		v := v // capture loop variable
		writeSSEEvent(rw, "snapshot", ChangeEvent[T]{Key: k, Value: &v})
	}
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			eventType := "change"
			if ev.Deleted {
				eventType = "delete"
			}
			writeSSEEvent(rw, eventType, ev)
			flusher.Flush()
		}
	}
}

func removeWatchCh[T any](s []chan ChangeEvent[T], ch chan ChangeEvent[T]) []chan ChangeEvent[T] {
	for i, c := range s {
		if c == ch {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}

package config

import (
	"sync"
	"sync/atomic"
)

// LiveValue holds a config value that a live-apply applier can swap at runtime
// while consumers read it concurrently. It is the storage-plus-notification
// primitive that lets a config field become hot without every consumer
// hand-rolling its own thread-safe update path:
//
//   - A consumer that only needs the current value calls [LiveValue.Load] on
//     each use — wait-free, no locks.
//   - A consumer that must REACT to a change (reset a ticker, rebuild derived
//     state, re-auth a client) registers a callback with [LiveValue.OnChange].
//
// A live-apply applier, after reloading config, calls [LiveValue.Store] once;
// re-read consumers pick up the new value on their next Load, and reactive
// consumers' callbacks fire synchronously.
//
// The zero value is not usable; construct with [NewLiveValue].
type LiveValue[T any] struct {
	ptr  atomic.Pointer[T]
	mu   sync.Mutex        // guards subs
	subs []func(old, new T)
}

// NewLiveValue returns a LiveValue holding initial.
func NewLiveValue[T any](initial T) *LiveValue[T] {
	lv := &LiveValue[T]{}
	lv.ptr.Store(&initial)
	return lv
}

// Load returns the current value. Safe for concurrent use and wait-free.
func (lv *LiveValue[T]) Load() T {
	return *lv.ptr.Load()
}

// Store swaps in v and then invokes every OnChange callback with the (old, new)
// pair, in registration order, synchronously. Callbacks observe the new value
// via Load. A callback must not block or call Store on the same LiveValue (it
// would recurse). Store always notifies — it does not attempt to detect a
// no-op change (T need not be comparable); appliers Store only on an actual
// reload, so a spurious notification cannot arise in normal use.
func (lv *LiveValue[T]) Store(v T) {
	old := *lv.ptr.Load()
	lv.ptr.Store(&v)

	lv.mu.Lock()
	subs := make([]func(old, new T), len(lv.subs))
	copy(subs, lv.subs)
	lv.mu.Unlock()

	for _, fn := range subs {
		fn(old, v)
	}
}

// OnChange registers fn to run after each Store, receiving the previous and new
// values. Subscriptions live for the process lifetime — appliers and consumers
// wire themselves up once at startup, so there is no Unsubscribe.
func (lv *LiveValue[T]) OnChange(fn func(old, new T)) {
	lv.mu.Lock()
	lv.subs = append(lv.subs, fn)
	lv.mu.Unlock()
}

package timeutil

import (
	"sync/atomic"
	"time"
)

var lastUnique atomic.Int64

// UniqueNano returns the current wall-clock time in Unix nanoseconds, bumped
// to stay strictly greater than every earlier value this process returned.
//
// Use it where time.Now().UnixNano() was used as an identifier. That idiom is
// only unique when the clock has nanosecond resolution: on macOS the wall clock
// advances in whole microseconds, so two calls in the same microsecond return
// the same value (observed: duplicate ccstream control request IDs).
func UniqueNano() int64 {
	now := time.Now().UnixNano()
	for {
		last := lastUnique.Load()
		next := now
		if next <= last {
			next = last + 1
		}
		if lastUnique.CompareAndSwap(last, next) {
			return next
		}
	}
}

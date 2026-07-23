package platform

import (
	"sync"
	"time"

	"foci/internal/clock"
	"foci/internal/log"
)

// GroupThrottle buffers non-mention messages per chat ID with a fixed-window
// cooldown. Mentions flush all buffered messages immediately and reset the
// timer. Timer fires deliver all accumulated messages as a batch.
//
// All methods are safe for concurrent use. Nil receiver is safe (all methods no-op).
type GroupThrottle struct {
	window  time.Duration
	flushFn func([]QueuedMessage)
	log     *log.ComponentLogger
	clock   clock.Clock

	mu    sync.Mutex
	chats map[int64]*chatBucket
}

// chatBucket holds buffered messages and the pending timer for a single chat.
type chatBucket struct {
	msgs  []QueuedMessage
	timer clock.Timer
}

// NewGroupThrottle creates a throttle with the given window duration.
// flushFn is called (on a timer goroutine) with accumulated messages when the
// window expires or a mention forces an immediate flush.
func NewGroupThrottle(window time.Duration, flushFn func([]QueuedMessage), logger *log.ComponentLogger) *GroupThrottle {
	return NewGroupThrottleWithClock(window, flushFn, logger, clock.Real())
}

// NewGroupThrottleWithClock is NewGroupThrottle with an injectable time
// source, so tests can drive the fixed-window cooldown deterministically via
// a *clock.Fake instead of racing real sleeps against the window (#1513).
// Production callers should use NewGroupThrottle.
func NewGroupThrottleWithClock(window time.Duration, flushFn func([]QueuedMessage), logger *log.ComponentLogger, clk clock.Clock) *GroupThrottle {
	return &GroupThrottle{
		window:  window,
		flushFn: flushFn,
		log:     logger,
		clock:   clk,
		chats:   make(map[int64]*chatBucket),
	}
}

// Add buffers a message for the given chat. If the message is a mention,
// all buffered messages (including this one) are flushed immediately and
// the cooldown resets. Otherwise the message accumulates until the timer fires.
func (g *GroupThrottle) Add(msg QueuedMessage) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	chatID := msg.ChatID
	bucket, ok := g.chats[chatID]
	if !ok {
		bucket = &chatBucket{}
		g.chats[chatID] = bucket
	}
	bucket.msgs = append(bucket.msgs, msg)

	if msg.IsMention {
		// Mention: stop any pending timer and flush immediately.
		if bucket.timer != nil {
			bucket.timer.Stop()
			bucket.timer = nil
		}
		g.flushBucketLocked(chatID, bucket)
		return
	}

	// Non-mention: start a timer if one isn't already running (fixed window).
	if bucket.timer == nil {
		bucket.timer = g.clock.AfterFunc(g.window, func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			b, ok := g.chats[chatID]
			if !ok || len(b.msgs) == 0 {
				return
			}
			b.timer = nil
			g.flushBucketLocked(chatID, b)
		})
		if g.log != nil {
			g.log.Debugf("throttle: started %s timer for chat %d", g.window, chatID)
		}
	}
}

// flushBucketLocked delivers all buffered messages and clears the bucket.
// Caller must hold g.mu.
func (g *GroupThrottle) flushBucketLocked(chatID int64, bucket *chatBucket) {
	msgs := bucket.msgs
	bucket.msgs = nil
	if g.log != nil {
		g.log.Infof("throttle: flushing %d message(s) for chat %d", len(msgs), chatID)
	}
	// Release the lock before calling flushFn to avoid deadlocks with Enqueue.
	g.mu.Unlock()
	g.flushFn(msgs)
	g.mu.Lock()
}

// Stop cancels all pending timers. Buffered messages are discarded.
func (g *GroupThrottle) Stop() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, bucket := range g.chats {
		if bucket.timer != nil {
			bucket.timer.Stop()
			bucket.timer = nil
		}
	}
	g.chats = make(map[int64]*chatBucket)
}

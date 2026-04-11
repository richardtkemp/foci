package platform

import (
	"sync/atomic"
	"testing"
	"time"
)

// Tests that DM messages (non-group) bypass throttle and go straight to the
// channel regardless of requireMention or throttle settings.
func TestMessageQueue_DMBypassesThrottleAndMention(t *testing.T) {
	mq := NewMessageQueue(MessageQueueConfig{
		Size:           64,
		RequireMention: true,
		SteerMode:      false,
		Throttle:       nil,
		TurnActive:     func() bool { return false },
	})

	mq.Enqueue(QueuedMessage{
		UserID:      "u1",
		Text:        "hello",
		IsGroupChat: false,
		IsMention:   false,
	})

	select {
	case msg := <-mq.Chan():
		if msg.Text != "hello" {
			t.Fatalf("unexpected text: %s", msg.Text)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("DM message not delivered to channel")
	}
}

// Tests that group messages are dropped when requireMention=true, no throttle,
// and the message is not a mention.
func TestMessageQueue_GroupDropWithoutMention(t *testing.T) {
	mq := NewMessageQueue(MessageQueueConfig{
		Size:           64,
		RequireMention: true,
		Throttle:       nil,
		TurnActive:     func() bool { return false },
	})

	mq.Enqueue(QueuedMessage{
		UserID:      "u1",
		Text:        "ignored",
		IsGroupChat: true,
		IsMention:   false,
	})

	select {
	case <-mq.Chan():
		t.Fatal("non-mention group message should have been dropped")
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

// Tests that group mentions are delivered when requireMention=true and no throttle.
func TestMessageQueue_GroupMentionDelivered(t *testing.T) {
	mq := NewMessageQueue(MessageQueueConfig{
		Size:           64,
		RequireMention: true,
		Throttle:       nil,
		TurnActive:     func() bool { return false },
	})

	mq.Enqueue(QueuedMessage{
		UserID:      "u1",
		Text:        "hey bot!",
		IsGroupChat: true,
		IsMention:   true,
	})

	select {
	case msg := <-mq.Chan():
		if msg.Text != "hey bot!" {
			t.Fatalf("unexpected text: %s", msg.Text)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("mention group message not delivered")
	}
}

// Tests that group non-mention messages are routed to the throttle when
// throttle is configured, even with requireMention=true.
func TestMessageQueue_GroupThrottleBuffers(t *testing.T) {
	mq := NewMessageQueue(MessageQueueConfig{
		Size:           64,
		RequireMention: true,
		TurnActive:     func() bool { return false },
	})

	// Create throttle that flushes into the queue channel.
	gt := NewGroupThrottle(30*time.Millisecond, func(msgs []QueuedMessage) {
		for _, m := range msgs {
			mq.pushToChannel(m)
		}
	}, nil)
	defer gt.Stop()
	mq.throttle = gt

	mq.Enqueue(QueuedMessage{
		UserID:      "u1",
		Text:        "group msg",
		ChatID:      42,
		IsGroupChat: true,
		IsMention:   false,
	})

	// Should not be in channel yet (buffered in throttle).
	select {
	case <-mq.Chan():
		t.Fatal("message should be in throttle, not channel yet")
	case <-time.After(10 * time.Millisecond):
	}

	// Wait for throttle to fire.
	time.Sleep(50 * time.Millisecond)

	select {
	case msg := <-mq.Chan():
		if msg.Text != "group msg" {
			t.Fatalf("unexpected text: %s", msg.Text)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("throttle did not flush message to channel")
	}
}

// Tests that steer mode routes text-only messages to the steer buffer
// when a turn is active.
func TestMessageQueue_SteerMode(t *testing.T) {
	mq := NewMessageQueue(MessageQueueConfig{
		Size:       64,
		SteerMode:  true,
		TurnActive: func() bool { return true },
	})

	mq.Enqueue(QueuedMessage{
		UserID: "u1",
		Text:   "redirect this",
	})

	// Should be in steer buffer, not channel.
	select {
	case <-mq.Chan():
		t.Fatal("steered message should not be in channel")
	case <-time.After(10 * time.Millisecond):
	}

	parts := mq.DrainSteer()
	if len(parts) != 1 || parts[0].Text != "redirect this" {
		t.Fatalf("unexpected steer parts: %v", parts)
	}
}

// Tests that a message with attachments bypasses steer even when steer mode
// is active and a turn is in progress.
func TestMessageQueue_SteerSkipsAttachments(t *testing.T) {
	mq := NewMessageQueue(MessageQueueConfig{
		Size:       64,
		SteerMode:  true,
		TurnActive: func() bool { return true },
	})

	mq.Enqueue(QueuedMessage{
		UserID:      "u1",
		Text:        "has attachment",
		Attachments: []Attachment{{Data: []byte("img")}},
	})

	select {
	case msg := <-mq.Chan():
		if msg.Text != "has attachment" {
			t.Fatalf("unexpected text: %s", msg.Text)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("message with attachment should be in channel, not steered")
	}
}

// Tests DrainQueue returns all buffered messages non-blocking.
func TestMessageQueue_DrainQueue(t *testing.T) {
	mq := NewMessageQueue(MessageQueueConfig{
		Size:       64,
		TurnActive: func() bool { return false },
	})

	for i := 0; i < 5; i++ {
		mq.ch <- QueuedMessage{Text: "msg"}
	}

	drained := mq.DrainQueue()
	if len(drained) != 5 {
		t.Fatalf("expected 5 drained, got %d", len(drained))
	}

	// Second drain should return nil.
	drained2 := mq.DrainQueue()
	if len(drained2) != 0 {
		t.Fatalf("expected 0 on second drain, got %d", len(drained2))
	}
}

// Tests that messages are dropped (not blocked) when the queue is full.
func TestMessageQueue_FullQueueDrops(t *testing.T) {
	mq := NewMessageQueue(MessageQueueConfig{
		Size:       2,
		TurnActive: func() bool { return false },
	})

	// Fill the queue.
	mq.Enqueue(QueuedMessage{Text: "1"})
	mq.Enqueue(QueuedMessage{Text: "2"})
	// Third should be dropped, not block.
	mq.Enqueue(QueuedMessage{Text: "3-dropped"})

	drained := mq.DrainQueue()
	if len(drained) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(drained))
	}
}

// Tests that DrainSteer returns nil when no messages are buffered.
func TestMessageQueue_DrainSteerEmpty(t *testing.T) {
	mq := NewMessageQueue(MessageQueueConfig{Size: 8})
	if parts := mq.DrainSteer(); parts != nil {
		t.Fatalf("expected nil, got %v", parts)
	}
}

// Tests that group mention + steer mode + turn active routes to steer buffer
// (rule 2: urgent redirect).
func TestMessageQueue_GroupMentionSteer(t *testing.T) {
	var active atomic.Bool
	active.Store(true)

	mq := NewMessageQueue(MessageQueueConfig{
		Size:       64,
		SteerMode:  true,
		TurnActive: func() bool { return active.Load() },
	})

	mq.Enqueue(QueuedMessage{
		UserID:      "u1",
		Text:        "hey @bot do this",
		IsGroupChat: true,
		IsMention:   true,
	})

	parts := mq.DrainSteer()
	if len(parts) != 1 || parts[0].Text != "hey @bot do this" {
		t.Fatalf("expected mention to be steered, got: %v", parts)
	}
}

// Tests Stop cancels throttle timers.
func TestMessageQueue_Stop(t *testing.T) {
	var flushed int32
	gt := NewGroupThrottle(50*time.Millisecond, func(msgs []QueuedMessage) {
		atomic.AddInt32(&flushed, int32(len(msgs)))
	}, nil)

	mq := NewMessageQueue(MessageQueueConfig{
		Size:       64,
		Throttle:   gt,
		TurnActive: func() bool { return false },
	})

	mq.Enqueue(QueuedMessage{ChatID: 1, Text: "buffered", IsGroupChat: true})
	mq.Stop()

	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt32(&flushed) != 0 {
		t.Fatal("throttle should not have flushed after Stop")
	}
}

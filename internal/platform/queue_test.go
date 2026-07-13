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
		Throttle:       nil,
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
	})

	// Create throttle that flushes into the queue channel.
	gt := NewGroupThrottle(30*time.Millisecond, func(msgs []QueuedMessage) {
		for _, m := range msgs {
			mq.pushToChannel(m)
		}
	}, nil)
	defer gt.Stop()
	mq.throttle.Store(gt)

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

// Tests DrainQueue returns all buffered messages non-blocking.
func TestMessageQueue_DrainQueue(t *testing.T) {
	mq := NewMessageQueue(MessageQueueConfig{
		Size: 64,
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
		Size: 2,
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

// Tests Stop cancels throttle timers.
func TestMessageQueue_Stop(t *testing.T) {
	var flushed int32
	gt := NewGroupThrottle(50*time.Millisecond, func(msgs []QueuedMessage) {
		atomic.AddInt32(&flushed, int32(len(msgs)))
	}, nil)

	mq := NewMessageQueue(MessageQueueConfig{
		Size:     64,
		Throttle: gt,
	})

	mq.Enqueue(QueuedMessage{ChatID: 1, Text: "buffered", IsGroupChat: true})
	mq.Stop()

	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt32(&flushed) != 0 {
		t.Fatal("throttle should not have flushed after Stop")
	}
}

// Tests that EnqueueCommand routes to the cmd channel separately from
// the main channel — commands bypass filter rules entirely.
func TestMessageQueue_EnqueueCommand(t *testing.T) {
	mq := NewMessageQueue(MessageQueueConfig{
		Size:           64,
		RequireMention: true, // would otherwise drop a non-mention group message
	})

	mq.EnqueueCommand(QueuedMessage{Text: "/status", IsGroupChat: true})

	select {
	case msg := <-mq.CmdChan():
		if msg.Text != "/status" {
			t.Fatalf("unexpected: %s", msg.Text)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("command not delivered to cmd channel")
	}
}

// TestMessageQueue_SetThrottleIsLiveNotBaked proves SetThrottle/GetThrottle
// swap the throttle a running MessageQueue uses on its very next Enqueue —
// the mechanism a live config edit to behavior.group_throttle relies on —
// without reconstructing the queue.
func TestMessageQueue_SetThrottleIsLiveNotBaked(t *testing.T) {
	mq := NewMessageQueue(MessageQueueConfig{Size: 64, RequireMention: true})
	if mq.GetThrottle() != nil {
		t.Fatal("expected no throttle initially")
	}

	// No throttle configured yet: a non-mention group message is dropped.
	mq.Enqueue(QueuedMessage{Text: "buffered?", ChatID: 1, IsGroupChat: true, IsMention: false})
	select {
	case msg := <-mq.Chan():
		t.Fatalf("unexpected message before throttle configured: %v", msg)
	case <-time.After(20 * time.Millisecond):
	}

	gt := NewGroupThrottle(10*time.Millisecond, func(msgs []QueuedMessage) {
		for _, m := range msgs {
			mq.pushToChannel(m)
		}
	}, nil)
	defer gt.Stop()
	mq.SetThrottle(gt)
	if mq.GetThrottle() != gt {
		t.Fatal("GetThrottle did not return the throttle just set")
	}

	mq.Enqueue(QueuedMessage{Text: "now buffered", ChatID: 1, IsGroupChat: true, IsMention: false})
	select {
	case msg := <-mq.Chan():
		if msg.Text != "now buffered" {
			t.Fatalf("unexpected text: %s", msg.Text)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("message never flushed through the newly set throttle")
	}
}

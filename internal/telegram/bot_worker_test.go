package telegram

import (
	"context"
	"sync"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/warnings"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// mockHandler implements platform.MessageHandler for worker tests.
type mockHandler struct {
	mu        sync.Mutex
	calls     [][]string        // texts batches received by HandleMessageWithAttachments
	onCall    func(texts []string) // optional hook called inside HandleMessageWithAttachments
}

func (h *mockHandler) HandleMessage(_ context.Context, _ string, text string) (string, error) {
	return h.HandleMessageWithAttachments(context.Background(), "", []string{text}, nil)
}

func (h *mockHandler) HandleMessageWithAttachments(_ context.Context, _ string, texts []string, _ []platform.Attachment) (string, error) {
	h.mu.Lock()
	h.calls = append(h.calls, texts)
	fn := h.onCall
	h.mu.Unlock()
	if fn != nil {
		fn(texts)
	}
	return "ok", nil
}

func (h *mockHandler) IsProcessing() bool               { return false }
func (h *mockHandler) TransformMessage(t string) string  { return t }
func (h *mockHandler) Warnings() *warnings.Queue         { return nil }

// allCalls returns a copy of all recorded call batches.
func (h *mockHandler) allCalls() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([][]string, len(h.calls))
	for i, c := range h.calls {
		cp := make([]string, len(c))
		copy(cp, c)
		out[i] = cp
	}
	return out
}

// totalCalls returns the total number of calls received.
func (h *mockHandler) totalCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func TestAgentWorker_OrphanDrainIsRecursive(t *testing.T) {
	// Proves that steers arriving during orphan processing are themselves
	// drained and processed, rather than sitting stale in the buffer until
	// the next turn's tool execution. Before the fix, only the first orphan
	// was drained; the second remained in the buffer indefinitely.
	handler := &mockHandler{}

	b := &Bot{
		log:             log.NewComponentLogger("telegram:test"),
		client:          &mockClient{},
		handler:         handler,
		commands:        command.NewRegistry(),
		lastMsgStore:    command.NewLastMessageStore(),
		allowedUsers:    map[string]bool{"111": true},
		sessionKey:      "agent:test:main",
		queue:           make(chan queuedMessage, 64),
		chatSessionKeys: make(map[int64]string),
		display:         BotDisplayConfig{SteerMode: true},
	}

	// Wire up onCall now that b exists: inject orphan-2 during orphan-1 processing.
	var once sync.Once
	handler.onCall = func(texts []string) {
		if len(texts) > 0 && texts[0] == "orphan-1" {
			once.Do(func() {
				// Simulate a message arriving during orphan-1 processing.
				// In production, the receiver goroutine calls appendSteer.
				b.appendSteer("orphan-2")
			})
		}
	}

	// Pre-load the steer buffer with orphan-1 (simulates a message that
	// arrived during the queued turn but wasn't consumed by tool execution).
	b.appendSteer("orphan-1")

	msg := &gotgbot.Message{
		From: &gotgbot.User{Id: 111, Username: "testuser"},
		Chat: gotgbot.Chat{Id: 12345},
		Text: "queued-msg",
		Date: int64(time.Now().Unix()),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.agentWorker(ctx)
	}()

	b.queue <- queuedMessage{msg: msg, userID: "111", text: "queued-msg"}

	// Wait for all three handler calls.
	deadline := time.After(5 * time.Second)
	for {
		if handler.totalCalls() >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for handler calls; got %v", handler.allCalls())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()
	<-done

	got := handler.allCalls()
	// Expect 3 separate calls: queued-msg, orphan-1, orphan-2
	if len(got) != 3 {
		t.Fatalf("handler calls = %d, want 3; calls: %v", len(got), got)
	}
	want := []string{"queued-msg", "orphan-1", "orphan-2"}
	for i, w := range want {
		if len(got[i]) != 1 || got[i][0] != w {
			t.Errorf("call[%d] = %v, want [%q]", i, got[i], w)
		}
	}
}

func TestAgentWorker_QueueBatching(t *testing.T) {
	// Proves that multiple messages queued while the agent is busy are
	// batched into a single HandleMessageWithAttachments call with
	// multiple texts, rather than processed as separate turns.
	handler := &mockHandler{}

	// Block the first call so we can queue messages while busy.
	firstCallDone := make(chan struct{})
	handler.onCall = func(texts []string) {
		if len(texts) > 0 && texts[0] == "first" {
			<-firstCallDone // block until we've queued extra messages
		}
	}

	b := &Bot{
		log:             log.NewComponentLogger("telegram:test"),
		client:          &mockClient{},
		handler:         handler,
		commands:        command.NewRegistry(),
		lastMsgStore:    command.NewLastMessageStore(),
		allowedUsers:    map[string]bool{"111": true},
		sessionKey:      "agent:test:main",
		queue:           make(chan queuedMessage, 64),
		chatSessionKeys: make(map[int64]string),
	}

	msg := &gotgbot.Message{
		From: &gotgbot.User{Id: 111, Username: "testuser"},
		Chat: gotgbot.Chat{Id: 12345},
		Text: "first",
		Date: int64(time.Now().Unix()),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.agentWorker(ctx)
	}()

	// Send first message — worker picks it up and blocks.
	b.queue <- queuedMessage{msg: msg, userID: "111", text: "first"}
	// Give the worker time to pick up the first message.
	time.Sleep(50 * time.Millisecond)

	// Queue two more messages while the first is being processed.
	b.queue <- queuedMessage{msg: msg, userID: "111", text: "second"}
	b.queue <- queuedMessage{msg: msg, userID: "111", text: "third"}

	// Unblock the first call — worker will pick up "second" and "third"
	// from the queue and batch them.
	close(firstCallDone)

	// Wait for the second call (the batch).
	deadline := time.After(5 * time.Second)
	for {
		if handler.totalCalls() >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out; calls: %v", handler.allCalls())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()
	<-done

	got := handler.allCalls()
	if len(got) != 2 {
		t.Fatalf("expected 2 handler calls, got %d: %v", len(got), got)
	}

	// First call: single text "first"
	if len(got[0]) != 1 || got[0][0] != "first" {
		t.Errorf("call[0] = %v, want [first]", got[0])
	}

	// Second call: batched "second" and "third"
	if len(got[1]) != 2 || got[1][0] != "second" || got[1][1] != "third" {
		t.Errorf("call[1] = %v, want [second third]", got[1])
	}
}

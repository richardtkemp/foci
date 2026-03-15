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
	mu       sync.Mutex
	calls    []string // texts received by HandleMessage
	onCall   func(text string) // optional hook called inside HandleMessage
}

func (h *mockHandler) HandleMessage(_ context.Context, _ string, text string) (string, error) {
	h.mu.Lock()
	h.calls = append(h.calls, text)
	fn := h.onCall
	h.mu.Unlock()
	if fn != nil {
		fn(text)
	}
	return "ok", nil
}

func (h *mockHandler) HandleMessageWithAttachments(_ context.Context, _ string, text string, _ []platform.Attachment) (string, error) {
	return h.HandleMessage(context.Background(), "", text)
}

func (h *mockHandler) IsProcessing() bool          { return false }
func (h *mockHandler) TransformMessage(t string) string { return t }
func (h *mockHandler) Warnings() *warnings.Queue    { return nil }

func (h *mockHandler) texts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.calls))
	copy(out, h.calls)
	return out
}

func TestAgentWorker_OrphanDrainIsRecursive(t *testing.T) {
	// Proves that steers arriving during orphan processing are themselves
	// drained and processed, rather than sitting stale in the buffer until
	// the next turn's tool execution. Before the fix, only the first orphan
	// was drained; the second remained in the buffer indefinitely.
	handler := &mockHandler{}

	// We use onCall to inject a steer into the buffer while the orphan
	// turn is being processed, simulating a user message arriving mid-turn.
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
	handler.onCall = func(text string) {
		if text == "orphan-1" {
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

	// Run agentWorker in a goroutine; send one queued message to trigger it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.agentWorker(ctx)
	}()

	b.queue <- queuedMessage{msg: msg, userID: "111", text: "queued-msg"}

	// Wait for all three messages to be processed.
	deadline := time.After(5 * time.Second)
	for {
		if len(handler.texts()) >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for handler calls; got %v", handler.texts())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()
	<-done

	got := handler.texts()
	want := []string{"queued-msg", "orphan-1", "orphan-2"}
	if len(got) != len(want) {
		t.Fatalf("handler calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

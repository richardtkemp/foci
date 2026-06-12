package telegram

import (
	"context"
	"sync"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/platform"
	"foci/internal/warnings"
)

// mockHandler implements platform.MessageHandler for worker tests.
type mockHandler struct {
	mu     sync.Mutex
	calls  [][]string           // texts batches received by HandleMessage
	onCall func(texts []string) // optional hook called inside HandleMessage
}

func (h *mockHandler) HandleMessage(_ context.Context, _ string, texts []string, _ []platform.Attachment) error {
	h.mu.Lock()
	h.calls = append(h.calls, texts)
	fn := h.onCall
	h.mu.Unlock()
	if fn != nil {
		fn(texts)
	}
	return nil
}

func (h *mockHandler) IsProcessing() bool               { return false }
func (h *mockHandler) TransformMessage(t string) string { return t }
func (h *mockHandler) Warnings() *warnings.Queue        { return nil }

// allCalls returns a copy of all recorded call batches. Retained for
// potential future tests; currently unused.
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

// TestCommandWorker_DispatchesQueuedCommands verifies the command worker
// goroutine pulls from mq.CmdChan and dispatches via the bot's command
// dispatcher. Commands flow on their own channel independent of the
// agent message pump — this is intentionally bot-side because slash
// commands are platform-UI concerns, not agent message flow.
func TestCommandWorker_DispatchesQueuedCommands(t *testing.T) {
	cmdRan := make(chan struct{}, 1)
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name:        "ping",
		Description: "test",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			select {
			case cmdRan <- struct{}{}:
			default:
			}
			return command.Response{Text: "pong"}, nil
		},
	})

	b, _ := testBot([]string{"111"}, cmds)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmdDone := make(chan struct{})
	go func() { defer close(cmdDone); b.commandWorker(ctx) }()

	pingMsg := makeMsg(111, "owner", "/ping")
	b.mq.EnqueueCommand(platform.QueuedMessage{
		Original: pingMsg, UserID: "111", Text: "/ping", ChatID: 12345,
	})

	select {
	case <-cmdRan:
		// Good — command was dispatched.
	case <-time.After(2 * time.Second):
		t.Fatal("command did not run within 2s")
	}

	cancel()
	<-cmdDone
}

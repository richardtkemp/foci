package telegram

import (
	"context"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/agent/turnevent"
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

func (h *mockHandler) IsProcessing() bool                { return false }
func (h *mockHandler) TransformMessage(t string) string  { return t }
func (h *mockHandler) Warnings() *warnings.Queue         { return nil }

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

// recordingDriver captures Drive calls for assertions about envelope
// translation. Used in place of the real Bot.Drive (which needs a full
// renderer/sink wiring) when we want to inspect what reached the agent's
// per-session worker.
type recordingDriver struct {
	mu    sync.Mutex
	calls [][]agent.Envelope
}

func (d *recordingDriver) Drive(_ context.Context, _ string, batch []agent.Envelope, _ turnevent.Steerer) error {
	d.mu.Lock()
	d.calls = append(d.calls, batch)
	d.mu.Unlock()
	return nil
}

func (d *recordingDriver) numCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

func (d *recordingDriver) firstBatch() []agent.Envelope {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.calls) == 0 {
		return nil
	}
	return d.calls[0]
}

// TestAgentEnqueue_RoutesThroughInboxToDriver is a smoke test for the
// agent.Inbox path that Bot.handoffToAgent ultimately drives. It exercises
// agent.Enqueue directly with a recording Driver to verify the envelope
// reaches the per-session worker. The pump goroutine itself is a 4-line
// pipe (mq.Chan → handoffToAgent → agent.Enqueue) — the only meaningful
// step is the envelope construction, covered separately.
func TestAgentEnqueue_RoutesThroughInboxToDriver(t *testing.T) {
	a := &agent.Agent{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartInbox(ctx)

	d := &recordingDriver{}
	a.Enqueue(agent.Envelope{
		SessionKey: "agent:test:main",
		Text:       "hello",
		ChatID:     12345,
		UserID:     "111",
		Driver:     d,
	})

	deadline := time.After(2 * time.Second)
	for d.numCalls() < 1 {
		select {
		case <-deadline:
			t.Fatalf("driver not called; calls=%d", d.numCalls())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	batch := d.firstBatch()
	if len(batch) != 1 || batch[0].Text != "hello" || batch[0].SessionKey != "agent:test:main" {
		t.Errorf("unexpected batch: %+v", batch)
	}
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


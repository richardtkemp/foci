package discord

import (
	"context"
	"errors"
	"testing"

	"foci/internal/platform"
)

// TestWrapTurnLifecycle verifies WrapTurn sets the turn-active flag for the
// duration of the turn and drains notifications buffered during the turn. The
// session/keepalive hooks moved to Agent.HandleMessage — no longer WrapTurn's.
func TestWrapTurnLifecycle(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.SetChatID(42)

	err := b.WrapTurn(context.Background(), func() error {
		if !b.turnActive.Load() {
			t.Error("expected turnActive during turn")
		}
		b.SendNotification("buffered during turn")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.turnActive.Load() {
		t.Error("expected turnActive cleared after turn")
	}
	if got := fs.lastSend(t); got.content != "buffered during turn" {
		t.Errorf("expected buffered notification drained, got %q", got.content)
	}
}

// TestWrapTurnErrorPassthrough verifies turn errors are returned and the
// turn-active flag is still cleared.
func TestWrapTurnErrorPassthrough(t *testing.T) {
	b, _, _ := newTestBot(t, "a")

	wantErr := errors.New("turn failed")
	if err := b.WrapTurn(context.Background(), func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Errorf("expected error passthrough, got %v", err)
	}
	if b.turnActive.Load() {
		t.Error("expected turnActive cleared after error")
	}
}

// TestSessionKeyForQueuedMessage verifies session resolution: secondary bots
// use the override key, primary bots derive per-chat keys, and agent-less bots
// fall back to (empty) SessionKey.
func TestSessionKeyForQueuedMessage(t *testing.T) {
	sec, _, _ := newTestBot(t, "")
	sec.isSecondary = true
	sec.SetSessionKeyDirect("a/c9")
	if got := sec.sessionKeyForQueuedMessage(platform.QueuedMessage{ChatID: 42}); got != "a/c9" {
		t.Errorf("secondary: got %q", got)
	}

	prim, _, _ := newTestBot(t, "a")
	if got := prim.sessionKeyForQueuedMessage(platform.QueuedMessage{ChatID: 42}); got != prim.SessionKeyForChat(42) {
		t.Errorf("primary: got %q", got)
	}

	bare := &Bot{}
	if got := bare.sessionKeyForQueuedMessage(platform.QueuedMessage{ChatID: 42}); got != "" {
		t.Errorf("bare: got %q", got)
	}
}

// TestHandoffToAgentNoRef verifies messages are dropped without panic when no
// agent reference is wired (test mode).
func TestHandoffToAgentNoRef(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	b.handoffToAgent(platform.QueuedMessage{ChatID: 42, Text: "hi"}) // must not panic
}

// TestProcessQueuedCommandGuards verifies missing original message and nil
// dispatcher are handled without dispatching or panicking.
func TestProcessQueuedCommandGuards(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")

	// Missing original.
	b.processQueuedCommand(context.Background(), platform.QueuedMessage{Text: "/x"})

	// Original present but no dispatcher.
	msg := testDiscordMessage("42", "u1", "/x")
	b.processQueuedCommand(context.Background(), platform.QueuedMessage{Text: "/x", Original: msg})

	if fs.sendCount() != 0 {
		t.Errorf("expected no sends, got %d", fs.sendCount())
	}
}

// TestProcessQueuedCommandDispatches verifies a registered command routed via
// the command channel executes and its response is rendered as a reply.
func TestProcessQueuedCommandDispatches(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	registerTestCommand(b, "ver", "v1.2.3")
	b.SetCommandContext(commandTestContext())

	msg := testDiscordMessage("42", "u1", "/ver")
	b.processQueuedCommand(context.Background(), platform.QueuedMessage{
		Text: "/ver", ChatID: 42, UserID: "u1", Original: msg,
	})

	got := fs.lastSend(t)
	if got.channelID != "42" || got.content != "v1.2.3" {
		t.Errorf("expected version reply on channel 42, got %+v", got)
	}
}

// TestNewTurnSink verifies sink construction: nil for non-discord envelopes,
// and a live sink plus cleanup func for discord-origin envelopes.
func TestNewTurnSink(t *testing.T) {
	b, _, _ := newTestBot(t, "a")

	sink, cleanup := b.NewTurnSink(agentEnvelope("not-a-discord-message", 42))
	if sink != nil || cleanup != nil {
		t.Error("expected nil sink for non-discord original")
	}

	env := agentEnvelope(testDiscordMessage("42", "u1", "hi"), 42)
	sink, cleanup = b.NewTurnSink(env)
	if sink == nil || cleanup == nil {
		t.Fatal("expected sink and cleanup for discord envelope")
	}
	cleanup()
}

// TestConnectionReturnsSelf verifies the agent.Driver Connection accessor.
func TestConnectionReturnsSelf(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	if b.Connection() != platform.Connection(b) {
		t.Error("expected Connection() to return the bot itself")
	}
}

// TestCancelTurnNoAgent verifies cancelTurn is a safe no-op without an agent
// reference.
func TestCancelTurnNoAgent(t *testing.T) {
	b, _, _ := newTestBot(t, "a")
	b.cancelTurn() // must not panic
}

// TestAgentMessagePump verifies the pump dispatches queued commands and exits
// when the context is cancelled.
func TestAgentMessagePump(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	registerTestCommand(b, "ver", "pumped")
	b.SetCommandContext(commandTestContext())

	msg := testDiscordMessage("42", "u1", "/ver")
	b.mq.EnqueueCommand(platform.QueuedMessage{Text: "/ver", ChatID: 42, UserID: "u1", Original: msg})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.agentMessagePump(ctx)
		close(done)
	}()

	// The pump drains the command before blocking; poll the fake for the reply.
	waitFor(t, func() bool { return fs.sendCount() == 1 })
	if got := fs.lastSend(t); got.content != "pumped" {
		t.Errorf("expected command reply, got %q", got.content)
	}

	cancel()
	<-done // hangs (test timeout) if the pump ignores cancellation
}

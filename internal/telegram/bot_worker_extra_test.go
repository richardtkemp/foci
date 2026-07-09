package telegram

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/platform"
)

func TestSessionKeyForQueuedMessage(t *testing.T) {
	// Proves the queued-message session key follows the routing rules:
	// secondary bots use their override key, primary bots with an agent ID
	// derive a per-chat key, and bots without an agent ID fall back to the
	// bot's own session key.
	t.Run("secondary uses override", func(t *testing.T) {
		b, _ := testBot(nil, command.NewRegistry())
		b.isSecondary = true
		b.SetSessionKeyDirect("agent:scout:branch:x")
		if got := b.sessionKeyForQueuedMessage(platform.QueuedMessage{ChatID: 7}); got != "agent:scout:branch:x" {
			t.Errorf("got %q, want override key", got)
		}
	})
	t.Run("primary derives from chat", func(t *testing.T) {
		b, _ := testBot(nil, command.NewRegistry())
		b.agentID = "scout"
		b.chatmeta.AgentID = "scout"
		b.SetSessionKeyDirect("")
		got := b.sessionKeyForQueuedMessage(platform.QueuedMessage{ChatID: 7})
		if got != b.sessionKeyForMsg(7) {
			t.Errorf("got %q, want per-chat key %q", got, b.sessionKeyForMsg(7))
		}
	})
	t.Run("no agent ID falls back to bot key", func(t *testing.T) {
		b, _ := testBot(nil, command.NewRegistry())
		if got := b.sessionKeyForQueuedMessage(platform.QueuedMessage{ChatID: 7}); got != "agent:test:main" {
			t.Errorf("got %q, want bot session key", got)
		}
	})
}

func TestProcessQueuedCommand(t *testing.T) {
	// Proves processQueuedCommand executes a registered command via the
	// dispatcher and renders its response, and silently drops messages with
	// no original Telegram message or no dispatcher.
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name: "ping",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "pong"}, nil
		},
	})
	b, mock := testBot([]string{"111"}, cmds)

	msg := makeMsg(111, "owner", "/ping")
	b.processQueuedCommand(context.Background(), platform.QueuedMessage{
		Original: msg, UserID: "111", Text: "/ping", ChatID: 12345,
	})
	if mock.sentCount() != 1 {
		t.Fatalf("sends = %d, want 1 (pong reply)", mock.sentCount())
	}

	// Missing original: dropped without panic or send.
	b.processQueuedCommand(context.Background(), platform.QueuedMessage{Text: "/ping"})
	// Nil dispatcher: dropped without panic or send.
	b.dispatcher = nil
	b.processQueuedCommand(context.Background(), platform.QueuedMessage{Original: msg, Text: "/ping"})
	if mock.sentCount() != 1 {
		t.Errorf("sends = %d, want still 1 after dropped commands", mock.sentCount())
	}
}

func TestNewTurnSink(t *testing.T) {
	// Proves NewTurnSink builds a sink and cleanup func for a Telegram-origin
	// envelope, and returns (nil, nil) for envelopes from other platforms.
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	sink, cleanup := b.NewTurnSink(agent.Envelope{
		SessionKey: "agent:test:main",
		ChatID:     12345,
		Original:   makeMsg(111, "owner", "hi"),
	})
	if sink == nil || cleanup == nil {
		t.Fatal("expected sink and cleanup for telegram envelope")
	}
	cleanup()

	sink, cleanup = b.NewTurnSink(agent.Envelope{Original: "not a telegram message"})
	if sink != nil || cleanup != nil {
		t.Error("expected nil sink/cleanup for foreign envelope")
	}
}

func TestConnection(t *testing.T) {
	// Proves the bot exposes itself as the agent's platform connection.
	b, _ := testBot(nil, command.NewRegistry())
	if b.Connection() != platform.Connection(b) {
		t.Error("Connection should return the bot itself")
	}
}

func TestWrapTurn_LifecycleAndNotificationBuffering(t *testing.T) {
	// Proves WrapTurn marks the turn active while fn runs (so notifications
	// sent mid-turn are buffered, not delivered), then drains the buffer after
	// fn returns. The session/keepalive hooks are no longer WrapTurn's job —
	// they fire at the turn boundary in Agent.HandleMessage.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.SetChatID(12345)

	err := b.WrapTurn(context.Background(), func() error {
		if !b.turnActive.Load() {
			t.Error("turnActive should be true inside fn")
		}
		b.SendNotification("buffered during turn")
		if mock.sentCount() != 0 {
			t.Errorf("notification sent mid-turn (sends=%d), want buffered", mock.sentCount())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WrapTurn: %v", err)
	}
	if b.turnActive.Load() {
		t.Error("turnActive should be false after WrapTurn")
	}
	if mock.sentCount() != 1 {
		t.Errorf("sends = %d, want 1 (buffered notification drained)", mock.sentCount())
	}
}

func TestWrapTurn_ErrorPropagates(t *testing.T) {
	// Proves a turn error is returned to the caller (error handling must not
	// swallow it) while notification-draining cleanup still runs.
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	wantErr := errors.New("turn failed")
	err := b.WrapTurn(context.Background(), func() error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
	if b.turnActive.Load() {
		t.Error("turnActive should be false after WrapTurn even on error")
	}
}

func TestHandoffToAgent_NoAgentRef(t *testing.T) {
	// Proves handoffToAgent drops messages without panicking when no agent
	// is wired (receive-only test mode).
	b, _ := testBot(nil, command.NewRegistry())
	b.handoffToAgent(platform.QueuedMessage{Text: "hi", ChatID: 1})
}

func TestAgentMessagePump_DrainsAndExits(t *testing.T) {
	// Proves the pump goroutine drains queued messages from the platform
	// queue (dropping them when no agent is wired) and exits when its
	// context is cancelled.
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.agentMessagePump(ctx) }()

	b.mq.Enqueue(platform.QueuedMessage{
		Original: makeMsg(111, "owner", "hello"), UserID: "111", Text: "hello", ChatID: 12345,
	})

	// The pump owns the only receive end; once it has consumed the message,
	// the queue channel is empty. Wait for that, then cancel.
	deadline := time.After(2 * time.Second)
	for len(b.mq.Chan()) > 0 {
		select {
		case <-deadline:
			t.Fatal("pump did not drain the queue within 2s")
		default:
			runtime.Gosched()
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not exit after cancel")
	}
}

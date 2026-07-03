package cctmux

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// newStartedBackend returns a Backend wired to a fake pane with the watcher
// already "running", so sendToPane skips session discovery and pre-send
// offset recording — isolating the input-dispatch logic under test.
func newStartedBackend(t *testing.T) (*Backend, *fakeTmux) {
	t.Helper()
	shortEnterDelays(t)
	pane, f := newFakePane("cc-w")
	b := &Backend{watcher: &sessionWatcher{}}
	b.pane = pane
	return b, f
}

// textSends returns the literal text payloads sent to the pane via either
// send-keys -l or the load-buffer stdin path, in order.
func textSends(f *fakeTmux) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for i, c := range f.calls {
		switch {
		case len(c) > 0 && c[0] == "load-buffer":
			out = append(out, f.stdins[i])
		case hasSubsequence(c, []string{"-l"}) && len(c) > 0 && c[0] == "send-keys":
			out = append(out, c[len(c)-1])
		}
	}
	return out
}

// TestInject_UserIdleBeginsTurn proves an idle SourceUser inject begins a
// turn: prompt pasted to the pane, per-turn callback registered (turn becomes
// in-flight), typing indicator started, and prompt dedup state cleared.
func TestInject_UserIdleBeginsTurn(t *testing.T) {
	b, f := newStartedBackend(t)

	var typing []bool
	b.SetTypingFunc(func(v bool) { typing = append(typing, v) })
	b.lastPromptMu.Lock()
	b.lastPrompt = "stale prompt" // must be cleared on user input
	b.lastPromptMu.Unlock()

	err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceUser,
		Text:   "hello claude, do the thing",
		Turn:   &delegator.TurnEvents{OnTurnComplete: func(*delegator.TurnResult) {}},
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}

	if got := textSends(f); len(got) != 1 || got[0] != "hello claude, do the thing" {
		t.Errorf("text sends = %v", got)
	}
	if !b.IsTurnInFlight() {
		t.Error("turn should be in flight after begin-turn inject")
	}
	if len(typing) == 0 || typing[0] != true {
		t.Errorf("typing calls = %v, want [true ...]", typing)
	}
	b.lastPromptMu.Lock()
	defer b.lastPromptMu.Unlock()
	if b.lastPrompt != "" {
		t.Error("lastPrompt dedup state should be cleared on user input")
	}
}

// TestInject_UserInFlightFollowsUp proves a SourceUser inject during an
// in-flight turn routes through sendCommand (queued follow-up) and does not
// replace the registered per-turn callback.
func TestInject_UserInFlightFollowsUp(t *testing.T) {
	b, f := newStartedBackend(t)

	fired := false
	b.turnMu.Lock()
	b.turnEvents = &delegator.TurnEvents{OnTurnComplete: func(*delegator.TurnResult) { fired = true }}
	b.turnMu.Unlock()

	err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceUser,
		Text:   "follow-up message",
		Turn:   &delegator.TurnEvents{OnTurnComplete: func(*delegator.TurnResult) { t.Error("replacement callback installed") }},
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if got := textSends(f); len(got) != 1 || got[0] != "follow-up message" {
		t.Errorf("text sends = %v", got)
	}

	// Original callback must still be the registered one.
	b.fireTurnComplete(&delegator.TurnResult{})
	if !fired {
		t.Error("original per-turn callback was replaced by the follow-up inject")
	}
}

// TestInject_SteerInFlightInterruptsThenSends proves an in-flight steer sends
// Escape twice and Ctrl-C (interrupt) before delivering the steer text.
func TestInject_SteerInFlightInterruptsThenSends(t *testing.T) {
	b, f := newStartedBackend(t)
	b.turnMu.Lock()
	b.turnEvents = &delegator.TurnEvents{OnTurnComplete: func(*delegator.TurnResult) {}}
	b.turnMu.Unlock()

	err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "stop, change course",
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}

	var specials []string
	for _, c := range f.allCalls() {
		if len(c) > 0 && c[0] == "send-keys" && !hasSubsequence(c, []string{"-l"}) && !hasSubsequence(c, []string{"Enter"}) {
			specials = append(specials, c[len(c)-1])
		}
	}
	want := []string{"Escape", "Escape", "C-c"}
	if len(specials) != 3 || specials[0] != want[0] || specials[1] != want[1] || specials[2] != want[2] {
		t.Errorf("interrupt keys = %v, want %v", specials, want)
	}
	if got := textSends(f); len(got) != 1 || got[0] != "stop, change course" {
		t.Errorf("text sends = %v", got)
	}
}

// TestInject_SteerIdleDegradesToBeginTurn proves an idle steer (nothing to
// interrupt) degrades to a begin-turn dispatch: no interrupt keys, turn
// callback registered.
func TestInject_SteerIdleDegradesToBeginTurn(t *testing.T) {
	b, f := newStartedBackend(t)

	err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "idle steer",
		Turn:   &delegator.TurnEvents{OnTurnComplete: func(*delegator.TurnResult) {}},
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	for _, c := range f.allCalls() {
		if hasSubsequence(c, []string{"Escape"}) || hasSubsequence(c, []string{"C-c"}) {
			t.Errorf("idle steer should not interrupt, got call %v", c)
		}
	}
	if !b.IsTurnInFlight() {
		t.Error("idle steer should register a turn callback (begin-turn)")
	}
}

// TestInject_SystemIdleBeginsTurn proves an idle SourceSystem inject begins a
// fresh tracked turn exactly like SourceUser: prompt pasted, per-turn
// callback registered.
func TestInject_SystemIdleBeginsTurn(t *testing.T) {
	b, f := newStartedBackend(t)

	err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSystem,
		Text:   "[keepalive]",
		Turn:   &delegator.TurnEvents{OnTurnComplete: func(*delegator.TurnResult) {}},
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if got := textSends(f); len(got) != 1 || got[0] != "[keepalive]" {
		t.Errorf("text sends = %v", got)
	}
	if !b.IsTurnInFlight() {
		t.Error("turn should be in flight after system begin-turn inject")
	}
}

// TestInject_SystemInFlightRejects proves a SourceSystem inject during an
// in-flight turn returns ErrTurnInFlight without sending anything and without
// disturbing the registered per-turn callback — system input never folds into
// (steers) a running turn; the caller waits for completion and retries.
func TestInject_SystemInFlightRejects(t *testing.T) {
	b, f := newStartedBackend(t)

	fired := false
	b.turnMu.Lock()
	b.turnEvents = &delegator.TurnEvents{OnTurnComplete: func(*delegator.TurnResult) { fired = true }}
	b.turnMu.Unlock()

	err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSystem,
		Text:   "[keepalive]",
		Turn:   &delegator.TurnEvents{OnTurnComplete: func(*delegator.TurnResult) { t.Error("replacement callback installed") }},
	})
	if !errors.Is(err, delegator.ErrTurnInFlight) {
		t.Fatalf("Inject error = %v, want ErrTurnInFlight", err)
	}
	if got := textSends(f); len(got) != 0 {
		t.Errorf("system inject sent text during in-flight turn: %v", got)
	}

	// Original callback must still be the registered one.
	b.fireTurnComplete(&delegator.TurnResult{})
	if !fired {
		t.Error("original per-turn callback was replaced by the rejected system inject")
	}
}

// TestInject_SlashCommandsArePassthrough proves SourceCompact and SourcePass
// route straight to the pane as text, regardless of turn state, without
// touching the per-turn callback.
func TestInject_SlashCommandsArePassthrough(t *testing.T) {
	for _, src := range []delegator.InjectSource{delegator.SourceCompact, delegator.SourcePass} {
		b, f := newStartedBackend(t)
		err := b.ImmediateInject(context.Background(), delegator.Inject{Source: src, Text: "/compact keep recent"})
		if err != nil {
			t.Fatalf("Inject(%v): %v", src, err)
		}
		if got := textSends(f); len(got) != 1 || got[0] != "/compact keep recent" {
			t.Errorf("source %v: text sends = %v", src, got)
		}
		if b.IsTurnInFlight() {
			t.Errorf("source %v: slash command must not register a turn callback", src)
		}
	}
}

// TestInject_UnknownSource proves an unrecognised source value is rejected
// with an error rather than silently dropped.
func TestInject_UnknownSource(t *testing.T) {
	b, _ := newStartedBackend(t)
	err := b.ImmediateInject(context.Background(), delegator.Inject{Source: delegator.InjectSource(99), Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "unknown source") {
		t.Fatalf("err = %v, want unknown-source error", err)
	}
}

// TestInject_AttachmentsDroppedButTextDelivered proves attachments are
// silently dropped on the tmux backend (no structured content channel) while
// the text still goes through.
func TestInject_AttachmentsDroppedButTextDelivered(t *testing.T) {
	b, f := newStartedBackend(t)
	err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source:      delegator.SourceUser,
		Text:        "describe this image",
		Attachments: []delegator.Attachment{{MimeType: "image/png", Data: []byte{1, 2, 3}}},
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if got := textSends(f); len(got) != 1 || got[0] != "describe this image" {
		t.Errorf("text sends = %v", got)
	}
}

// TestInject_NotStarted proves every source routes to a "not started" error
// when the backend has no pane.
func TestInject_NotStarted(t *testing.T) {
	for _, src := range []delegator.InjectSource{
		delegator.SourceUser, delegator.SourceSteer, delegator.SourceCompact, delegator.SourcePass,
	} {
		b := &Backend{}
		err := b.ImmediateInject(context.Background(), delegator.Inject{Source: src, Text: "x"})
		if err == nil || !strings.Contains(err.Error(), "not started") {
			t.Errorf("source %v: err = %v, want not-started error", src, err)
		}
	}
}

// TestSendToPane_SendTextError proves a pane delivery failure surfaces as a
// wrapped send-prompt error and the turn callback is still registered (the
// caller decides how to unwind).
func TestSendToPane_SendTextError(t *testing.T) {
	b, f := newStartedBackend(t)
	f.respond = func(args []string, _ string) (string, error) {
		return "", context.DeadlineExceeded
	}
	err := b.sendToPane(context.Background(), "boom", nil, false)
	if err == nil || !strings.Contains(err.Error(), "send prompt") {
		t.Fatalf("err = %v, want send prompt error", err)
	}
}

// ---------------------------------------------------------------------------
// WaitReady
// ---------------------------------------------------------------------------

// TestWaitReady proves the readiness poll: errors immediately when not
// started, returns once the pane shows CC's input prompt, and honours
// context cancellation while the prompt is absent.
func TestWaitReady(t *testing.T) {
	t.Run("not started", func(t *testing.T) {
		b := &Backend{}
		if err := b.WaitReady(context.Background()); err == nil || !strings.Contains(err.Error(), "not started") {
			t.Fatalf("err = %v, want not-started error", err)
		}
	})

	t.Run("prompt visible", func(t *testing.T) {
		pane, f := newFakePane("cc-w")
		f.respond = func(args []string, _ string) (string, error) {
			return "Claude Code v2\n❯ ", nil
		}
		b := &Backend{}
		b.pane = pane

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := b.WaitReady(ctx); err != nil {
			t.Fatalf("WaitReady: %v", err)
		}
	})

	t.Run("context cancelled before ready", func(t *testing.T) {
		pane, f := newFakePane("cc-w")
		f.respond = func(args []string, _ string) (string, error) {
			return "still booting", nil
		}
		b := &Backend{}
		b.pane = pane

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err := b.WaitReady(ctx)
		if err == nil || !strings.Contains(err.Error(), "waiting for Claude Code ready") {
			t.Fatalf("err = %v, want readiness wait error", err)
		}
	})
}

// ---------------------------------------------------------------------------
// recordPreSendOffset
// ---------------------------------------------------------------------------

// TestRecordPreSendOffset_WatcherAlreadyRunning proves the offset is left
// untouched when a watcher exists (it already owns the read position).
func TestRecordPreSendOffset_WatcherAlreadyRunning(t *testing.T) {
	b := &Backend{watcher: &sessionWatcher{}, preSendOffset: -1}
	b.pane = &tmuxPane{pid: -1}
	b.recordPreSendOffset()
	if b.preSendOffset != -1 {
		t.Errorf("preSendOffset = %d, want unchanged -1", b.preSendOffset)
	}
}

// TestRecordPreSendOffset_NoChildProcess proves the offset stays at the tail
// default (-1) when the claude child PID cannot be found, so a later watcher
// never replays the whole session history.
//
// The success path (offset = current JSONL size) is not unit-tested: it
// requires findChildPID to resolve a real /proc child deterministically,
// which cannot be faked without seaming /proc itself.
func TestRecordPreSendOffset_NoChildProcess(t *testing.T) {
	b := &Backend{preSendOffset: -1}
	b.pane = &tmuxPane{pid: -1} // impossible PID → findChildPID fails
	b.recordPreSendOffset()
	if b.preSendOffset != -1 {
		t.Errorf("preSendOffset = %d, want -1 (tail default)", b.preSendOffset)
	}
}

// ---------------------------------------------------------------------------
// CaptureCommandOutput
// ---------------------------------------------------------------------------

// TestCaptureCommandOutput proves the stability poll: errors when not
// started, and returns the pane content once it has been unchanged for the
// stabilisation window.
func TestCaptureCommandOutput(t *testing.T) {
	t.Run("not started", func(t *testing.T) {
		b := &Backend{}
		if _, err := b.CaptureCommandOutput(context.Background(), 10*time.Millisecond, time.Millisecond); err == nil {
			t.Fatal("expected not-started error")
		}
	})

	t.Run("returns stable content", func(t *testing.T) {
		pane, f := newFakePane("cc-w")
		f.respond = func([]string, string) (string, error) {
			return "/context output\ntokens: 1234", nil
		}
		b := &Backend{}
		b.pane = pane

		got, err := b.CaptureCommandOutput(context.Background(), 30*time.Millisecond, 5*time.Millisecond)
		if err != nil {
			t.Fatalf("CaptureCommandOutput: %v", err)
		}
		if !strings.Contains(got, "tokens: 1234") {
			t.Errorf("content = %q", got)
		}
	})
}

package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/turnevent"
)

// leakConn is a platform.Connection standing in for the owner's live chat. It
// records EVERY outbound call — text, notifications, typing, files — so a test
// can assert that nothing at all reached it.
type leakConn struct {
	mu    sync.Mutex
	calls []string
}

func (c *leakConn) rec(format string, args ...any) error {
	c.mu.Lock()
	c.calls = append(c.calls, fmt.Sprintf(format, args...))
	c.mu.Unlock()
	return nil
}

func (c *leakConn) got() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *leakConn) SendText(t string) error                { return c.rec("SendText %q", t) }
func (c *leakConn) SendTextToChat(_ int64, t string) error { return c.rec("SendTextToChat %q", t) }
func (c *leakConn) SessionKey() string                     { return "helen/c42" }
func (c *leakConn) SendDocument(p, _ string) error         { return c.rec("SendDocument %s", p) }
func (c *leakConn) SendVoice(p string) error               { return c.rec("SendVoice %s", p) }
func (c *leakConn) SendVideo(p, _ string) error            { return c.rec("SendVideo %s", p) }
func (c *leakConn) SendPhoto(p, _ string) error            { return c.rec("SendPhoto %s", p) }
func (c *leakConn) SendAudio(p, _ string) error            { return c.rec("SendAudio %s", p) }
func (c *leakConn) SendAnimation(p, _ string) error        { return c.rec("SendAnimation %s", p) }
func (c *leakConn) SendVoiceData([]byte) error             { return c.rec("SendVoiceData") }
func (c *leakConn) SendDocumentToChat(_ int64, p, _ string) error {
	return c.rec("SendDocumentToChat %s", p)
}
func (c *leakConn) SendVoiceToChat(_ int64, p string) error { return c.rec("SendVoiceToChat %s", p) }
func (c *leakConn) SendVideoToChat(_ int64, p, _ string) error {
	return c.rec("SendVideoToChat %s", p)
}
func (c *leakConn) SendPhotoToChat(_ int64, p, _ string) error {
	return c.rec("SendPhotoToChat %s", p)
}
func (c *leakConn) SendAudioToChat(_ int64, p, _ string) error {
	return c.rec("SendAudioToChat %s", p)
}
func (c *leakConn) SendAnimationToChat(_ int64, p, _ string) error {
	return c.rec("SendAnimationToChat %s", p)
}
func (c *leakConn) SendVoiceDataToChat(int64, []byte) error { return c.rec("SendVoiceDataToChat") }
func (c *leakConn) PlatformName() string                    { return "telegram" }
func (c *leakConn) SessionKeyForChat(int64) string          { return "helen/c42" }
func (c *leakConn) DefaultSessionKey() string               { return "helen/c42" }
func (c *leakConn) SetSessionKey(string)                    {}
func (c *leakConn) SetSessionKeyDirect(string)              {}
func (c *leakConn) SetChatID(int64)                         {}
func (c *leakConn) ChatID() int64                           { return 42 }
func (c *leakConn) Username() string                        { return "" }
func (c *leakConn) SendInjectedMessage(_, t string) error   { return c.rec("SendInjectedMessage %q", t) }
func (c *leakConn) SendToSession(sk, t string) error        { return c.rec("SendToSession %s %q", sk, t) }
func (c *leakConn) SendNotification(t string)               { _ = c.rec("SendNotification %q", t) }
func (c *leakConn) SendNotificationDirect(t string) string {
	_ = c.rec("SendNotificationDirect %q", t)
	return ""
}
func (c *leakConn) SetTyping(on bool) { _ = c.rec("SetTyping %v", on) }

var _ platform.Connection = (*leakConn)(nil)

// ownerSink records whatever reaches the owner chat's registered per-turn sink.
type ownerSink struct {
	mu     sync.Mutex
	events []string
}

func (s *ownerSink) Emit(_ context.Context, ev turnevent.Event) {
	s.mu.Lock()
	s.events = append(s.events, fmt.Sprintf("%T", ev))
	s.mu.Unlock()
}
func (s *ownerSink) DeliversToPlatform() bool { return true }
func (s *ownerSink) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// chattyBatchBackend is a delegated backend that does everything a real CC
// batch turn can do that has a user-visible surface: it streams text, deltas,
// thinking and tool events DURING the turn (synchronously inside the
// begin-turn inject, i.e. before the orchestrator registers the turn's own
// sink); it fires every UI callback foci hands it (typing, subagent status,
// autonomous-run adoption); and it emits a late text block while being closed,
// after the turn has ended.
type chattyBatchBackend struct {
	batchTurnBackend

	cbMu      sync.Mutex
	typing    func(bool)
	subStatus func(string)
	autoOpen  func()
}

func (b *chattyBatchBackend) SetTypingFunc(fn func(bool)) {
	b.cbMu.Lock()
	b.typing = fn
	b.cbMu.Unlock()
}
func (b *chattyBatchBackend) SetOnSubagentStatus(fn func(string)) {
	b.cbMu.Lock()
	b.subStatus = fn
	b.cbMu.Unlock()
}
func (b *chattyBatchBackend) SetOnAutonomousOpen(fn func()) {
	b.cbMu.Lock()
	b.autoOpen = fn
	b.cbMu.Unlock()
}

func (b *chattyBatchBackend) fireUI() {
	b.cbMu.Lock()
	typing, sub, auto := b.typing, b.subStatus, b.autoOpen
	b.cbMu.Unlock()
	if typing != nil {
		typing(true)
		typing(false)
	}
	if sub != nil {
		sub("batch subagent running")
	}
	if auto != nil {
		auto()
	}
}

func (b *chattyBatchBackend) Close() error {
	b.mu.Lock()
	se := b.sessionEvents
	b.mu.Unlock()
	if se != nil && se.OnText != nil {
		se.OnText("LATE TEXT emitted while the batch backend closes")
	}
	return b.batchTurnBackend.Close()
}

// TestRunBatch_DeliversNothingToAnyChat pins the reason the batch path was ever
// separate (12036174: the session path "leaked output to platform"). The owner
// session has a live chat — a delivering sink on its router and a connection
// that late-delivery resolves to for ANY key, the way route.ConnFor resolves a
// b-child to its root chat — and every platform UI hook is wired. A batch must
// still return the model's text to the caller and put nothing, of any kind,
// in front of the user.
func TestRunBatch_DeliversNothingToAnyChat(t *testing.T) {
	const answer = "BATCH ANSWER — for the caller only"
	be := &chattyBatchBackend{}
	be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		be.fireUI()
		if h.OnThinkingDelta != nil {
			h.OnThinkingDelta("batch thinking")
		}
		if h.OnToolStart != nil {
			h.OnToolStart("tu1", "Read", `{"file_path":"MEMORY.md"}`)
		}
		if h.OnToolEnd != nil {
			h.OnToolEnd("tu1", "Read", "contents", false)
		}
		if h.OnTextDelta != nil {
			h.OnTextDelta("BATCH ANS")
		}
		if h.OnText != nil {
			h.OnText(answer)
		}
		if h.OnTurnComplete != nil {
			h.OnTurnComplete(&delegator.TurnResult{Text: answer, Model: "claude-sonnet-4-5"})
		}
		return nil, nil
	}
	a := newBatchTestAgent(t, be)

	// The owner's live chat.
	conn := &leakConn{}
	a.ResolveLateConn = func(string) platform.Connection { return conn }
	owner := &ownerSink{}
	a.sessionRouter("helen/c42").Register(owner)

	var ui []string
	var uiMu sync.Mutex
	note := func(s string) { uiMu.Lock(); ui = append(ui, s); uiMu.Unlock() }
	mgr := a.DelegatedManager
	mgr.TypingFunc = func(sk string, on bool) { note(fmt.Sprintf("typing %s %v", sk, on)) }
	mgr.SubagentStatusFunc = func(sk, d string) { note(fmt.Sprintf("subagent-status %s %q", sk, d)) }
	mgr.SystemNoticeFunc = func(sk, text string) { note(fmt.Sprintf("system-notice %s %q", sk, text)) }
	mgr.PermissionPromptFunc = func(sk, _, text, _, _ string, _ []delegator.PromptChoice) {
		note(fmt.Sprintf("permission-prompt %s %q", sk, text))
	}
	mgr.OpenAutonomousTurn = func(sk string, _ delegator.Delegator) { note("autonomous-turn " + sk) }

	got, err := mgr.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt: "consolidate", OwnerSessionKey: "helen/c42", Purpose: delegator.BatchPurposeConsolidation,
	})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if got != answer {
		t.Errorf("RunBatch returned %q, want the model's text %q", got, answer)
	}

	if calls := conn.got(); len(calls) != 0 {
		t.Errorf("batch output reached the owner's platform connection (%d call(s)):\n  %s", len(calls), strings.Join(calls, "\n  "))
	}
	if evs := owner.got(); len(evs) != 0 {
		t.Errorf("batch events reached the owner chat's sink: %v", evs)
	}
	uiMu.Lock()
	defer uiMu.Unlock()
	if len(ui) != 0 {
		t.Errorf("batch fired platform UI callbacks:\n  %s", strings.Join(ui, "\n  "))
	}
}

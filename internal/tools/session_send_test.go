package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"foci/internal/provider"
	"foci/internal/session"
)

// mockSessionAppender captures Append calls.
type mockSessionAppender struct {
	key      string
	msg      provider.Message
	err      error
	appended bool
}

func (m *mockSessionAppender) For(sessionKey string) session.SessionWriter {
	return &mockSessionWriter{appender: m}
}

type mockSessionWriter struct {
	appender *mockSessionAppender
}

func (w *mockSessionWriter) Append(key string, msg provider.Message) error {
	w.appender.key = key
	w.appender.msg = msg
	w.appender.appended = true
	return w.appender.err
}

func (w *mockSessionWriter) AppendAll(key string, msgs []provider.Message) error {
	return nil
}

func (w *mockSessionWriter) Replace(key string, msgs []provider.Message) error {
	return nil
}

func (w *mockSessionWriter) Clear(key string) error {
	return nil
}

func TestSendToSession(t *testing.T) {
	// Verifies basic send_to_session flow: message is delivered via notifier
	// with reply_to=caller (default).
	t.Parallel()
	store := &mockSessionAppender{}
	delivered := make(chan struct{ sk, msg string }, 1)
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		delivered <- struct{ sk, msg string }{sk, msg}
	})

	tool := NewSendToSessionTool(store, notifier, nil, nil)

	ctx := WithSessionKey(context.Background(), "test/i111")
	params, _ := json.Marshal(map[string]string{
		"session_key": "test/i0",
		"message":     "Here are the results of my research.",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "Message sent to session test/i0") {
		t.Errorf("result = %q", result.Text)
	}

	// The tool no longer appends directly — InjectToAgent triggers
	// HandleMessage which loads the session and appends the message.
	// So we only verify the notifier was called correctly.

	// Check notifier was triggered (default reply_to=caller)
	d := <-delivered
	if d.sk != "test/i0" {
		t.Errorf("notifier session = %q, want test/i0", d.sk)
	}
	if !strings.Contains(d.msg, "Here are the results of my research.") {
		t.Errorf("notifier msg = %q", d.msg)
	}
}

func TestSendToSessionReplyToSession(t *testing.T) {
	// Verifies reply_to=session routes through sessionNotifyFn instead of
	// the caller notifier.
	t.Parallel()
	store := &mockSessionAppender{}
	callerNotified := false
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		callerNotified = true
	})

	sessionDelivered := make(chan struct{ sk, msg string }, 1)
	sessionNotifyFn := SessionNotifyFn(func(sk, msg string) {
		sessionDelivered <- struct{ sk, msg string }{sk, msg}
	})

	tool := NewSendToSessionTool(store, notifier, sessionNotifyFn, nil)

	ctx := WithSessionKey(context.Background(), "alpha/c111")
	params, _ := json.Marshal(map[string]string{
		"session_key": "beta/c222",
		"message":     "Tell Eleni about dinner.",
		"reply_to":    "session",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "reply_to=session") {
		t.Errorf("result = %q, want reply_to=session", result.Text)
	}

	// Check sessionNotifyFn was called, not the caller notifier
	d := <-sessionDelivered
	if d.sk != "beta/c222" {
		t.Errorf("session notify key = %q, want beta/c222", d.sk)
	}
	if !strings.Contains(d.msg, "Tell Eleni about dinner.") {
		t.Errorf("session notify msg = %q", d.msg)
	}
	if callerNotified {
		t.Error("caller notifier should not have been called with reply_to=session")
	}
	// reply_to=session should NOT append — HandleMessage does that
	if store.appended {
		t.Error("Append should not be called for reply_to=session (HandleMessage appends)")
	}
}

func TestSendToSessionInvalidReplyTo(t *testing.T) {
	// Verifies that an invalid reply_to value is rejected.
	t.Parallel()
	store := &mockSessionAppender{}
	tool := NewSendToSessionTool(store, nil, nil, nil)

	ctx := WithSessionKey(context.Background(), "test/i0")
	params, _ := json.Marshal(map[string]string{
		"session_key": "test/i1",
		"message":     "hello",
		"reply_to":    "invalid",
	})

	_, err := tool.Execute(ctx, params)
	if err == nil {
		t.Fatal("expected error for invalid reply_to")
	}
	if !strings.Contains(err.Error(), "reply_to must be") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendToSessionEmptyParams(t *testing.T) {
	// Verifies that empty session_key and message are rejected.
	t.Parallel()
	store := &mockSessionAppender{}
	tool := NewSendToSessionTool(store, nil, nil, nil)

	// Empty session_key
	params, _ := json.Marshal(map[string]string{
		"session_key": "",
		"message":     "hello",
	})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for empty session_key")
	}
	if !strings.Contains(err.Error(), "session_key is required") {
		t.Errorf("error = %q", err.Error())
	}

	// Empty message
	params, _ = json.Marshal(map[string]string{
		"session_key": "test/i0",
		"message":     "",
	})
	_, err = tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for empty message")
	}
	if !strings.Contains(err.Error(), "message is required") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendToSessionNilNotifier(t *testing.T) {
	// Verifies graceful behavior when notifier is nil.
	t.Parallel()
	store := &mockSessionAppender{}
	tool := NewSendToSessionTool(store, nil, nil, nil)

	ctx := WithSessionKey(context.Background(), "test/i0")
	params, _ := json.Marshal(map[string]string{
		"session_key": "test/i1",
		"message":     "hello",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Message sent") {
		t.Errorf("result = %q", result.Text)
	}
	// With nil notifier, no append should happen (no way to trigger HandleMessage)
	if store.appended {
		t.Error("Append should not be called when notifier is nil (no way to process message)")
	}
}

func TestSendToSessionPerUserChatRouting(t *testing.T) {
	// Verifies that cross-session communication between per-user chat sessions
	// routes to the correct target session key, enabling chat ID extraction
	// for Telegram delivery.
	t.Parallel()
	store := &mockSessionAppender{}
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		// reply_to=caller: verify the notifier receives the TARGET session key
		// so the async_notify callback can extract the chat ID.
		if ChatIDFromSessionKey(sk) == 0 {
			t.Errorf("async_notify should receive session key with extractable chat ID, got %q", sk)
		}
	})

	sessionDelivered := make(chan struct{ sk, msg string }, 1)
	sessionNotifyFn := SessionNotifyFn(func(sk, msg string) {
		sessionDelivered <- struct{ sk, msg string }{sk, msg}
	})

	tool := NewSendToSessionTool(store, notifier, sessionNotifyFn, nil)

	// Dick's session sends to Eleni's session with reply_to=session
	dickSession := "fotini/c5970082313"
	eleniSession := "fotini/c8792716180"

	ctx := WithSessionKey(context.Background(), dickSession)
	params, _ := json.Marshal(map[string]string{
		"session_key": eleniSession,
		"message":     "Στείλε μήνυμα στην Ελένη",
		"reply_to":    "session",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "reply_to=session") {
		t.Errorf("result = %q", result.Text)
	}

	// Verify sessionNotifyFn receives the TARGET session key (Eleni's),
	// not the caller's (Dick's)
	d := <-sessionDelivered
	if d.sk != eleniSession {
		t.Errorf("session notify key = %q, want %s", d.sk, eleniSession)
	}
	// Verify chat ID can be extracted from the target session key
	chatID := ChatIDFromSessionKey(d.sk)
	if chatID != 8792716180 {
		t.Errorf("ChatIDFromSessionKey(%q) = %d, want 8792716180", d.sk, chatID)
	}

	// Now test reply_to=caller path
	params, _ = json.Marshal(map[string]string{
		"session_key": eleniSession,
		"message":     "What did Eleni say?",
		"reply_to":    "caller",
	})

	result, err = tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute (caller): %v", err)
	}
	if !strings.Contains(result.Text, "reply_to=caller") {
		t.Errorf("result = %q", result.Text)
	}
	// Note: The tool no longer appends directly for reply_to=caller.
	// InjectToAgent triggers HandleMessage which does the append.
}

func TestSendToSessionBareNameResolution(t *testing.T) {
	// Verifies that a loose target that doesn't parse as a session key
	// (a bare agent name) is resolved via resolveKeyFn before routing,
	// while the resolved full key is used for delivery and in the result text.
	t.Parallel()
	store := &mockSessionAppender{}
	delivered := make(chan struct{ sk, msg string }, 1)
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		delivered <- struct{ sk, msg string }{sk, msg}
	})

	resolveKeyFn := func(target string) (string, string, error) {
		if target == "scout" {
			return "scout/c5970082313", "default", nil
		}
		return "", "", fmt.Errorf("no such target %q", target)
	}

	tool := NewSendToSessionTool(store, notifier, nil, resolveKeyFn)

	ctx := WithSessionKey(context.Background(), "test/i0")
	params, _ := json.Marshal(map[string]string{
		"session_key": "scout",
		"message":     "resolved bare agent name",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "scout/c5970082313") {
		t.Errorf("result = %q, want resolved key in output", result.Text)
	}

	d := <-delivered
	if d.sk != "scout/c5970082313" {
		t.Errorf("notifier session = %q, want scout/c5970082313", d.sk)
	}
}

func TestSendToSessionFullKeySkipsResolution(t *testing.T) {
	// Verifies that a well-formed session key is used as-is and never goes
	// through resolveKeyFn — keys are stable identities, so a parseable key
	// needs no resolution.
	t.Parallel()
	store := &mockSessionAppender{}
	delivered := make(chan struct{ sk, msg string }, 1)
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		delivered <- struct{ sk, msg string }{sk, msg}
	})

	resolveKeyFn := func(target string) (string, string, error) {
		t.Errorf("resolveKeyFn should not be called for full key, got %q", target)
		return "", "", nil
	}

	tool := NewSendToSessionTool(store, notifier, nil, resolveKeyFn)

	ctx := WithSessionKey(context.Background(), "test/i0")
	params, _ := json.Marshal(map[string]string{
		"session_key": "scout/c5970082313",
		"message":     "full key passthrough",
	})

	if _, err := tool.Execute(ctx, params); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	d := <-delivered
	if d.sk != "scout/c5970082313" {
		t.Errorf("notifier session = %q, want scout/c5970082313", d.sk)
	}
}

func TestSendToSessionBareNameUnresolved(t *testing.T) {
	// Verifies that an unresolvable bare agent name returns an error instead
	// of silently sending to a nonexistent session.
	t.Parallel()
	store := &mockSessionAppender{}
	resolveKeyFn := func(target string) (string, string, error) { return "", "", fmt.Errorf("unresolvable") }

	tool := NewSendToSessionTool(store, nil, nil, resolveKeyFn)

	ctx := WithSessionKey(context.Background(), "test/i0")
	params, _ := json.Marshal(map[string]string{
		"session_key": "nonexistent",
		"message":     "hello",
	})

	_, err := tool.Execute(ctx, params)
	if err == nil {
		t.Fatal("expected error for unresolvable bare agent name")
	}
	if !strings.Contains(err.Error(), "could not resolve") {
		t.Errorf("error = %q", err.Error())
	}
}

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"foci/anthropic"
)

// mockSessionAppender captures Append calls.
type mockSessionAppender struct {
	key      string
	msg      anthropic.Message
	err      error
	appended bool
}

func (m *mockSessionAppender) Append(key string, msg anthropic.Message) error {
	m.key = key
	m.msg = msg
	m.appended = true
	return m.err
}

func TestSendToSession(t *testing.T) {
	store := &mockSessionAppender{}
	delivered := make(chan struct{ sk, msg string }, 1)
	notifier := NewAsyncNotifier(func(sk, msg string) {
		delivered <- struct{ sk, msg string }{sk, msg}
	})

	tool := NewSendToSessionTool(store, notifier, nil)

	ctx := WithSessionKey(context.Background(), "agent:test:multiball:mb-111")
	params, _ := json.Marshal(map[string]string{
		"session_key": "agent:test:main",
		"message":     "Here are the results of my research.",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "Message sent to session agent:test:main") {
		t.Errorf("result = %q", result)
	}

	// Check the appended message
	if store.key != "agent:test:main" {
		t.Errorf("appended to key = %q, want agent:test:main", store.key)
	}
	if store.msg.Role != "user" {
		t.Errorf("msg role = %q, want user", store.msg.Role)
	}
	text := anthropic.TextOf(store.msg.Content)
	if !strings.Contains(text, "[Message from session agent:test:multiball:mb-111]") {
		t.Errorf("expected origin tag, got %q", text)
	}
	if !strings.Contains(text, "Here are the results of my research.") {
		t.Errorf("expected message body, got %q", text)
	}

	// Check notifier was triggered (default reply_to=caller)
	d := <-delivered
	if d.sk != "agent:test:main" {
		t.Errorf("notifier session = %q, want agent:test:main", d.sk)
	}
	if !strings.Contains(d.msg, "Here are the results of my research.") {
		t.Errorf("notifier msg = %q", d.msg)
	}
}

func TestSendToSessionReplyToSession(t *testing.T) {
	store := &mockSessionAppender{}
	callerNotified := false
	notifier := NewAsyncNotifier(func(sk, msg string) {
		callerNotified = true
	})

	sessionDelivered := make(chan struct{ sk, msg string }, 1)
	sessionNotifyFn := SessionNotifyFn(func(sk, msg string) {
		sessionDelivered <- struct{ sk, msg string }{sk, msg}
	})

	tool := NewSendToSessionTool(store, notifier, sessionNotifyFn)

	ctx := WithSessionKey(context.Background(), "agent:alpha:chat:111")
	params, _ := json.Marshal(map[string]string{
		"session_key": "agent:beta:chat:222",
		"message":     "Tell Eleni about dinner.",
		"reply_to":    "session",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "reply_to=session") {
		t.Errorf("result = %q, want reply_to=session", result)
	}

	// Check sessionNotifyFn was called, not the caller notifier
	d := <-sessionDelivered
	if d.sk != "agent:beta:chat:222" {
		t.Errorf("session notify key = %q, want agent:beta:chat:222", d.sk)
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
	store := &mockSessionAppender{}
	tool := NewSendToSessionTool(store, nil, nil)

	ctx := WithSessionKey(context.Background(), "agent:test:main")
	params, _ := json.Marshal(map[string]string{
		"session_key": "agent:test:branch",
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
	store := &mockSessionAppender{}
	tool := NewSendToSessionTool(store, nil, nil)

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
		"session_key": "agent:test:main",
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

func TestSendToSessionAppendError(t *testing.T) {
	store := &mockSessionAppender{err: fmt.Errorf("disk full")}
	tool := NewSendToSessionTool(store, nil, nil)

	ctx := WithSessionKey(context.Background(), "agent:test:main")
	params, _ := json.Marshal(map[string]string{
		"session_key": "agent:test:branch",
		"message":     "hello",
	})

	_, err := tool.Execute(ctx, params)
	if err == nil {
		t.Fatal("expected error from append")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSendToSessionNilNotifier(t *testing.T) {
	store := &mockSessionAppender{}
	tool := NewSendToSessionTool(store, nil, nil)

	ctx := WithSessionKey(context.Background(), "agent:test:main")
	params, _ := json.Marshal(map[string]string{
		"session_key": "agent:test:branch",
		"message":     "hello",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Message sent") {
		t.Errorf("result = %q", result)
	}
	// Should still have appended
	if store.key != "agent:test:branch" {
		t.Errorf("appended to key = %q", store.key)
	}
}

package agent

import (
	"context"
	"testing"
)

// TestCheckStaleContext_OK verifies CheckStaleContext returns nil for a live context.
func TestCheckStaleContext_OK(t *testing.T) {
	s := &sharedTurnOps{agent: &Agent{}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)

	if err := s.CheckStaleContext(ts); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestCheckStaleContext_Cancelled verifies CheckStaleContext returns the
// context error when the context is cancelled.
func TestCheckStaleContext_Cancelled(t *testing.T) {
	s := &sharedTurnOps{agent: &Agent{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ts := NewTurnState(ctx, "test/s", []string{"hi"}, nil)

	if err := s.CheckStaleContext(ts); err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

// TestTouchActivity verifies that OnActivity callbacks are fired.
func TestTouchActivity(t *testing.T) {
	var called string
	a := &Agent{}
	a.OnActivity = append(a.OnActivity, func(key string) { called = key })
	s := &sharedTurnOps{agent: a}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)

	s.TouchActivity(ts)

	if called != "test/s" {
		t.Fatalf("OnActivity called with %q, want %q", called, "test/s")
	}
}

// TestLoadSessionMeta verifies that LoadSessionMeta populates ts.SessionMeta.
func TestLoadSessionMeta(t *testing.T) {
	a := &Agent{}
	s := &sharedTurnOps{agent: a}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)

	s.LoadSessionMeta(ts)

	if ts.SessionMeta == nil {
		t.Fatal("SessionMeta should be non-nil after LoadSessionMeta")
	}
}

// TestLogConversationRecv_SetsChatID verifies that LogConversationRecv
// falls back to ChatIDFromKey when Meta.ChatID is zero.
func TestLogConversationRecv_SetsChatID(t *testing.T) {
	a := &Agent{}
	s := &sharedTurnOps{agent: a}
	ts := NewTurnState(context.Background(), "clutch/c100", []string{"hi"}, nil)
	ts.Meta = &TurnMetadata{} // ChatID = 0

	s.LogConversationRecv(ts)

	// ConvChatID should be set from the session key parse.
	if ts.ConvChatID == 0 {
		t.Fatal("ConvChatID should be set from session key")
	}
}


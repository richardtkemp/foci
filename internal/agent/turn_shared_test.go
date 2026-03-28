package agent

import (
	"context"
	"testing"

	"foci/internal/session"
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
	ts := NewTurnState(context.Background(), "clutch/c100/1000000000", []string{"hi"}, nil)
	ts.Meta = &TurnMetadata{} // ChatID = 0

	s.LogConversationRecv(ts)

	// ConvChatID should be set from the session key parse.
	if ts.ConvChatID == 0 {
		t.Fatal("ConvChatID should be set from session key")
	}
}

// TestRegisterSessionIndex_NilIndex verifies no panic when SessionIndex is nil.
func TestRegisterSessionIndex_NilIndex(t *testing.T) {
	a := &Agent{}
	s := &sharedTurnOps{agent: a}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Meta = &TurnMetadata{}

	// Should not panic.
	s.RegisterSessionIndex(ts)
}

// TestRegisterSessionIndex_Upserts verifies session appears in the index.
func TestRegisterSessionIndex_Upserts(t *testing.T) {
	idx, err := session.NewSessionIndex(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("create index: %v", err)
	}
	defer idx.Close()

	a := &Agent{SessionIndex: idx}
	s := &sharedTurnOps{agent: a}
	ts := NewTurnState(context.Background(), "clutch/c100/1000000000", []string{"hi"}, nil)
	ts.Meta = &TurnMetadata{}

	s.RegisterSessionIndex(ts)

	entry, err := idx.Get("clutch/c100/1000000000")
	if err != nil {
		t.Fatalf("Get after RegisterSessionIndex: %v", err)
	}
	if entry.SessionKey != "clutch/c100/1000000000" {
		t.Fatalf("entry.SessionKey = %q, want %q", entry.SessionKey, "clutch/c100/1000000000")
	}
}

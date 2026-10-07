package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/session"
)

// TestMemoryTriggerSkipsActivityBump proves option B of the reflection-skip
// feature: turns whose trigger is a memory-formation pass (reflection /
// session_end_memory) must NOT advance last_activity_at, while ordinary turns
// must. This is what keeps a reflection's own turn from looking like "activity
// since the last reflection" and defeating ReflectionRedundant on delegated
// agents (where reflection injects into the main session).
func TestMemoryTriggerSkipsActivityBump(t *testing.T) {
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close() //nolint:errcheck

	ag := &Agent{Model: "test", SessionIndex: idx}

	const key = "test-agent/i0"
	old := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     key,
		FilePath:       "f",
		CreatedAt:      old,
		LastActivityAt: old,
		SessionType:    session.SessionTypeChat,
		Status:         session.SessionStatusActive,
	})

	activity := func() time.Time {
		e, err := idx.Get(key)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return e.LastActivityAt
	}

	// Reflection turn: the single per-turn write must NOT advance last_activity_at.
	rts := NewTurnState(context.Background(), key, []string{"reflect"}, nil)
	rts.Trigger = "reflection"
	ag.recordTurnActivity(rts)
	if got := activity(); !got.Equal(old) {
		t.Errorf("reflection turn bumped last_activity_at: got %v, want unchanged %v", got, old)
	}

	// session_end_memory turn: same.
	sts := NewTurnState(context.Background(), key, []string{"memory"}, nil)
	sts.Trigger = "session_end_memory"
	ag.recordTurnActivity(sts)
	if got := activity(); !got.Equal(old) {
		t.Errorf("session_end_memory turn bumped last_activity_at: got %v, want unchanged %v", got, old)
	}

	// Ordinary user turn: MUST advance last_activity_at.
	uts := NewTurnState(context.Background(), key, []string{"hi"}, nil)
	uts.Trigger = "user"
	ag.recordTurnActivity(uts)
	if got := activity(); !got.After(old) {
		t.Errorf("user turn did not bump last_activity_at: still %v", got)
	}
}

// TestRecordTurnActivity_HumanSourceMarkerBumpsUserActivity proves the #1130
// human-source marker: an HTTP-injected turn (trigger "user") carries
// WithHumanSource when the caller declared a human sent it, and that marker
// alone widens the turn-entry write to last_user_activity_at — a person typing
// `foci send --human` counts as user attention exactly like a platform turn,
// landing on the turn's own session key.
func TestRecordTurnActivity_HumanSourceMarkerBumpsUserActivity(t *testing.T) {
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close() //nolint:errcheck

	ag := &Agent{Model: "test", SessionIndex: idx}
	const key = "test-agent/i0"
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  key,
		FilePath:    "f",
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})

	if _, ok := idx.LastUserActivity(key); ok {
		t.Fatal("session already has user activity before any turn")
	}

	ts := NewTurnState(WithHumanSource(context.Background()), key, []string{"hi"}, nil)
	ts.Trigger = "user"
	ag.recordTurnActivity(ts)

	if _, ok := idx.LastUserActivity(key); !ok {
		t.Errorf("human-source turn (trigger %q) did not write last_user_activity_at", ts.Trigger)
	}
}

// TestRecordTurnActivity_PlainUserTriggerNoUserActivityBump is the
// characterisation half of #1130: trigger "user" with NO marker stays
// automated — `isInteractiveTrigger("user")` must remain false so a plain
// HTTP /send (foci send from a cron, a webhook-style caller) never counts as
// user attention.
func TestRecordTurnActivity_PlainUserTriggerNoUserActivityBump(t *testing.T) {
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close() //nolint:errcheck

	ag := &Agent{Model: "test", SessionIndex: idx}
	const key = "test-agent/i0"
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  key,
		FilePath:    "f",
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})

	ts := NewTurnState(context.Background(), key, []string{"hi"}, nil)
	ts.Trigger = "user"
	ag.recordTurnActivity(ts)

	if _, ok := idx.LastUserActivity(key); ok {
		t.Errorf("plain trigger-%q turn wrote last_user_activity_at; HTTP turns are automated unless declared human", ts.Trigger)
	}
}

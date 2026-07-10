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

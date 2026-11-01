package session

// index_default_ordinary_test.go: the characterisation that the four-rung
// default-session ladder is unchanged for ordinary (Telegram/Discord/app)
// chats. Kept in a file that compiles against the base commit (no
// new-symbol references), so the red gate can prove it passes there.

import (
	"testing"
	"time"
)

// TestDefaultSessionKey_OrdinaryChatsUnchanged pins the four rungs of
// DefaultSessionKeyForAgentOn for ordinary (Telegram/Discord/app) chats, with
// exact keys, so the never-default exclusion added for Slack (#2459) cannot
// change default-session resolution for the existing transports.
func TestDefaultSessionKey_OrdinaryChatsUnchanged(t *testing.T) {
	idx := tempIndex(t)
	now := time.Now().UTC().Truncate(time.Second)

	// Telegram chat 111: pinned default, older activity. Discord chat 222:
	// registered only, far more recent. App chat 333: pinned default.
	mustUpsert := func(key string, lastAct time.Time) {
		t.Helper()
		idx.Upsert(SessionIndexEntry{SessionKey: key, SessionType: SessionTypeChat, Status: SessionStatusActive,
			CreatedAt: now.Add(-48 * time.Hour), LastActivityAt: lastAct})
		idx.TouchUserActivity(key, lastAct)
	}
	mustUpsert("ag/c111", now.Add(-24*time.Hour))
	mustUpsert("ag/c222", now.Add(-time.Minute))
	mustUpsert("ag/c333", now.Add(-12*time.Hour))
	for _, row := range []struct {
		platform string
		chat     int64
	}{
		{"telegram", 111}, {"discord", 222}, {"app", 333},
	} {
		if err := idx.SetChatMetadata("ag", row.platform, row.chat, "registered", "true"); err != nil {
			t.Fatal(err)
		}
	}
	if err := idx.SetDefaultChat("ag", "telegram", 111); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetDefaultChat("ag", "app", 333); err != nil {
		t.Fatal(err)
	}

	// Rung 1: the preferred platform's pinned default.
	if got := idx.DefaultSessionKeyForAgentOn("ag", "telegram"); got != "ag/c111" {
		t.Errorf("rung 1 (preferred pin) = %q, want ag/c111", got)
	}
	// Rung 2: preferred platform, no pin, most recently active registered chat.
	if err := idx.ClearDefaultChat("ag", "telegram"); err != nil {
		t.Fatal(err)
	}
	if got := idx.DefaultSessionKeyForAgentOn("ag", "telegram"); got != "ag/c111" {
		t.Errorf("rung 2 (preferred registered) = %q, want ag/c111", got)
	}
	// Rung 3: any is_default chat, ordered by user activity — app 333 is the
	// only remaining pin.
	if got := idx.DefaultSessionKeyForAgentOn("ag", ""); got != "ag/c333" {
		t.Errorf("rung 3 (any pin) = %q, want ag/c333", got)
	}
	// Rung 4: no pins at all — most recently user-active root session.
	if err := idx.ClearDefaultChat("ag", "app"); err != nil {
		t.Fatal(err)
	}
	if got := idx.DefaultSessionKeyForAgentOn("ag", ""); got != "ag/c222" {
		t.Errorf("rung 4 (most recent root) = %q, want ag/c222", got)
	}
	// Absent preferred platform falls through to the same ladder.
	if got := idx.DefaultSessionKeyForAgentOn("ag", "app"); got != "ag/c222" {
		t.Errorf("absent platform fallthrough = %q, want ag/c222", got)
	}
}

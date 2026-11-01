package session

import (
	"testing"
	"time"
)

// TestDefaultSessionKey_NeverDefaultChatExcluded proves every rung of
// DefaultSessionKeyForAgentOn skips chats flagged never_default (the Slack
// transport's conversations, #2459): a Slack session that is the most
// recently active root, a registered slack chat, even a slack chat pinned
// is_default — none of them can become the default. Each rung gets its own
// subtest so removing one exclusion clause fails exactly one.
func TestDefaultSessionKey_NeverDefaultChatExcluded(t *testing.T) {
	// seedSlackChat registers a slack chat (registered + never_default) with
	// a session row active at ts, optionally pinned is_default.
	seedSlackChat := func(idx *SessionIndex, chatID int64, ts time.Time, pin bool) {
		idx.Upsert(SessionIndexEntry{SessionKey: NewChatSessionKey("ag", chatID), SessionType: SessionTypeChat, Status: SessionStatusActive,
			CreatedAt: ts.Add(-time.Hour), LastActivityAt: ts})
		idx.TouchUserActivity(NewChatSessionKey("ag", chatID), ts)
		if err := idx.SetChatMetadata("ag", "slack", chatID, "registered", "true"); err != nil {
			t.Fatal(err)
		}
		if err := idx.SetChatMetadata("ag", "slack", chatID, ChatMetaNeverDefault, "true"); err != nil {
			t.Fatal(err)
		}
		if pin {
			// A stray is_default row, written directly as it would sit in
			// the table: SetDefaultChat refuses never-default chats now, so
			// this is the only shape of a pinned slack chat left — one the
			// rungs must still skip.
			if err := idx.SetChatMetadata("ag", "slack", chatID, "is_default", "true"); err != nil {
				t.Fatal(err)
			}
		}
	}
	// seedTelegramChat registers an ordinary chat (no flag), pinned optional.
	seedTelegramChat := func(idx *SessionIndex, chatID int64, ts time.Time, pin bool) {
		idx.Upsert(SessionIndexEntry{SessionKey: NewChatSessionKey("ag", chatID), SessionType: SessionTypeChat, Status: SessionStatusActive,
			CreatedAt: ts.Add(-time.Hour), LastActivityAt: ts})
		idx.TouchUserActivity(NewChatSessionKey("ag", chatID), ts)
		if err := idx.SetChatMetadata("ag", "telegram", chatID, "registered", "true"); err != nil {
			t.Fatal(err)
		}
		if pin {
			if err := idx.SetDefaultChat("ag", "telegram", chatID); err != nil {
				t.Fatal(err)
			}
		}
	}
	now := time.Now().UTC().Truncate(time.Second)

	t.Run("rung 1: preferred platform's pinned slack chat is skipped", func(t *testing.T) {
		idx := tempIndex(t)
		seedSlackChat(idx, 111, now, true) // pinned default on slack
		seedTelegramChat(idx, 222, now.Add(-time.Minute), true)
		if got := idx.DefaultSessionKeyForAgentOn("ag", "slack"); got != "ag/c222" {
			t.Errorf("rung 1 = %q, want the telegram pin (the slack pin is never-default)", got)
		}
	})
	t.Run("rung 2: preferred platform's registered slack chat is skipped", func(t *testing.T) {
		idx := tempIndex(t)
		seedSlackChat(idx, 111, now, false) // most recently active registered slack chat
		seedTelegramChat(idx, 222, now.Add(-time.Minute), false)
		if got := idx.DefaultSessionKeyForAgentOn("ag", "slack"); got != "ag/c222" {
			t.Errorf("rung 2 = %q, want the telegram chat (the slack chat is never-default)", got)
		}
	})
	t.Run("rung 3: any is_default slack chat is skipped", func(t *testing.T) {
		idx := tempIndex(t)
		seedSlackChat(idx, 111, now, true) // pinned, newer
		seedTelegramChat(idx, 222, now.Add(-time.Minute), true)
		if got := idx.DefaultSessionKeyForAgentOn("ag", ""); got != "ag/c222" {
			t.Errorf("rung 3 = %q, want the telegram pin (a slack pin must never win)", got)
		}
	})
	t.Run("rung 4: most recently active slack root is skipped", func(t *testing.T) {
		idx := tempIndex(t)
		seedTelegramChat(idx, 222, now.Add(-24*time.Hour), false)
		seedSlackChat(idx, 111, now, false) // the most recently active root
		if got := idx.DefaultSessionKeyForAgentOn("ag", ""); got != "ag/c222" {
			t.Errorf("rung 4 = %q, want the telegram root (the newer slack root is never-default)", got)
		}
		// Only the slack chat exists: nothing resolvable at all.
		idx2 := tempIndex(t)
		seedSlackChat(idx2, 111, now, false)
		if got := idx2.DefaultSessionKeyForAgentOn("ag", ""); got != "" {
			t.Errorf("slack-only agent resolved default %q, want \"\"", got)
		}
	})
}

// TestSetDefaultChat_RefusesNeverDefaultChat proves the /sessions default
// write cannot pin a never-default conversation (#2459 req 6): a chat
// carrying the never_default row (what a Slack registration writes) is
// refused with an error the command surfaces, no is_default row lands, and
// the agent's existing default on another chat stays in place.
func TestSetDefaultChat_RefusesNeverDefaultChat(t *testing.T) {
	idx := tempIndex(t)
	if err := idx.SetDefaultChat("ag", "telegram", 222); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetChatMetadata("ag", "slack", 111, ChatMetaNeverDefault, "true"); err != nil {
		t.Fatal(err)
	}

	if err := idx.SetDefaultChat("ag", "telegram", 111); err == nil {
		t.Fatal("SetDefaultChat accepted a never-default chat; want the refusal error")
	}
	if got := idx.DefaultChatForAgent("ag", "telegram"); got != 222 {
		t.Errorf("default chat after the refused pin = %d, want 222 (the old default untouched)", got)
	}

	// The refusal must not overreach: an ordinary chat still pins.
	if err := idx.SetDefaultChat("ag", "telegram", 333); err != nil {
		t.Fatalf("SetDefaultChat(ordinary chat) = %v", err)
	}
	if got := idx.DefaultChatForAgent("ag", "telegram"); got != 333 {
		t.Errorf("default chat = %d, want 333", got)
	}
}

// TestDefaultSessionKey_OrdinaryChatsUnchanged lives in
// index_default_ordinary_test.go (a base-compilable characterisation file).

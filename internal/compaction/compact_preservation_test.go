package compaction

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"foci/internal/anthropic"
	"foci/internal/provider"
	"foci/internal/session"
)

func TestCompactPreserveMessages(t *testing.T) {
	// Verifies that the preserve count correctly retains the
	// last N messages verbatim in the compacted session, that the summary includes a
	// preservation note, that the handoff is folded into the summary when the first
	// preserved message is a user turn, and that role alternation is maintained throughout.
	server := mockCompactionServer("Summary of conversation.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Add 10 messages (5 user + 5 assistant)
	for i := 0; i < 5; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent(fmt.Sprintf("user msg %d", i))})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent(fmt.Sprintf("assistant reply %d", i))})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.WithConfig(4096, 4, 4) // preserve last 4 messages

	summary, newKey, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if summary == "" {
		t.Error("expected non-empty summary")
	}

	// After compaction: preserved[0] is user, so handoff folds into summary.
	// 2 (marker + summary+handoff) + 4 preserved = 6
	msgs, _ := store.Load(newKey)
	if len(msgs) != 6 {
		t.Fatalf("after compact: %d messages, want 6", len(msgs))
	}

	// Summary+handoff (folded) should contain the preservation note
	summaryText := provider.TextOf(msgs[1].Content)
	if !strings.Contains(summaryText, "last 4 messages") {
		t.Errorf("summary missing preservation note: %q", summaryText)
	}
	// Handoff text should be folded into the summary
	if !strings.Contains(summaryText, "Compaction complete") {
		t.Errorf("summary should contain folded handoff: %q", summaryText)
	}

	// 10 messages: [u0,a0,u1,a1,u2,a2,u3,a3,u4,a4]
	// Preserve last 4: [u3,a3,u4,a4]
	// preserved[0]=user → handoff folded into summary
	// Result: [marker, summary+handoff, u3, a3, u4, a4]
	expected := []struct {
		role string
		text string
	}{
		{"user", "user msg 3"},
		{"assistant", "assistant reply 3"},
		{"user", "user msg 4"},
		{"assistant", "assistant reply 4"},
	}
	for i, exp := range expected {
		idx := 2 + i // preserved starts at index 2 (handoff folded)
		if msgs[idx].Role != exp.role {
			t.Errorf("preserved[%d].Role = %q, want %q", i, msgs[idx].Role, exp.role)
		}
		if provider.TextOf(msgs[idx].Content) != exp.text {
			t.Errorf("preserved[%d] = %q, want %q", i, provider.TextOf(msgs[idx].Content), exp.text)
		}
	}

	// Verify role alternation: every pair of consecutive messages has different roles
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == msgs[i-1].Role {
			t.Errorf("consecutive same role at [%d,%d]: %s", i-1, i, msgs[i].Role)
		}
	}
}

func TestCompactPreserveMessagesZero(t *testing.T) {
	// Verifies that setting preserveMessages to zero behaves
	// identically to the default no-preservation mode: the compacted session contains only
	// the marker, summary, and handoff — no original messages are kept — and the summary
	// does not include a preservation note.
	server := mockCompactionServer("Summary of conversation.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.WithConfig(4096, 4, 0) // preserve=0 → same as current behaviour

	_, newKey, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(newKey)
	if len(msgs) != 3 {
		t.Fatalf("after compact: %d messages, want 3 (no preserved)", len(msgs))
	}

	// Summary should NOT contain preservation note
	summaryText := provider.TextOf(msgs[1].Content)
	if strings.Contains(summaryText, "last") {
		t.Errorf("summary should not have preservation note when preserve=0: %q", summaryText)
	}
}

func TestCompactPreserveMoreThanAvailable(t *testing.T) {
	// Verifies that when preserveMessages exceeds the
	// number of messages that can be spared (i.e., leaving fewer than minMessages for
	// summarisation), the preserve count is clamped downward to the maximum feasible value
	// rather than failing or preserving everything.
	server := mockCompactionServer("Summary.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// 10 messages, minMessages=4, preserve=100
	for i := 0; i < 5; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.WithConfig(4096, 4, 100) // preserve=100 but only 10 messages

	_, newKey, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(newKey)
	// Should clamp: 10 messages, need at least 4 to summarize, so preserve = 6
	// preserved[0] is user → handoff folded into summary
	// Result: 2 (marker + summary+handoff) + 6 preserved = 8
	if len(msgs) != 8 {
		t.Fatalf("after compact: %d messages, want 8 (clamped preserve)", len(msgs))
	}
}

func TestCompactPreserveRoleAlternation(t *testing.T) {
	// Verifies that role alternation is maintained after
	// compaction regardless of whether the first preserved message is a user or assistant turn.
	// When preserved messages start with a user turn the handoff is folded into the summary;
	// when they start with an assistant turn the handoff appears as a separate user message —
	// both paths must produce a strictly alternating role sequence.
	server := mockCompactionServer("Summary.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")

	t.Run("preserved_starts_user", func(t *testing.T) {
		// Even preserve count from even total → preserved[0] is user
		store := session.NewStore(t.TempDir())
		key := "test/imain/1000000000"
		for i := 0; i < 5; i++ {
			store.TestAppend(key, provider.Message{Role: "user", Content: provider.TextContent("u")})
			store.TestAppend(key, provider.Message{Role: "assistant", Content: provider.TextContent("a")})
		}

		c := NewCompactor(store, "claude-haiku-4-5", 0.8)
		c.WithConfig(4096, 4, 4) // preserve 4 → [u3,a3,u4,a4] → starts user

		_, newKey, err := c.Compact(context.Background(), noStream(client), key, nil, "", "", false)
		if err != nil {
			t.Fatalf("Compact: %v", err)
		}

		msgs, _ := store.Load(newKey)
		// Handoff folded: 2 header + 4 preserved = 6
		if len(msgs) != 6 {
			t.Fatalf("got %d messages, want 6", len(msgs))
		}
		// Verify no consecutive same-role
		for i := 1; i < len(msgs); i++ {
			if msgs[i].Role == msgs[i-1].Role {
				t.Errorf("consecutive %s at [%d,%d]", msgs[i].Role, i-1, i)
			}
		}
		// Handoff text should be folded into the assistant summary
		if !strings.Contains(provider.TextOf(msgs[1].Content), "Compaction complete") {
			t.Errorf("summary should contain folded handoff")
		}
	})

	t.Run("preserved_starts_assistant", func(t *testing.T) {
		// Odd preserve count from even total → preserved[0] is assistant
		store := session.NewStore(t.TempDir())
		key := "test/imain/1000000000"
		for i := 0; i < 5; i++ {
			store.TestAppend(key, provider.Message{Role: "user", Content: provider.TextContent("u")})
			store.TestAppend(key, provider.Message{Role: "assistant", Content: provider.TextContent("a")})
		}

		c := NewCompactor(store, "claude-haiku-4-5", 0.8)
		c.WithConfig(4096, 4, 3) // preserve 3 → [a3,u4,a4] → starts assistant

		_, newKey, err := c.Compact(context.Background(), noStream(client), key, nil, "", "", false)
		if err != nil {
			t.Fatalf("Compact: %v", err)
		}

		msgs, _ := store.Load(newKey)
		// Standard layout: 3 header + 3 preserved = 6
		if len(msgs) != 6 {
			t.Fatalf("got %d messages, want 6", len(msgs))
		}
		// Verify no consecutive same-role
		for i := 1; i < len(msgs); i++ {
			if msgs[i].Role == msgs[i-1].Role {
				t.Errorf("consecutive %s at [%d,%d]", msgs[i].Role, i-1, i)
			}
		}
		// Handoff should be separate user message (not folded)
		if msgs[2].Role != "user" {
			t.Errorf("handoff role = %q, want user", msgs[2].Role)
		}
	})
}

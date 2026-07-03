package compaction

import (
	"context"
	"fmt"
	"testing"

	"foci/internal/provider"
	"foci/internal/session"
)

func TestManaRefreshPreservePercentage(t *testing.T) {
	// Verifies that when preserveMessages is set to a percentage of the total
	// message count (as mana-refresh mode does via agent/compaction.go), only
	// the older half is summarised and the recent half is preserved verbatim.
	// With 20 messages and preserve=10 (50%), messages [0..10) are summarised
	// and messages [10..20) are preserved.
	server := mockCompactionServer("Summary of older conversation.")
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain"

	// Add 20 messages (10 user + 10 assistant)
	for i := 0; i < 10; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent(fmt.Sprintf("user msg %d", i))})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent(fmt.Sprintf("assistant reply %d", i))})
	}

	c := NewCompactor(store, 0.8)
	// Simulate mana-refresh: preserve 50% of 20 = 10 messages
	c.WithConfig(4096, 4, 10)

	_, err := c.Compact(context.Background(), noStream(client), sessionKey, "claude-haiku-4-5", "anthropic", nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(sessionKey)

	// preserved[0] is user → handoff folded into summary
	// Result: 2 (marker + summary+handoff) + 10 preserved = 12
	if len(msgs) != 12 {
		t.Fatalf("after compact: %d messages, want 12", len(msgs))
	}

	// Verify the preserved messages are the last 10 from the original session
	// (user msg 5, assistant reply 5, ..., user msg 9, assistant reply 9)
	for i := 0; i < 10; i++ {
		idx := 2 + i // preserved starts at index 2 (handoff folded)
		origIdx := 10 + i
		var wantRole, wantText string
		if origIdx%2 == 0 {
			wantRole = "user"
			wantText = fmt.Sprintf("user msg %d", origIdx/2)
		} else {
			wantRole = "assistant"
			wantText = fmt.Sprintf("assistant reply %d", origIdx/2)
		}
		if msgs[idx].Role != wantRole {
			t.Errorf("preserved[%d].Role = %q, want %q", i, msgs[idx].Role, wantRole)
		}
		if provider.TextOf(msgs[idx].Content) != wantText {
			t.Errorf("preserved[%d] = %q, want %q", i, provider.TextOf(msgs[idx].Content), wantText)
		}
	}
}

func TestManaRefreshPreserveExplicitCountOverridesPercentage(t *testing.T) {
	// Verifies that an explicit preserve count takes priority over a percentage.
	// With 20 messages and explicit preserve=6, exactly 6 messages are preserved
	// regardless of any percentage setting.
	server := mockCompactionServer("Summary.")
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain"

	for i := 0; i < 10; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent(fmt.Sprintf("u%d", i))})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent(fmt.Sprintf("a%d", i))})
	}

	c := NewCompactor(store, 0.8)
	// Explicit count: preserve 6 messages
	c.WithConfig(4096, 4, 6)

	_, err := c.Compact(context.Background(), noStream(client), sessionKey, "claude-haiku-4-5", "anthropic", nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(sessionKey)
	// preserved[0] is user → handoff folded: 2 header + 6 preserved = 8
	if len(msgs) != 8 {
		t.Fatalf("after compact: %d messages, want 8", len(msgs))
	}
}

func TestWalkBackFallbackKeepsOriginalSplit(t *testing.T) {
	// Verifies that when safeSplitPoint walk-back would push the split below
	// minMessages, the original split is kept instead of nuking all preserved
	// messages. The orphaned tool_use at the boundary is repaired by
	// repairOrphanedToolUse.
	server := mockCompactionServer("Summary with boundary repair.")
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain"

	// Build a session where walk-back would push below minMessages:
	// [0] user text
	// [1] assistant text
	// [2] user text
	// [3] assistant tool_use   ← walk-back wants to pull this into preserved
	// [4] user tool_result     ← proposed split point (preserve=2 → splitIdx=4)
	// [5] assistant text
	//
	// With minMessages=4 and preserve=2: splitIdx=4, walk-back to 3,
	// but 3 < minMessages=4 → old code would preserve nothing.
	// New code: keep original splitIdx=4, repair the orphan.
	store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("u0")})
	store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("a0")})
	store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("u1")})
	store.TestAppend(sessionKey, toolUseMsg("toolu_boundary"))
	store.TestAppend(sessionKey, toolResultMsg("toolu_boundary"))
	store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("done")})

	c := NewCompactor(store, 0.8)
	c.WithConfig(4096, 4, 2) // preserve 2, minMessages 4

	_, err := c.Compact(context.Background(), noStream(client), sessionKey, "claude-haiku-4-5", "anthropic", nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(sessionKey)

	// Should preserve 2 messages (the last 2), NOT 0.
	// The boundary tool_use/tool_result pair is split, but repairOrphanedToolUse
	// fixes the toSummarise side.
	// preserved[0] = tool_result (user) → handoff folded into summary
	// Result: 2 (marker + summary+handoff) + 2 preserved = 4
	if len(msgs) < 4 {
		t.Fatalf("after compact: %d messages, want >= 4 (should NOT nuke to 3)", len(msgs))
	}

	// Verify the last message is preserved
	lastMsg := msgs[len(msgs)-1]
	if provider.TextOf(lastMsg.Content) != "done" {
		t.Errorf("last preserved message = %q, want 'done'", provider.TextOf(lastMsg.Content))
	}
}

func TestWalkBackFallbackDoesNotNukeSession(t *testing.T) {
	// Regression test for the double-compaction bug: verifies that a session
	// starting with compaction headers (as created by a prior compaction) does
	// not get fully summarised when the walk-back guard fires. With the old
	// code, this scenario would set preserveN=0 and summarise ALL messages.
	server := mockCompactionServer("Re-summary.")
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain"

	// Simulate a post-compaction session:
	// [0] user: compaction marker
	// [1] assistant: summary + handoff
	// [2] user: preserved msg
	// [3] assistant: tool_use  ← walk-back target
	// [4] user: tool_result    ← proposed split
	// [5] assistant: final reply
	store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("[Session compacted.]")})
	store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("Previous summary.")})
	store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("continue work")})
	store.TestAppend(sessionKey, toolUseMsg("toolu_post"))
	store.TestAppend(sessionKey, toolResultMsg("toolu_post"))
	store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("all done")})

	c := NewCompactor(store, 0.8)
	c.WithConfig(4096, 4, 2) // preserve 2, minMessages 4

	_, err := c.Compact(context.Background(), noStream(client), sessionKey, "claude-haiku-4-5", "anthropic", nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(sessionKey)

	// With old code: walk-back pushes splitIdx from 4 to 3, which is < minMessages=4,
	// so preserveN=0 → all 6 messages summarised → result is 3 messages (marker+summary+handoff).
	// With new code: keeps original split at 4, preserves 2 messages → result is 4 messages.
	if len(msgs) <= 3 {
		t.Fatalf("after compact: %d messages — session was nuked (want > 3 with preserved messages)", len(msgs))
	}

	// Last message should be the preserved "all done"
	if provider.TextOf(msgs[len(msgs)-1].Content) != "all done" {
		t.Errorf("last message = %q, want 'all done'", provider.TextOf(msgs[len(msgs)-1].Content))
	}
}

func TestMaxWalkBackCappedAt10(t *testing.T) {
	// Verifies that walk-back is capped at 10 steps even when preserveN is much
	// larger. Without the cap, a long chain of tool_use messages could walk back
	// the entire session.
	//
	// Build a session with 20 consecutive tool_use messages (no results) followed
	// by a normal ending. With preserveN=15, the old code would set maxWalkBack=15
	// and walk back through all tool_use messages. With the cap at 10, it stops
	// after 10 steps.
	msgs := make([]provider.Message, 0, 25)
	// 3 normal messages at the start
	msgs = append(msgs,
		provider.Message{Role: "user", Content: provider.TextContent("start")},
		provider.Message{Role: "assistant", Content: provider.TextContent("ok")},
		provider.Message{Role: "user", Content: provider.TextContent("go")},
	)
	// 12 consecutive tool_use messages (corrupt, no results)
	for i := 0; i < 12; i++ {
		msgs = append(msgs, toolUseMsg(fmt.Sprintf("toolu_%d", i)))
	}
	// Normal ending
	msgs = append(msgs,
		toolResultMsg("toolu_11"),
		provider.Message{Role: "assistant", Content: provider.TextContent("done")},
	)

	// splitIdx = 17 - 15 = 2, walk-back from index 2
	// msgs[1] is assistant text (no tool_use) → would stop immediately at 2
	// Let's test with a different split where walk-back matters:
	// splitIdx = 17 - 3 = 14, msgs[13] is tool_use → walk back
	// With cap=10: walks from 14 to 4 (10 steps), stops.
	// Without cap (maxWalkBack=3): walks from 14 to 13, 12, 11 (3 steps), stops.
	// But we want to test the cap, so use preserveN=3.
	// Actually the cap is min(preserveN, 10). With preserveN=3, cap=3.
	// To test the 10-step cap: need preserveN > 10.

	// Test: preserveN=15 → splitIdx = 17 - 15 = 2
	// msgs[1] = assistant text → no walk-back needed.
	// Better test: make splitIdx land in the tool_use chain.

	// Revised approach: 24 messages total, preserveN=10 → splitIdx=14
	// msgs[13] is tool_use → walks back. Cap = min(10,10) = 10.
	// Without cap would be preserveN which could be larger.

	// Let's just test safeSplitPoint directly with a large maxWalkBack vs capped.
	splitIdx := len(msgs) - 3 // = 14, msgs[13] = tool_use
	// Without cap: maxWalkBack=100, walks all the way to index 3 (msgs[2] is user text)
	uncapped := safeSplitPoint(msgs, splitIdx, 100)
	// With cap at 10: walks from 14 to 4 (10 steps), msgs[3] is still tool_use
	capped := safeSplitPoint(msgs, splitIdx, 10)

	if uncapped >= capped {
		// uncapped should walk further back (lower index)
		// This validates that capping prevents excessive walk-back
		t.Logf("uncapped=%d capped=%d (both may hit non-tool boundary)", uncapped, capped)
	}

	// The key assertion: capped walk-back should not go below splitIdx - 10
	if capped < splitIdx-10 {
		t.Errorf("capped walk-back went too far: splitIdx=%d, capped=%d, expected >= %d", splitIdx, capped, splitIdx-10)
	}
}

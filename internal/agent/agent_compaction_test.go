package agent

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"foci/internal/provider"
	"foci/internal/memory"
	"foci/internal/tools"
	"foci/internal/warnings"
)

func TestAgentCompactionIntegration(t *testing.T) {
	t.Run("basic", func(t *testing.T) {
		var turnCount atomic.Int32
		env := newCompactionTestEnv(t, &turnCount, 5)
		sessionKey := "test/icompact/1000000000"

		// Phase 1: 4 turns with low tokens — no compaction
		env.runTurns(t, sessionKey, 1, 4)

		msgs, _ := env.store.Load(sessionKey)
		if len(msgs) != 8 {
			t.Fatalf("after 4 turns: %d messages, want 8", len(msgs))
		}

		// Phase 2: Turn 5 — high tokens triggers compaction
		env.runTurns(t, sessionKey, 5, 5)

		// After compaction, session should have exactly 3 messages
		msgs, _ = env.store.Load(sessionKey)
		if len(msgs) != 3 {
			t.Fatalf("after compaction: %d messages, want 3", len(msgs))
		}

		// msg[0]: marker
		if !strings.Contains(provider.TextOf(msgs[0].Content), "compacted") {
			t.Errorf("msg[0] should contain 'compacted': %q", provider.TextOf(msgs[0].Content))
		}
		// msg[1]: summary from mock
		if !strings.Contains(provider.TextOf(msgs[1].Content), "compacted summary") {
			t.Errorf("msg[1] should contain summary: %q", provider.TextOf(msgs[1].Content))
		}
		// msg[2]: handoff
		if !strings.Contains(provider.TextOf(msgs[2].Content), "Compaction complete") {
			t.Errorf("msg[2] should contain handoff: %q", provider.TextOf(msgs[2].Content))
		}

		// Phase 3: Turn 6 — post-compaction continuity
		env.runTurns(t, sessionKey, 6, 6)

		// 3 compacted + user turn 6 + assistant turn 6 = 5
		msgs, _ = env.store.Load(sessionKey)
		if len(msgs) != 5 {
			t.Fatalf("after Turn 6: %d messages, want 5", len(msgs))
		}
	})

	t.Run("scratchpad", func(t *testing.T) {
		var turnCount atomic.Int32
		env := newCompactionTestEnv(t, &turnCount, 5)

		// Set up scratchpad with entries
		scratchpad, err := memory.NewScratchpad(filepath.Join(t.TempDir(), "scratchpad.db"))
		if err != nil {
			t.Fatalf("create scratchpad: %v", err)
		}
		defer scratchpad.Close()

		if err := scratchpad.Write("test", "current_task", "implementing feature X"); err != nil {
			t.Fatalf("write scratchpad: %v", err)
		}
		if err := scratchpad.Write("test", "blockers", "need API key for auth"); err != nil {
			t.Fatalf("write scratchpad: %v", err)
		}

		env.compactor.Scratchpad = scratchpad
		env.compactor.AgentID = "test"

		sessionKey := "test/icompactsp/1000000000"

		// Build up 4 turns then trigger compaction on turn 5
		env.runTurns(t, sessionKey, 1, 5)

		msgs, _ := env.store.Load(sessionKey)
		if len(msgs) != 3 {
			t.Fatalf("after compaction: %d messages, want 3", len(msgs))
		}

		// Verify handoff message contains scratchpad data
		handoff := provider.TextOf(msgs[2].Content)
		if !strings.Contains(handoff, "scratchpad") {
			t.Errorf("handoff should mention scratchpad: %q", handoff)
		}
		if !strings.Contains(handoff, "current_task") {
			t.Errorf("handoff should contain key 'current_task': %q", handoff)
		}
		if !strings.Contains(handoff, "implementing feature X") {
			t.Errorf("handoff should contain scratchpad value: %q", handoff)
		}
		if !strings.Contains(handoff, "blockers") {
			t.Errorf("handoff should contain key 'blockers': %q", handoff)
		}
		if !strings.Contains(handoff, "need API key for auth") {
			t.Errorf("handoff should contain scratchpad value: %q", handoff)
		}
	})

	t.Run("preserve", func(t *testing.T) {
		var turnCount atomic.Int32
		env := newCompactionTestEnv(t, &turnCount, 5)
		env.compactor.WithConfig(4096, 4, 4) // preserve last 4 messages

		sessionKey := "test/ipreserve/1000000000"

		// Phase 1: 4 turns with low tokens — no compaction
		env.runTurns(t, sessionKey, 1, 4)

		msgs, _ := env.store.Load(sessionKey)
		if len(msgs) != 8 {
			t.Fatalf("after 4 turns: %d messages, want 8", len(msgs))
		}

		// Phase 2: Turn 5 — high tokens triggers compaction
		env.runTurns(t, sessionKey, 5, 5)

		// After compaction with preserve=4, preserved[0] is user so handoff folds:
		// 2 (marker + summary+handoff) + 4 preserved = 6
		msgs, _ = env.store.Load(sessionKey)
		if len(msgs) != 6 {
			t.Fatalf("after compaction: %d messages, want 6 (2 header + 4 preserved)", len(msgs))
		}

		// Verify role alternation (the fix ensures no consecutive same-role)
		for i := 1; i < len(msgs); i++ {
			if msgs[i].Role == msgs[i-1].Role {
				t.Errorf("consecutive same role at [%d,%d]: %s", i-1, i, msgs[i].Role)
			}
		}

		// Verify the preserved messages are the last 4 from pre-compaction
		// Pre-compaction had 10 messages: [u1,a1,u2,a2,u3,a3,u4,a4,u5,a5]
		// Last 4: [u4,a4,u5,a5]
		preserved := msgs[2:] // preserved starts at index 2 (handoff folded)
		if len(preserved) != 4 {
			t.Fatalf("preserved = %d messages, want 4", len(preserved))
		}
		if preserved[0].Role != "user" {
			t.Errorf("preserved[0].Role = %q, want user", preserved[0].Role)
		}
		if preserved[1].Role != "assistant" {
			t.Errorf("preserved[1].Role = %q, want assistant", preserved[1].Role)
		}
		// Verify content of preserved messages (Turn 4 user msg has metadata prefix, so check contains)
		if !strings.Contains(provider.TextOf(preserved[0].Content), "Turn 4") {
			t.Errorf("preserved[0] should contain 'Turn 4': %q", provider.TextOf(preserved[0].Content))
		}
		if provider.TextOf(preserved[1].Content) != "Response 4" {
			t.Errorf("preserved[1] = %q, want 'Response 4'", provider.TextOf(preserved[1].Content))
		}

		// Summary+handoff should mention preservation and contain handoff text
		summaryText := provider.TextOf(msgs[1].Content)
		if !strings.Contains(summaryText, "last 4 messages") {
			t.Errorf("summary missing preservation note: %q", summaryText)
		}
		if !strings.Contains(summaryText, "Compaction complete") {
			t.Errorf("summary should contain folded handoff: %q", summaryText)
		}

		// Phase 3: Turn 6 — post-compaction continuity (messages survive reload)
		env.runTurns(t, sessionKey, 6, 6)

		// 6 compacted + user turn 6 + assistant turn 6 = 8
		msgs, _ = env.store.Load(sessionKey)
		if len(msgs) != 8 {
			t.Fatalf("after Turn 6: %d messages, want 8", len(msgs))
		}

		// The preserved messages should still be at positions 2-5
		if !strings.Contains(provider.TextOf(msgs[2].Content), "Turn 4") {
			t.Errorf("preserved msg should survive post-compaction turn: %q", provider.TextOf(msgs[2].Content))
		}
	})

	t.Run("notify", func(t *testing.T) {
		var turnCount atomic.Int32
		env := newCompactionTestEnv(t, &turnCount, 5)

		var notified []string
		env.ag.CompactionNotifyFunc = func(session string, msg string) {
			notified = append(notified, msg)
		}

		sessionKey := "test/icompactnotify/1000000000"

		// 4 turns, then turn 5 triggers compaction
		env.runTurns(t, sessionKey, 1, 5)

		if len(notified) != 2 {
			t.Fatalf("expected 2 notifications (start + end), got %d", len(notified))
		}
		if !strings.Contains(notified[0], "Compacting") {
			t.Errorf("start notification = %q, want to contain 'Compacting'", notified[0])
		}
		if !strings.Contains(notified[1], "10 messages") {
			t.Errorf("end notification = %q, want to contain '10 messages'", notified[1])
		}
	})

	t.Run("no_compact", func(t *testing.T) {
		var turnCount atomic.Int32
		env := newCompactionTestEnv(t, &turnCount, 5)

		var notified []string
		warnQ := warnings.NewQueue(0, 0)
		env.ag.Warnings = warnQ
		env.ag.CompactionNotifyFunc = func(session string, msg string) {
			notified = append(notified, msg)
		}

		sessionKey := "test/inocompact/1000000000"

		// 4 normal turns
		env.runTurns(t, sessionKey, 1, 4)

		// Turn 5 triggers compaction threshold — but with NoCompact set
		env.ag.SetSessionNoCompact(sessionKey, true)
		resp, err := env.ag.HandleMessage(context.Background(), sessionKey, "Turn 5")
		if err != nil {
			t.Fatalf("Turn 5: %v", err)
		}

		// Should still get a response
		if resp != "Response 5" {
			t.Errorf("got %q, want %q", resp, "Response 5")
		}

		// Compaction should NOT have fired
		if len(notified) != 0 {
			t.Errorf("expected 0 notifications with no_compact, got %d", len(notified))
		}

		// Session should still have all original messages (not compacted)
		msgs, err := env.store.Load(sessionKey)
		if err != nil {
			t.Fatalf("load session: %v", err)
		}
		// 5 turns × 2 messages each = 10
		if len(msgs) != 10 {
			t.Errorf("expected 10 messages (uncompacted), got %d", len(msgs))
		}

		// No warning should be pushed for no_compact sessions (removed in 63f8f6b2)
		warned := warnQ.Drain()
		if len(warned) != 0 {
			t.Fatalf("expected 0 warnings for no_compact session, got %d: %v", len(warned), warned)
		}
	})

	t.Run("skipped_when_async_pending", func(t *testing.T) {
		var turnCount atomic.Int32
		env := newCompactionTestEnv(t, &turnCount, 5)

		notifier := tools.NewAsyncNotifier(func(sk, msg string, replyTo string) {})
		env.ag.AsyncNotifier = notifier
		var notified []string
		env.ag.CompactionNotifyFunc = func(session string, msg string) {
			notified = append(notified, msg)
		}

		sessionKey := "test/iasyncpending/1000000000"

		// 4 normal turns
		env.runTurns(t, sessionKey, 1, 4)

		// Mark an async result as pending for this session
		notifier.MarkPending(sessionKey)

		// Turn 5 triggers compaction threshold — but async pending blocks it
		resp, err := env.ag.HandleMessage(context.Background(), sessionKey, "Turn 5")
		if err != nil {
			t.Fatalf("Turn 5: %v", err)
		}
		if resp != "Response 5" {
			t.Errorf("got %q, want %q", resp, "Response 5")
		}

		// Compaction should NOT have fired
		if len(notified) != 0 {
			t.Errorf("expected 0 compaction notifications with async pending, got %d", len(notified))
		}

		// Session should still have all original messages
		msgs, err := env.store.Load(sessionKey)
		if err != nil {
			t.Fatalf("load session: %v", err)
		}
		if len(msgs) != 10 {
			t.Errorf("expected 10 messages (uncompacted), got %d", len(msgs))
		}

		// Clear pending — next turn should compact
		notifier.MarkDone(sessionKey)
		turnCount.Store(4) // reset so turn 6 = count 5 → high tokens

		_, err = env.ag.HandleMessage(context.Background(), sessionKey, "Turn 6")
		if err != nil {
			t.Fatalf("Turn 6: %v", err)
		}

		// Compaction should have fired now
		if len(notified) == 0 {
			t.Error("expected compaction to fire after async pending cleared")
		}

		msgs, _ = env.store.Load(sessionKey)
		if len(msgs) > 5 {
			t.Errorf("expected compacted session (<=5 messages), got %d", len(msgs))
		}
	})
}


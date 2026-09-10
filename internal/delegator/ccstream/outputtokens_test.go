package ccstream

import "testing"

// CC delivers one line per content block, each repeating the message's usage.
// Three of the four classes are final on the first line; OUTPUT is a running
// count that starts at 1-3 and is completed only on the line carrying a non-nil
// stop_reason. Measured 2026-09-10 on a subagent transcript: across 29 message
// ids, input/cache_read/cache_write varied on ZERO and output varied on 26.
//
// The accumulator kept the FIRST delivery and ignored the rest, so it locked in
// the placeholder and discarded the real figure — 88.6% of subagent output
// tokens on this host, and output is the most expensive class.

func outMsg(id string, out int) *AssistantMessage {
	m := &AssistantMessage{}
	m.Message.ID = id
	m.Message.Model = "claude-opus-5"
	m.Message.Usage = TokenUsage{OutputTokens: out}
	return m
}

func TestNote_OutputRisesToTheCompletedFigure(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	// The exact shape observed on disk: placeholder, placeholder, real.
	b.noteAssistantUsage(outMsg("msg_A", 1))
	b.noteAssistantUsage(outMsg("msg_A", 1))
	b.noteAssistantUsage(outMsg("msg_A", 185))

	tu := b.turnUsageAcc.top["claude-opus-5"]
	if tu == nil {
		t.Fatal("no bucket for the model")
	}
	if tu.Output != 185 {
		t.Fatalf("Output = %d, want 185 — the completed figure must replace the placeholder", tu.Output)
	}
}

func TestNote_RepeatedIdenticalDeliveryDoesNotInflate(t *testing.T) {
	t.Parallel()
	// The property the old first-wins rule existed to protect: a three-block
	// message arriving three times with identical counts must be counted once.
	b := &Backend{}
	for i := 0; i < 3; i++ {
		b.noteAssistantUsage(msgWith("msg_A", "", 8320, 0, 8320))
	}
	if got := b.turnUsageAcc.writeSplit(false).Ephemeral1h; got != 8320 {
		t.Fatalf("1h = %d, want 8320 — repeated blocks must not multiply", got)
	}
}

func TestNote_StableClassesUnaffectedByRepeats(t *testing.T) {
	t.Parallel()
	// Input, cache-read and cache-write are already final on the first line, so
	// raising to the max must be a no-op for them.
	b := &Backend{}
	m := &AssistantMessage{}
	m.Message.ID = "msg_A"
	m.Message.Model = "claude-opus-5"
	m.Message.Usage = TokenUsage{InputTokens: 116, CacheReadInputTokens: 2410348, OutputTokens: 1}
	b.noteAssistantUsage(m)
	b.noteAssistantUsage(m)

	tu := b.turnUsageAcc.top["claude-opus-5"]
	if tu.Input != 116 {
		t.Errorf("Input = %d, want 116 (not doubled)", tu.Input)
	}
	if tu.CacheRead != 2410348 {
		t.Errorf("CacheRead = %d, want 2410348 (not doubled)", tu.CacheRead)
	}
}

func TestNote_OutOfOrderDeliveryKeepsTheHighWaterMark(t *testing.T) {
	t.Parallel()
	// The fix must not depend on the completed line arriving last. A tail that
	// resumes mid-file, or a drain that interleaves, can deliver them in any
	// order; a "last-wins" rule would then overwrite the real figure with a
	// placeholder and silently under-charge.
	b := &Backend{}
	b.noteAssistantUsage(outMsg("msg_A", 185))
	b.noteAssistantUsage(outMsg("msg_A", 1))

	if got := b.turnUsageAcc.top["claude-opus-5"].Output; got != 185 {
		t.Fatalf("Output = %d, want 185 — a later placeholder must not lower the mark", got)
	}
}

func TestNote_HighWaterMarkIsPerMessageNotPerModel(t *testing.T) {
	t.Parallel()
	// Two distinct messages on one model must SUM, not collapse to the larger.
	b := &Backend{}
	b.noteAssistantUsage(outMsg("msg_A", 185))
	b.noteAssistantUsage(outMsg("msg_B", 356))

	if got := b.turnUsageAcc.top["claude-opus-5"].Output; got != 541 {
		t.Fatalf("Output = %d, want 541 — distinct messages must sum", got)
	}
}

// TestNote_WritesNotDoubledWhenOutputRises is the production shape, and the one
// the other tests could not reach.
//
// A repeat with IDENTICAL usage short-circuits on `cur == prev` and never runs
// the increment arithmetic, so a test built from identical repeats pins the
// short-circuit rather than the maths. The real deliveries differ — output
// rises from a placeholder to the completed figure while cache writes stay
// flat — which is the only path that exercises the per-class increment. A
// fail-arm adding the full value instead of the increase reddened nothing until
// this test existed.
func TestNote_WritesNotDoubledWhenOutputRises(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	mk := func(out int) *AssistantMessage {
		m := &AssistantMessage{}
		m.Message.ID = "msg_A"
		m.Message.Model = "claude-opus-5"
		m.Message.Usage = TokenUsage{
			OutputTokens:             out,
			CacheCreationInputTokens: 8320,
			CacheReadInputTokens:     2410348,
			InputTokens:              116,
			CacheCreation:            &CacheCreationSplit{Ephemeral1h: 8320},
		}
		return m
	}
	b.noteAssistantUsage(mk(1))
	b.noteAssistantUsage(mk(185)) // same message, output completed

	tu := b.turnUsageAcc.top["claude-opus-5"]
	if tu.Output != 185 {
		t.Errorf("Output = %d, want 185", tu.Output)
	}
	if got := tu.Write.Ephemeral1h; got != 8320 {
		t.Errorf("1h = %d, want 8320 — a flat class must not re-add when another class rises", got)
	}
	if tu.CacheRead != 2410348 {
		t.Errorf("CacheRead = %d, want 2410348 — must not re-add", tu.CacheRead)
	}
	if tu.Input != 116 {
		t.Errorf("Input = %d, want 116 — must not re-add", tu.Input)
	}
}

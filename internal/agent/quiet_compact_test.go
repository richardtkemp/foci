package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/compaction"
	"foci/internal/delegator"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// Tests for Agent.QuietCompact (#2218): the scheduled quiet-hours trigger
// runs the exact /compact pipeline (compactSession), behind a guard set that
// is deliberately stricter than /compact's — never mid-turn, never with
// queued input, and never with ANY background work, even the kind /compact
// tolerates (unchanged prompt, no restart).

const quietGuardSK = "test-agent/c1"

func TestQuietCompactBlocked_AllGuards(t *testing.T) {
	// Proves the one guard set behind quiet compaction, leg by leg: a turn
	// in flight, a compaction in flight, queued inbox input, and background
	// work each refuse with a naming reason; a clear session allows "".
	//
	// Background work is exercised on a delegated agent with an UNCHANGED
	// prompt — the case where /compact proceeds (see
	// TestCompactSession_UnchangedPromptCompactsDespiteSubagents) but the
	// quiet trigger must not (stricter rule, #2218).
	ag, be, sent := bgCompactAgent(t, quietGuardSK)

	// All clear: no background work; a changed prompt alone blocks nothing.
	be.set(nil, false)
	if got := ag.QuietCompactBlocked(quietGuardSK); got != "" {
		t.Errorf("clear session: QuietCompactBlocked = %q, want \"\"", got)
	}

	// Leg 1: turn in flight.
	ag.SetTurnInFlightForTest(quietGuardSK, true)
	if got := ag.QuietCompactBlocked(quietGuardSK); got != "turn in flight" {
		t.Errorf("turn in flight: QuietCompactBlocked = %q, want \"turn in flight\"", got)
	}
	ag.SetTurnInFlightForTest(quietGuardSK, false)

	// Leg 2: compaction in flight.
	ag.markCompacting(quietGuardSK)
	if got := ag.QuietCompactBlocked(quietGuardSK); got != "compaction in flight" {
		t.Errorf("compacting: QuietCompactBlocked = %q, want \"compaction in flight\"", got)
	}
	ag.clearCompacting(quietGuardSK)

	// Leg 3: queued inbox input (a buffered steer text).
	inb := ag.getOrCreateInbox(quietGuardSK)
	inb.appendSteer("queued follow-up", time.Now())
	if got := ag.QuietCompactBlocked(quietGuardSK); got != "inbox input queued" {
		t.Errorf("inbox queued: QuietCompactBlocked = %q, want \"inbox input queued\"", got)
	}
	inb.drainSteer()

	// Leg 4: background work — with the prompt UNCHANGED, where /compact
	// would proceed.
	ag.DelegatedManager.StartOpts.SystemPromptFunc = func(string) string { return "v1" } // as launched
	if ag.BackgroundWorkRunning(quietGuardSK) {
		t.Fatal("fixture: /compact would be refused — the prompt must be unchanged here")
	}
	be.set([]delegator.RunningSubagent{{ID: "t1", Description: "review", Kind: delegator.SubagentKindAgent}}, true)
	if got := ag.QuietCompactBlocked(quietGuardSK); got == "" {
		t.Error("background work with an unchanged prompt: QuietCompactBlocked = \"\", want a reason (stricter than /compact)")
	}
	if len(*sent) != 0 {
		t.Errorf("guard check sent %q to the backend — it must be side-effect free", *sent)
	}
}

func TestQuietCompact_RefusesMidTurn(t *testing.T) {
	// Proves QuietCompact refuses while a turn is in flight on the session,
	// before anything else runs (here: no Compactor is wired at all, so a
	// non-refusal path would fail loudly with "not configured").
	ag := &Agent{}
	ag.SetTurnInFlightForTest("test/ibusy", true)

	err := ag.QuietCompact(context.Background(), "test/ibusy")
	var refused *QuietCompactRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("QuietCompact err = %v, want *QuietCompactRefusedError", err)
	}
	if refused.Reason != "turn in flight" {
		t.Errorf("refusal reason = %q, want \"turn in flight\"", refused.Reason)
	}
}

func TestQuietCompact_RefusesBackgroundWorkDespiteUnchangedPrompt(t *testing.T) {
	// The deliberate divergence from /compact (#2218): with background work
	// running and the prompt UNCHANGED — a restart would kill nothing, so
	// CompactSession proceeds (pinned by
	// TestCompactSession_UnchangedPromptCompactsDespiteSubagents) — the
	// quiet trigger still refuses and sends nothing to CC.
	const sk = "test-agent/c1"
	ag, be, sent := bgCompactAgent(t, sk)
	ag.DelegatedManager.StartOpts.SystemPromptFunc = func(string) string { return "v1" } // as launched

	if _, err := ag.CompactSession(context.Background(), sk, false); err != nil {
		t.Fatalf("fixture: CompactSession = %v, want it to proceed (unchanged prompt)", err)
	}

	err := ag.QuietCompact(context.Background(), sk)
	var refused *QuietCompactRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("QuietCompact err = %v, want *QuietCompactRefusedError (background work must refuse)", err)
	}
	if len(*sent) != 1 { // only the /compact above
		t.Errorf("QuietCompact sent to the backend: %q", *sent)
	}
	if be.wasClosed() {
		t.Error("refused quiet compaction closed the backend")
	}
}

func TestQuietCompact_APIPath_RunsCompactionPipeline(t *testing.T) {
	// Proves QuietCompact on an API agent runs the REAL compactSession
	// path: the session is compacted in place under the same key, the
	// pre-compaction memory hook fires, and the notify hooks fire exactly as
	// for a manual /compact.
	var turnCount atomic.Int32
	client := compactionTestClient(&turnCount, -1)

	store := session.NewStore(t.TempDir())
	compactor := compaction.NewCompactor(store, 0.8)
	const sk = "test/iquiet"

	// 6 messages (3 turns) so the >=5 gate passes.
	seedSession(t, store, sk, 3)

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: workspace.NewBootstrap(t.TempDir(), []string{}),
		Compactor: compactor,
		Model:     "claude-haiku-4-5",
	}

	var memoryFiredFor string
	ag.CompactionMemoryFunc.Add(func(sessionKey string) { memoryFiredFor = sessionKey })
	var notifyMsgs []string
	ag.CompactionNotifyFunc.Add(func(sessionKey, msg, summary string) {
		notifyMsgs = append(notifyMsgs, msg)
	})

	if err := ag.QuietCompact(context.Background(), sk); err != nil {
		t.Fatalf("QuietCompact: %v", err)
	}
	if memoryFiredFor != sk {
		t.Errorf("memory hook fired for %q, want %q (the /compact pipeline fires it)", memoryFiredFor, sk)
	}
	// The compacted history (marker + summary + handoff) loads under the
	// stable key — same shape as TestCompactSession_HappyPath.
	msgs, _ := store.Load(sk)
	if len(msgs) != 3 {
		t.Errorf("after quiet compaction: %d messages, want 3", len(msgs))
	}
	if len(notifyMsgs) != 1 {
		t.Errorf("compaction notices = %d, want 1 (the pipeline's own hooks)", len(notifyMsgs))
	}
}

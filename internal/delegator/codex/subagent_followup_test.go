package codex

import (
	"encoding/json"
	"testing"

	"foci/internal/delegator"
)

// Codex 0.145.0 delivers the ENTIRE subAgentActivity lifecycle on
// item/completed — started, interacted and interrupted alike. item/started
// carries only agentMessage, reasoning and userMessage items. Verified with a
// live app-server across a spawn -> follow-up message -> close sequence; the
// payloads below are that run's, with only the ids shortened:
//
//	item/completed subAgentActivity kind=started     agentThreadId=019fa44a-… agentPath=/root/scribe
//	item/completed subAgentActivity kind=interacted  agentThreadId=019fa44a-…
//	item/completed subAgentActivity kind=interrupted agentThreadId=019fa44a-…
//
// Handling kind=started only in onItemStarted therefore opened no run at all:
// no poll, no OnSubagentStart, and the later stop() found nothing to end — the
// whole subagent display was inert, and so was every mechanism layered on it
// (the #1571 cursor, the #1576 prompt queue).
//
// Both notifications are handled now rather than swapping one for the other:
// start() is idempotent for an already-active child, so a codex version that
// emits on item/started (or on both) behaves identically, and foci stops
// depending on which one a given release happens to use.
func TestSubagentStartArrivesOnItemCompleted(t *testing.T) {
	b := newTestBackend(t)
	b.subagents = newSubagentTracker()
	ev := captureSubagentEvents(b)

	b.onItemCompleted(&itemCompletedParams{Item: activityItem("call_AwOX4DcY", "started", "019fa44a-44b3")})

	if len(ev.starts) != 1 {
		t.Fatalf("starts = %+v, want 1 — codex delivers kind=started on item/completed, so a handler that only watches item/started never opens the run", ev.starts)
	}
	if ev.starts[0].run != 1 {
		t.Errorf("run = %d, want 1", ev.starts[0].run)
	}

	// The rest of the lifecycle must still close that same run.
	b.onItemCompleted(&itemCompletedParams{Item: activityItem("call_uVPmpLSB", "interrupted", "019fa44a-44b3")})
	if len(ev.ends) != 1 || ev.ends[0].group != ev.starts[0].group {
		t.Errorf("ends = %+v, want one end matching the started run %q", ev.ends, ev.starts[0].group)
	}
}

// Whichever notification a codex release uses, exactly one run must open —
// never two. start()'s already-active guard is what makes handling both safe.
func TestSubagentStartOnBothNotificationsOpensOneRun(t *testing.T) {
	b := newTestBackend(t)
	b.subagents = newSubagentTracker()
	ev := captureSubagentEvents(b)

	b.onItemStarted(&itemStartedParams{Item: activityItem("call_dup", "started", "agent-x")})
	b.onItemCompleted(&itemCompletedParams{Item: activityItem("call_dup", "started", "agent-x")})

	if len(ev.starts) != 1 {
		t.Fatalf("starts = %+v, want exactly 1 — a release emitting both notifications must not open two runs", ev.starts)
	}
	b.subagents.stopAll()
}

// subagentEvents captures the three subagent callbacks for assertions.
type subagentEvents struct {
	starts  []subagentEvent
	prompts []subagentEvent
	texts   []subagentEvent
	ends    []subagentEvent
}

type subagentEvent struct {
	group, prompt string
	run           int
}

func captureSubagentEvents(b *Backend) *subagentEvents {
	ev := &subagentEvents{}
	b.AttachSessionEvents(&delegator.SessionEvents{
		OnSubagentStart: func(g, _, p string, r int) {
			ev.starts = append(ev.starts, subagentEvent{g, p, r})
		},
		OnSubagentPrompt: func(g, p string, r int) {
			ev.prompts = append(ev.prompts, subagentEvent{g, p, r})
		},
		OnSubagentText: func(g, text string, r int) {
			ev.texts = append(ev.texts, subagentEvent{g, text, r})
		},
		OnSubagentEnd: func(g string, r int) {
			ev.ends = append(ev.ends, subagentEvent{g, "", r})
		},
	})
	return ev
}

func activityItem(id, kind, thread string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"type": "subAgentActivity", "id": id, "kind": kind,
		"agentPath": "worker", "agentThreadId": thread,
	})
	return raw
}

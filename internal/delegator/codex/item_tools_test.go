package codex

import (
	"testing"

	"foci/internal/delegator"
)

type toolEvent struct {
	kind, id, name, detail string
	isError                bool
}

func captureToolEvents(b *Backend) *[]toolEvent {
	var evs []toolEvent
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnToolStart: func(id, name, input string) {
			evs = append(evs, toolEvent{kind: "start", id: id, name: name, detail: input})
		},
		OnToolEnd: func(id, name, output string, isError bool) {
			evs = append(evs, toolEvent{kind: "end", id: id, name: name, isError: isError})
		},
	})
	return &evs
}

// TestItem_SleepShowsAsClockSleepTool (#2955): codex's clock.sleep tool sends
// a "sleep" item on item/started and item/completed (live probe, codex
// 0.167.0 on the work Mac; schema SleepThreadItem {id, durationMs}; the
// rollout file spells the same item {"type":"Extension","kind":"clock.sleep"}).
// foci had no case for it, so a 60s sleep showed as "thinking" throughout.
func TestItem_SleepShowsAsClockSleepTool(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.itemCache = make(map[string]itemEnvelope)
	evs := captureToolEvents(b)

	b.dispatch([]byte(`{"method":"item/started","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"sleep","id":"call_s","durationMs":45000}}}`))
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"sleep","id":"call_s","durationMs":45000}}}`))

	want := []toolEvent{
		{kind: "start", id: "call_s", name: "clock.sleep", detail: "45s"},
		{kind: "end", id: "call_s", name: "clock.sleep"},
	}
	assertToolEvents(t, *evs, want)
}

// TestItem_UnknownTypeShowsAsToolByTypeName (#2955): an item type foci has no
// mapping for shows as a running tool named by its type, so a new codex tool
// is never silent "thinking". Completion ends it, failed status is an error.
func TestItem_UnknownTypeShowsAsToolByTypeName(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.itemCache = make(map[string]itemEnvelope)
	evs := captureToolEvents(b)

	b.dispatch([]byte(`{"method":"item/started","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"brandNewThing","id":"it_x"}}}`))
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"brandNewThing","id":"it_x","status":"failed"}}}`))

	want := []toolEvent{
		{kind: "start", id: "it_x", name: "brandNewThing"},
		{kind: "end", id: "it_x", name: "brandNewThing", isError: true},
	}
	assertToolEvents(t, *evs, want)
}

// TestItem_NonToolTypesStaySilent: the generic fallback must not turn
// conversation items (messages, reasoning, plans, review-mode markers) into
// tools, and a completion with no matching start shows nothing.
func TestItem_NonToolTypesStaySilent(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.itemCache = make(map[string]itemEnvelope)
	evs := captureToolEvents(b)

	for _, typ := range []string{"userMessage", "agentMessage", "reasoning", "plan", "hookPrompt",
		"functionCallOutput", "subAgentActivity", "collabAgentToolCall", "enteredReviewMode", "exitedReviewMode"} {
		b.dispatch([]byte(`{"method":"item/started","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"` + typ + `","id":"it_` + typ + `"}}}`))
		b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"` + typ + `","id":"it_` + typ + `"}}}`))
	}
	// Completed-only unknown item: no start was shown, so no end either.
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"brandNewThing","id":"it_late"}}}`))

	assertToolEvents(t, *evs, nil)
}

func assertToolEvents(t *testing.T, got, want []toolEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tool events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

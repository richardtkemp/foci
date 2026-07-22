package codex

import (
	"reflect"
	"sort"
	"testing"

	"foci/internal/delegator"
)

// TestDeliverSubagentItems_OldestFirst is the red/green regression for #1324
// sub-issue 2: readSubagentThread previously iterated the flattened item list
// newest-first (for i := len-1; i >= 0; i--), so a subagent's messages reached
// the client in reverse chronological order. deliverSubagentItems must emit
// them oldest-first, matching the order the subagent produced them.
func TestDeliverSubagentItems_OldestFirst(t *testing.T) {
	b := newTestBackend(t)
	var got []string
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnSubagentText: func(_ string, text string, _ int) {
			got = append(got, text)
		},
	})

	items := []subagentItem{
		{Type: "agentMessage", Text: "first"},
		{Type: "reasoning", Text: "ignored"},
		{Type: "agentMessage", Text: "second"},
		{Type: "agentMessage", Text: "third"},
	}
	poll := &subagentPoll{groupKey: "g1"}

	b.deliverSubagentItems(items, poll)

	want := []string{"first", "second", "third"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery order = %v, want %v", got, want)
	}
	if poll.seenItems != len(items) {
		t.Errorf("seenItems = %d, want %d", poll.seenItems, len(items))
	}
}

// TestDeliverSubagentItems_DedupOnlyNew verifies the seen cursor: a second
// poll with the same prefix plus one appended item delivers only the new
// item, still oldest-first.
func TestDeliverSubagentItems_DedupOnlyNew(t *testing.T) {
	b := newTestBackend(t)
	var got []string
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnSubagentText: func(_ string, text string, _ int) {
			got = append(got, text)
		},
	})
	poll := &subagentPoll{groupKey: "g1"}

	b.deliverSubagentItems([]subagentItem{
		{Type: "agentMessage", Text: "a"},
		{Type: "agentMessage", Text: "b"},
	}, poll)
	b.deliverSubagentItems([]subagentItem{
		{Type: "agentMessage", Text: "a"},
		{Type: "agentMessage", Text: "b"},
		{Type: "agentMessage", Text: "c"},
	}, poll)

	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered = %v, want %v", got, want)
	}
}

// TestFinishAll_EndsIndicatorsAndClears is the regression for #1324 sub-issues
// 1 & 3: a normally-completing subagent emits no terminal subAgentActivity
// kind, so onTurnCompleted -> finishAll must end every active poll's indicator
// (via OnSubagentEnd, keyed by groupKey) and clear the active set (stopping the
// idle 500ms poll). Uses the tracker directly with no live goroutines so the
// poll.done close is a no-op flush signal.
func TestFinishAll_EndsIndicatorsAndClears(t *testing.T) {
	b := newTestBackend(t)
	var ended []string
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnSubagentEnd: func(groupKey string, _ int) {
			ended = append(ended, groupKey)
		},
	})
	b.subagents = newSubagentTracker()
	b.subagents.active["thread-A"] = &subagentPoll{groupKey: "g-A", done: make(chan struct{})}
	b.subagents.active["thread-B"] = &subagentPoll{groupKey: "g-B", done: make(chan struct{})}

	b.subagents.finishAll(b)

	if len(b.subagents.active) != 0 {
		t.Errorf("active not cleared: %d entries remain", len(b.subagents.active))
	}
	sort.Strings(ended)
	want := []string{"g-A", "g-B"}
	if !reflect.DeepEqual(ended, want) {
		t.Fatalf("ended indicators = %v, want %v", ended, want)
	}
}

// TestFinishAll_NoEndCallback is a smoke test: finishAll must not panic when no
// OnSubagentEnd is installed (it still clears the active set).
func TestFinishAll_NoEndCallback(t *testing.T) {
	b := newTestBackend(t)
	b.sessionEvents.Store(&delegator.SessionEvents{})
	b.subagents = newSubagentTracker()
	b.subagents.active["t"] = &subagentPoll{groupKey: "g", done: make(chan struct{})}

	b.subagents.finishAll(b)

	if len(b.subagents.active) != 0 {
		t.Errorf("active not cleared: %d entries remain", len(b.subagents.active))
	}
}

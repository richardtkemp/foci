package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/turnevent"
)

// eventCaptureSink records every event it receives, for assertions on what the
// session router forwarded.
type eventCaptureSink struct {
	mu     sync.Mutex
	events []turnevent.Event
}

func (s *eventCaptureSink) Emit(_ context.Context, ev turnevent.Event) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
}

func (s *eventCaptureSink) DeliversToPlatform() bool { return true }

func (s *eventCaptureSink) snapshot() []turnevent.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]turnevent.Event(nil), s.events...)
}

// TestAttachDelivery_APIRetryReachesRouterAsBackendRetry: a backend's API
// retry (Claude Code's api_retry, #2260) reaches the session router as a
// turnevent.BackendRetry, and never as a RetryNotice, which would post a chat
// message on Telegram/Discord.
func TestAttachDelivery_APIRetryReachesRouterAsBackendRetry(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	be := &mockBackendDT{}
	sk := "test/c2260"
	a.AttachDelivery(be, sk)

	capture := &eventCaptureSink{}
	a.sessionRouter(sk).Register(capture)
	defer a.sessionRouter(sk).Clear()

	se := be.sessionEvents
	if se == nil || se.OnAPIRetry == nil {
		t.Fatal("AttachDelivery installed no OnAPIRetry: CC retries are dropped")
	}
	se.OnAPIRetry(delegator.APIRetry{Endpoint: "Claude Code", Attempt: 3, MaxRetries: 15, Delay: 2 * time.Minute, Status: 529, Category: "overloaded"})

	got := capture.snapshot()
	if len(got) != 1 {
		t.Fatalf("router received %d events, want 1: %#v", len(got), got)
	}
	if _, ok := got[0].(turnevent.RetryNotice); ok {
		t.Fatalf("router got a RetryNotice: %#v", got[0])
	}
	want := turnevent.BackendRetry{Endpoint: "Claude Code", Attempt: 3, MaxRetries: 15, Delay: 2 * time.Minute, Status: 529, Category: "overloaded"}
	if got[0] != turnevent.Event(want) {
		t.Errorf("router event = %#v, want %#v", got[0], want)
	}
}

// TestAttachDelivery_SubagentPromptReachesRouter pins #2092: AttachDelivery
// builds the only production SessionEvents, and a backend's
// OnSubagentPrompt call (a SendMessage follow-up to a running subagent, #1419)
// must reach the session router as a turnevent.SubagentPrompt. Before the fix
// the closure was never set, so ccstream's nil-check made the call a silent
// no-op and the app never saw a follow-up prompt.
func TestAttachDelivery_SubagentPromptReachesRouter(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	be := &mockBackendDT{}
	sk := "test/c2092"
	a.AttachDelivery(be, sk)

	capture := &eventCaptureSink{}
	a.sessionRouter(sk).Register(capture)
	defer a.sessionRouter(sk).Clear()

	se := be.sessionEvents
	if se == nil {
		t.Fatal("AttachDelivery installed no SessionEvents on the backend")
	}
	if se.OnSubagentPrompt == nil {
		t.Fatal("SessionEvents.OnSubagentPrompt is nil: follow-up prompts to a running subagent are dropped")
	}
	se.OnSubagentPrompt("toolu_1", "and check the tests too", 2)

	got := capture.snapshot()
	if len(got) != 1 {
		t.Fatalf("router received %d events, want 1: %#v", len(got), got)
	}
	want := turnevent.SubagentPrompt{GroupKey: "toolu_1", Prompt: "and check the tests too", RunIndex: 2}
	if got[0] != turnevent.Event(want) {
		t.Errorf("router event = %#v, want %#v", got[0], want)
	}
}

// TestAttachDelivery_ToolEndNotReported (#3188): a backend's not-reported end
// reaches the router as a ToolResult with OutputNotReported, closes the call
// in the tool tally, and a plain end with empty output stays unflagged.
func TestAttachDelivery_ToolEndNotReported(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	be := &mockBackendDT{}
	sk := "test/c3188"
	a.AttachDelivery(be, sk)
	tt := a.resetToolTally(sk)

	capture := &eventCaptureSink{}
	a.sessionRouter(sk).Register(capture)
	defer a.sessionRouter(sk).Clear()

	se := be.sessionEvents
	if se == nil || se.OnToolEndNotReported == nil {
		t.Fatal("AttachDelivery installed no OnToolEndNotReported: the flag never reaches the app or the trace")
	}
	se.OnToolStart("m1", "mcp:s.t", "{}")
	se.ToolEndNotReported("m1", "mcp:s.t", false)
	se.OnToolStart("c1", "bash", "{}")
	se.OnToolEnd("c1", "bash", "", false)

	var results []turnevent.ToolResult
	for _, ev := range capture.snapshot() {
		if r, ok := ev.(turnevent.ToolResult); ok {
			results = append(results, r)
		}
	}
	want := []turnevent.ToolResult{
		{ID: "m1", Name: "mcp:s.t", OutputNotReported: true},
		{ID: "c1", Name: "bash"},
	}
	if len(results) != len(want) {
		t.Fatalf("router ToolResults = %#v, want %#v", results, want)
	}
	for i := range want {
		if results[i] != want[i] {
			t.Errorf("ToolResult %d = %#v, want %#v", i, results[i], want[i])
		}
	}
	if got := tt.completed(); got != 2 {
		t.Errorf("tally completed = %d, want 2 (a not-reported end still ends the call)", got)
	}
}

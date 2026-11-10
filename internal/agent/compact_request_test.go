package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/compaction"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
)

// requestTurn is a finished turn on sk's live backend, with usage far below
// any compaction threshold.
func requestTurn(t *testing.T, a *Agent, sk string) *TurnState {
	t.Helper()
	be, err := a.DelegatedManager.Get(context.Background(), sk)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	ts := NewTurnState(context.Background(), sk, []string{"hi"}, nil)
	ts.SessionMeta = a.getSessionMeta(sk)
	ts.Backend = be
	ts.StartedAt = time.Now()
	ts.FinalUsage = &provider.Usage{InputTokens: 10}
	return ts
}

// TestRequestedCompaction_RunsAfterTurnWhileSubagentsRun (#3040): the agent's
// request is run by the turn's post-turn RunCompaction, below any threshold
// and while a subagent runs; the CC restart (prompt changed) waits for the
// subagent instead of killing it, then happens.
func TestRequestedCompaction_RunsAfterTurnWhileSubagentsRun(t *testing.T) {
	t.Parallel()
	const sk = "test-agent/c1"
	ag, be, sent := bgCompactAgent(t, sk)

	if err := ag.RequestCompaction(sk, ""); err != nil {
		t.Fatalf("RequestCompaction: %v", err)
	}
	if len(*sent) != 0 {
		t.Fatalf("the request compacted at once (sent %q): it must wait for the turn's end", *sent)
	}
	if !ag.CompactionRequested(sk) {
		t.Fatal("request not recorded")
	}

	tr := &DelegatedTransport{sharedTurnOps{agent: ag}}
	tr.RunCompaction(requestTurn(t, ag, sk))

	if len(*sent) != 1 || !strings.HasPrefix((*sent)[0], "/compact ") {
		t.Fatalf("sent %q, want one /compact", *sent)
	}
	if ag.CompactionRequested(sk) {
		t.Error("request still recorded after it ran")
	}
	if be.wasClosed() {
		t.Fatal("the restart killed the running subagent: it must wait for it")
	}
	be.set(nil, false) // the subagent finishes
	waitClosed(t, be)

	// Consumed: the next turn does not compact again.
	tr.RunCompaction(requestTurn(t, ag, sk))
	if len(*sent) != 1 {
		t.Errorf("a second turn compacted again (sent %q)", *sent)
	}
}

// TestRequestedCompaction_WaitsForARealTurnEnd: a post-turn with no final
// usage (a steered follow-up, whose backend run continues) leaves the request
// for the run's own end.
func TestRequestedCompaction_WaitsForARealTurnEnd(t *testing.T) {
	t.Parallel()
	const sk = "test-agent/c1"
	ag, _, sent := bgCompactAgent(t, sk)
	if err := ag.RequestCompaction(sk, ""); err != nil {
		t.Fatalf("RequestCompaction: %v", err)
	}
	tr := &DelegatedTransport{sharedTurnOps{agent: ag}}
	ts := requestTurn(t, ag, sk)
	ts.FinalUsage = nil
	tr.RunCompaction(ts)
	if len(*sent) != 0 {
		t.Fatalf("compacted on a post-turn with no final usage (sent %q)", *sent)
	}
	if !ag.CompactionRequested(sk) {
		t.Fatal("request dropped by a post-turn that did not run it")
	}
}

// TestRequestedCompaction_ClearedBySessionReset: a reset drops a request
// the old conversation made.
func TestRequestedCompaction_ClearedBySessionReset(t *testing.T) {
	t.Parallel()
	const sk = "test-agent/c1"
	ag, _, _ := bgCompactAgent(t, sk)
	if err := ag.RequestCompaction(sk, ""); err != nil {
		t.Fatalf("RequestCompaction: %v", err)
	}
	ag.ClearSessionState(sk)
	if ag.CompactionRequested(sk) {
		t.Error("request survived a session reset")
	}
}

// TestRequestCompaction_NotConfigured: an agent with no way to compact says
// so at request time, not silently after the turn.
func TestRequestCompaction_NotConfigured(t *testing.T) {
	t.Parallel()
	ag := &Agent{}
	if err := ag.RequestCompaction("test-agent/c1", ""); err == nil {
		t.Fatal("want an error for an agent with no compactor and no backend")
	}
}

// TestRequestedCompaction_APITransport: the API transport runs the request
// below the threshold too (same compactSession path as /compact).
func TestRequestedCompaction_APITransport(t *testing.T) {
	t.Parallel()
	const sk = "test-agent/c1"
	store := session.NewStore(t.TempDir())
	ag := &Agent{
		Model:     "test-model",
		Sessions:  store,
		Compactor: compaction.NewCompactor(store, 0.8),
	}
	var memorySK string
	ag.CompactionMemoryFunc.Add(func(sk string) { memorySK = sk })
	if err := ag.RequestCompaction(sk, ""); err != nil {
		t.Fatalf("RequestCompaction: %v", err)
	}
	ts := NewTurnState(context.Background(), sk, []string{"hi"}, nil)
	ts.SessionMeta = ag.getSessionMeta(sk)
	ts.FinalUsage = &provider.Usage{InputTokens: 10}

	tr := &APITransport{sharedTurnOps{agent: ag}}
	tr.RunCompaction(ts)

	// The session is empty, so compactSession declines with "too few
	// messages" after the memory hook: the request reached the compaction
	// pipeline, whatever the threshold, and was consumed.
	if memorySK != sk {
		t.Errorf("memory hook fired for %q, want %q: the request did not run", memorySK, sk)
	}
	if ag.CompactionRequested(sk) {
		t.Error("request still recorded after the turn ended")
	}
}

// TestRequestedCompaction_NoCompactSessionRefused (Q64, Dick 2026-11-10): a
// no_compact session refuses the request at request time, and a request made
// before no_compact was set is dropped at the turn's end, compacting nothing.
func TestRequestedCompaction_NoCompactSessionRefused(t *testing.T) {
	t.Parallel()
	const sk = "test-agent/c1"
	ag, _, sent := bgCompactAgent(t, sk)

	ag.SetSessionNoCompact(sk, true)
	if err := ag.RequestCompaction(sk, ""); err == nil {
		t.Fatal("want an error: the session has no_compact set")
	}
	if ag.CompactionRequested(sk) {
		t.Fatal("a refused request was recorded")
	}

	ag.SetSessionNoCompact(sk, false)
	if err := ag.RequestCompaction(sk, ""); err != nil {
		t.Fatalf("RequestCompaction: %v", err)
	}
	ag.SetSessionNoCompact(sk, true)
	tr := &DelegatedTransport{sharedTurnOps{agent: ag}}
	tr.RunCompaction(requestTurn(t, ag, sk))
	if len(*sent) != 0 {
		t.Fatalf("sent %q: a request in a no_compact session must not compact", *sent)
	}
	if ag.CompactionRequested(sk) {
		t.Error("request still recorded after it was dropped")
	}
}

// injected is one message a test notifier received.
type injected struct{ session, message, trigger string }

// recordInjections gives ag an AsyncNotifier that records every injection.
func recordInjections(ag *Agent) *[]injected {
	var mu sync.Mutex
	var got []injected
	ag.AsyncNotifier = tools.NewAsyncNotifier(func(target, msg, _, trigger string, _ *tools.AgentOrigin) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, injected{target, msg, trigger})
	})
	return &got
}

// TestRequestedCompaction_WakeAfterCompaction (#3041): a request with a wake
// message injects it into the same session as a wake (trigger
// scheduled_wake) once the compaction finished, saying it compacted, and
// sends no compaction-resume nudge beside it. The control arm (no wake
// message, same restart) shows the nudge path is live, so its absence above
// is the wake's doing.
func TestRequestedCompaction_WakeAfterCompaction(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, wake  string
		wantTrigger string
	}{
		{"with wake", "continue the triage: step 2", compactionWakeTrigger},
		{"without wake", "", "compaction-resume"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const sk = "test-agent/c1"
			ag, be, sent := bgCompactAgent(t, sk)
			be.set(nil, false) // idle: the restart happens at once
			got := recordInjections(ag)

			if err := ag.RequestCompaction(sk, tc.wake); err != nil {
				t.Fatalf("RequestCompaction: %v", err)
			}
			tr := &DelegatedTransport{sharedTurnOps{agent: ag}}
			tr.RunCompaction(requestTurn(t, ag, sk))

			if len(*sent) != 1 || !strings.HasPrefix((*sent)[0], "/compact ") {
				t.Fatalf("sent %q, want one /compact", *sent)
			}
			if !be.wasClosed() {
				t.Fatal("no restart: the control needs one for the nudge path")
			}
			if len(*got) != 1 {
				t.Fatalf("injections = %+v, want exactly one", *got)
			}
			inj := (*got)[0]
			if inj.session != sk || inj.trigger != tc.wantTrigger {
				t.Fatalf("injection = %+v, want session %s trigger %s", inj, sk, tc.wantTrigger)
			}
			if tc.wake != "" {
				if !strings.Contains(inj.message, tc.wake) || !strings.Contains(inj.message, "Compaction finished") {
					t.Errorf("wake text %q lacks the message or the outcome", inj.message)
				}
			}
		})
	}
}

// TestRequestedCompaction_WakeWhenNotCompacted (#3041): the wake is still
// injected when the compaction does not happen, saying why: dropped because
// no_compact was set since the request, or failed (API transport, too few
// messages).
func TestRequestedCompaction_WakeWhenNotCompacted(t *testing.T) {
	t.Parallel()
	const sk = "test-agent/c1"

	t.Run("dropped by no_compact", func(t *testing.T) {
		t.Parallel()
		ag, _, sent := bgCompactAgent(t, sk)
		got := recordInjections(ag)
		if err := ag.RequestCompaction(sk, "go on"); err != nil {
			t.Fatalf("RequestCompaction: %v", err)
		}
		ag.SetSessionNoCompact(sk, true)
		tr := &DelegatedTransport{sharedTurnOps{agent: ag}}
		tr.RunCompaction(requestTurn(t, ag, sk))
		if len(*sent) != 0 {
			t.Fatalf("sent %q: no_compact must not compact", *sent)
		}
		if len(*got) != 1 || (*got)[0].trigger != compactionWakeTrigger ||
			!strings.Contains((*got)[0].message, "no_compact") || !strings.Contains((*got)[0].message, "go on") {
			t.Fatalf("injections = %+v, want one wake naming no_compact", *got)
		}
	})

	t.Run("failed", func(t *testing.T) {
		t.Parallel()
		store := session.NewStore(t.TempDir())
		ag := &Agent{Model: "test-model", Sessions: store, Compactor: compaction.NewCompactor(store, 0.8)}
		got := recordInjections(ag)
		if err := ag.RequestCompaction(sk, "go on"); err != nil {
			t.Fatalf("RequestCompaction: %v", err)
		}
		ts := NewTurnState(context.Background(), sk, []string{"hi"}, nil)
		ts.SessionMeta = ag.getSessionMeta(sk)
		ts.FinalUsage = &provider.Usage{InputTokens: 10}
		(&APITransport{sharedTurnOps{agent: ag}}).RunCompaction(ts)
		if len(*got) != 1 || !strings.Contains((*got)[0].message, "Compaction failed") ||
			!strings.Contains((*got)[0].message, "too few messages") {
			t.Fatalf("injections = %+v, want one wake reporting the failure", *got)
		}
	})
}

// TestRequestCompaction_WakeMessageKeptAndRefused (#3041): a second request
// without a wake keeps the first one's; an agent with no injection path
// refuses a wake message at request time.
func TestRequestCompaction_WakeMessageKeptAndRefused(t *testing.T) {
	t.Parallel()
	const sk = "test-agent/c1"
	ag, _, _ := bgCompactAgent(t, sk)
	if err := ag.RequestCompaction(sk, "go on"); err == nil {
		t.Fatal("want an error: no AsyncNotifier, so the wake could not be injected")
	}
	if ag.CompactionRequested(sk) {
		t.Fatal("a refused request was recorded")
	}

	recordInjections(ag)
	if err := ag.RequestCompaction(sk, "go on"); err != nil {
		t.Fatalf("RequestCompaction: %v", err)
	}
	if err := ag.RequestCompaction(sk, ""); err != nil {
		t.Fatalf("RequestCompaction: %v", err)
	}
	if req, wake := ag.takeCompactionRequest(sk); !req || wake != "go on" {
		t.Fatalf("take = (%v, %q), want the first wake kept", req, wake)
	}
}

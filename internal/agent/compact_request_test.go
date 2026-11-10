package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/compaction"
	"foci/internal/provider"
	"foci/internal/session"
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

	if err := ag.RequestCompaction(sk); err != nil {
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
	if err := ag.RequestCompaction(sk); err != nil {
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
	if err := ag.RequestCompaction(sk); err != nil {
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
	if err := ag.RequestCompaction("test-agent/c1"); err == nil {
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
	if err := ag.RequestCompaction(sk); err != nil {
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

package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/workspace"
)

// These tests only mean something under -race (make test-one with
// GOFLAGS=-race): each pairs a writer of sessionMeta with a reader on another
// goroutine, and the detector reports any pair not ordered by metaMu.
// #2912 found the first pair in a nightly L2 run.
//
// None is parallel: a race report fails whichever tests are running when it
// fires, so a parallel sibling would turn red for another test's race.

// runMetaPair runs write a few rounds, with read looping on another goroutine
// until the writes are done, so read overlaps the whole of a slow write path. The
// close only orders the writes before read's exit, never read before a write.
func runMetaPair(write, read func()) {
	const rounds = 50
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			read() // before the check: read runs at least once, unordered with the writes
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	for range rounds {
		write()
	}
	close(done)
	wg.Wait()
}

// raceTurnState is a finished turn on key: its usage and model are what the end
// of a turn writes into the session meta.
func raceTurnState(a *Agent, key string) *TurnState {
	ts := NewTurnState(context.Background(), key, []string{"hi"}, nil)
	ts.SessionMeta = a.getSessionMeta(key)
	ts.FinalModel = "claude-opus-4-20250514"
	ts.FinalUsage = &provider.Usage{InputTokens: 5, OutputTokens: 6, CacheReadInputTokens: 7, CacheCreationInputTokens: 8}
	return ts
}

// TestMetaRace_PostTurnVsSessionModel is #2912: the end of a delegated turn
// records the backend's model while a keepalive scan on another goroutine reads
// it (keepaliveTargetsAt -> ttlFor -> SessionCacheTTL -> SessionModel).
func TestMetaRace_PostTurnVsSessionModel(t *testing.T) {
	a := &Agent{Model: "agent-default"}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := raceTurnState(a, "bot/c100")
	close(ts.CompletionChan)

	runMetaPair(
		func() { a.runPostTurn(tr, ts) },
		func() { _ = a.SessionModel(ts.SessionKey) },
	)

	if got := a.SessionModel(ts.SessionKey); got != ts.FinalModel {
		t.Errorf("SessionModel = %q, want the turn's model %q", got, ts.FinalModel)
	}
}

// TestMetaRace_UpdateSessionMetaVsStatusline: a turn's [meta] header can render
// the previous turn's usage and gap. An adopted autonomous turn ends outside the
// turn lock, so that render can meet the write, on either transport. The default
// template shows neither, so this one asks for them.
func TestMetaRace_UpdateSessionMetaVsStatusline(t *testing.T) {
	var cfg config.Config
	const src = "[[agents]]\nid = \"a\"\ndisplay.statusline = \"{gap} {cost} {tokens} {cache_read}\"\n"
	if _, err := toml.Decode(src, &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rc := config.Resolve(&cfg, cfg.Agents[0])

	for name, mk := range map[string]func(*Agent) TurnContract{
		"delegated": func(a *Agent) TurnContract { return &DelegatedTransport{sharedTurnOps{agent: a}} },
		"api":       func(a *Agent) TurnContract { return &APITransport{sharedTurnOps{agent: a}} },
	} {
		t.Run(name, func(t *testing.T) {
			a := &Agent{AgentID: "a", LiveConfigFn: func() *config.ResolvedAgentConfig { return rc }}
			tr := mk(a)
			ts := raceTurnState(a, "a/main")

			runMetaPair(
				func() { tr.UpdateSessionMeta(ts) },
				func() {
					got := a.composeTurnText(context.Background(), ts.SessionKey, "m", []string{"hi"}, nil).MetaPrefix
					if strings.Contains(got, "{") {
						t.Errorf("header %q left a placeholder unrendered", got)
					}
				},
			)

			got := a.composeTurnText(context.Background(), ts.SessionKey, "m", []string{"hi"}, nil).MetaPrefix
			if !strings.Contains(got, "prev_tokens=in:5/out:6") {
				t.Errorf("header %q does not show the finished turn's usage", got)
			}
		})
	}
}

// TestMetaRace_LastMessageTimeVsMetaStatus: the app's meta frame reads the gap
// from a sink goroutine while a turn entry stamps it.
func TestMetaRace_LastMessageTimeVsMetaStatus(t *testing.T) {
	a := &Agent{Model: "agent-default"}
	ts := raceTurnState(a, "bot/c100")

	runMetaPair(
		func() { a.recordTurnRan(ts) },
		func() { _ = a.MetaStatus(ts.SessionKey) },
	)
}

// TestMetaRace_CacheBaselineVsBranchRead: creating a branch reads its parent's
// cache baseline under the lock, from the goroutine that forks, while the
// parent's turn moves that baseline.
func TestMetaRace_CacheBaselineVsBranchRead(t *testing.T) {
	writers := map[string]func(a *Agent, sm *sessionMeta){
		"processAPIResponse": func(a *Agent, sm *sessionMeta) {
			resp := &provider.MessageResponse{Usage: provider.Usage{CacheReadInputTokens: 700}}
			a.processAPIResponse(orchestratorTestKey, sm, resp, 0, 0)
		},
		"ResetCacheBaseline": func(a *Agent, _ *sessionMeta) { a.ResetCacheBaseline(orchestratorTestKey) },
		"delegated UpdateSessionMeta": func(a *Agent, sm *sessionMeta) {
			ts := raceTurnState(a, orchestratorTestKey)
			ts.SessionMeta = sm
			(&DelegatedTransport{sharedTurnOps{agent: a}}).UpdateSessionMeta(ts)
		},
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			a, _ := newCacheEvidenceTestAgent(t)
			sm := a.getSessionMeta(orchestratorTestKey)
			runMetaPair(
				func() { write(a, sm) },
				func() { a.NoteBranchInheritsRoot(orchestratorTestBranchKey, orchestratorTestKey) },
			)
		})
	}
}

// TestMetaRace_SystemBlocksVsInvalidate: InvalidateSystemCaches clears every
// session's cached prompt from whichever goroutine ran the reset, while another
// session's turn reads and fills its own.
func TestMetaRace_SystemBlocksVsInvalidate(t *testing.T) {
	a := &Agent{Model: "agent-default", Bootstrap: workspace.NewBootstrap(t.TempDir(), []string{})}
	// Both sides take metaMu just before the unlocked store, which orders them
	// unless the invalidation lands between that lock and the store. The sleep
	// holds that window open; any sync here would order them too.
	a.DailyDigestFunc = func(string) string {
		time.Sleep(time.Millisecond)
		return ""
	}

	runMetaPair(
		func() { _ = a.buildSystemBlocks(orchestratorTestKey) },
		func() { a.InvalidateSystemCaches() },
	)
}

// TestMetaRace_CompactionResetVsInvalidate: the reset after a compaction clears
// the session's cached prompt and cache baseline in the post-turn path, while
// InvalidateSystemCaches and a fork read or clear them elsewhere. maybeCompact
// (API) shares resetAfterCompaction with this delegated path.
func TestMetaRace_CompactionResetVsInvalidate(t *testing.T) {
	be := &mockBackendDT{sendCommandFn: func(_ context.Context, _ string) error { return nil }}
	a := &Agent{
		Model: "test-model",
		// As in TestMetaRace_SystemBlocksVsInvalidate, the sleep holds open the
		// window between this path's last metaMu and its store.
		NudgeReloadFunc:  func() { time.Sleep(time.Millisecond) },
		Compactor:        compaction.NewCompactor(session.NewStore(t.TempDir()), 0.8),
		DelegatedManager: newMockDelegatedManager(t, be),
	}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	// "test/s" is the key newMockDelegatedManager registers.
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	ts.Backend = be
	ts.StartedAt = time.Now()
	ts.FinalUsage = &provider.Usage{InputTokens: 100000, CacheReadInputTokens: 70000}

	runMetaPair(
		func() { tr.RunCompaction(ts) },
		func() { a.InvalidateSystemCaches() },
	)
}

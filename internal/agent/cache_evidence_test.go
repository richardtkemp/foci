package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
	"foci/internal/provider"
	"foci/internal/session"
)

func newCacheEvidenceTestAgent(t *testing.T) (*Agent, *session.SessionIndex) {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	a := &Agent{AgentID: "test-agent", SessionIndex: idx}
	seedCacheTestRow(idx, orchestratorTestKey)
	seedCacheTestRow(idx, orchestratorTestBranchKey)
	return a, idx
}

// TestRecordCacheEvidence_OnlyCacheTokensAreEvidence: a response stamps
// last_cache_touch only when it reports cache-read or cache-write tokens
// (Dick's ruling, 2026-11-03, #2509).
func TestRecordCacheEvidence_OnlyCacheTokensAreEvidence(t *testing.T) {
	t.Parallel()
	a, idx := newCacheEvidenceTestAgent(t)

	a.recordCacheEvidence(orchestratorTestKey, 0, 0)
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); ok {
		t.Fatal("a response with no cache tokens stamped last_cache_touch")
	}
	a.recordCacheEvidence(orchestratorTestKey, 0, 50)
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); !ok {
		t.Fatal("a cache write was not taken as evidence")
	}
	idx.ClearCacheTouch(orchestratorTestKey)
	a.recordCacheEvidence(orchestratorTestKey, 50, 0)
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); !ok {
		t.Fatal("a cache read was not taken as evidence")
	}
}

// TestNoteBranchInheritsRoot_StampsRootOnlyOnEvidence is the #2509 incident: a
// fork must not mark its root warm when it is created, nor when its turn
// produces no model response. Only the branch's first evidenced response
// stamps the root, and only once.
func TestNoteBranchInheritsRoot_StampsRootOnlyOnEvidence(t *testing.T) {
	t.Parallel()
	a, idx := newCacheEvidenceTestAgent(t)

	a.NoteBranchInheritsRoot(orchestratorTestBranchKey, orchestratorTestKey)
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); ok {
		t.Fatal("creating a branch stamped the root's last_cache_touch")
	}
	// A failed call: no usage, so no evidence.
	a.recordCacheEvidence(orchestratorTestBranchKey, 0, 0)
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); ok {
		t.Fatal("a branch response without cache tokens stamped the root")
	}

	a.recordCacheEvidence(orchestratorTestBranchKey, 1000, 10)
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); !ok {
		t.Fatal("the branch's first evidenced response did not stamp the root")
	}
	if _, ok := idx.LastCacheTouch(orchestratorTestBranchKey); !ok {
		t.Fatal("the branch's evidenced response did not stamp the branch")
	}

	// One-time: later branch calls read the branch's own prefix.
	idx.ClearCacheTouch(orchestratorTestKey)
	a.recordCacheEvidence(orchestratorTestBranchKey, 2000, 10)
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); ok {
		t.Fatal("a second branch response stamped the root again")
	}
}

// TestNoteBranchInheritsRoot_StampsSourceBranch: a fork of a branch (a facet
// open as an app chat) reads that branch's prefix, so its source is stamped
// as well as the root. Keys have one branch level, so the fork is a sibling
// key under the same root.
func TestNoteBranchInheritsRoot_StampsSourceBranch(t *testing.T) {
	t.Parallel()
	a, idx := newCacheEvidenceTestAgent(t)
	grandchild := orchestratorTestKey + "/b1700000001"
	seedCacheTestRow(idx, grandchild)

	a.NoteBranchInheritsRoot(grandchild, orchestratorTestBranchKey)
	a.recordCacheEvidence(grandchild, 100, 0)
	for _, k := range []string{orchestratorTestKey, orchestratorTestBranchKey, grandchild} {
		if _, ok := idx.LastCacheTouch(k); !ok {
			t.Errorf("%s not stamped by the fork's first evidenced response", k)
		}
	}
}

// TestNoteBranchInheritsRoot_NoopForRoot: a root has nothing above it.
func TestNoteBranchInheritsRoot_NoopForRoot(t *testing.T) {
	t.Parallel()
	a, _ := newCacheEvidenceTestAgent(t)
	a.NoteBranchInheritsRoot(orchestratorTestKey, "")
	if _, ok := a.takeBranchRootWarm(orchestratorTestKey); ok {
		t.Fatal("a root key was recorded as an inheriting branch")
	}
}

// TestRecordCacheEvidence_WarnsWhenForkReadsLessThanSource: Dick's ruling
// (2026-11-03): warn when a fork's first cache read is less than its source's
// prior cache read. Not parallel: captures the global log.
func TestRecordCacheEvidence_WarnsWhenForkReadsLessThanSource(t *testing.T) {
	logs := captureLogs(t)
	a, _ := newCacheEvidenceTestAgent(t)
	a.recordCacheEvidence(orchestratorTestKey, 5000, 20)

	a.NoteBranchInheritsRoot(orchestratorTestBranchKey, orchestratorTestKey)
	a.recordCacheEvidence(orchestratorTestBranchKey, 5000, 20)
	if strings.Contains(logs.String(), "fork "+orchestratorTestBranchKey) {
		t.Fatalf("warned on a fork that read the source's whole prefix:\n%s", logs.String())
	}

	a.NoteBranchInheritsRoot(orchestratorTestBranchKey, orchestratorTestKey)
	a.recordCacheEvidence(orchestratorTestBranchKey, 1200, 3800)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "less than its source "+orchestratorTestKey) {
		if time.Now().After(deadline) {
			t.Fatalf("no WARN for a fork that read 1200 < 5000:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestProcessAPIResponse_StampsCacheEvidence: on the API transport each model
// response is the evidence (#2509): one with cache tokens stamps the session,
// one without does not.
func TestProcessAPIResponse_StampsCacheEvidence(t *testing.T) {
	t.Parallel()
	a, idx := newCacheEvidenceTestAgent(t)
	sm := &sessionMeta{}

	a.processAPIResponse(orchestratorTestKey, sm, &provider.MessageResponse{}, 0, time.Now(), 0)
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); ok {
		t.Fatal("a response with no cache tokens stamped last_cache_touch")
	}
	resp := &provider.MessageResponse{Usage: provider.Usage{CacheReadInputTokens: 700}}
	a.processAPIResponse(orchestratorTestKey, sm, resp, 0, time.Now(), 0)
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); !ok {
		t.Fatal("a response with cache-read tokens did not stamp last_cache_touch")
	}
}

// TestRecordCacheEvidence_ForkMissDoesNotWarmSource is #2929: a keepalive fork
// whose first call reads less than its source's last cached read did not read
// the source's newest cache entry, so it must not stamp the source (or root)
// warm. Stamping it kept keepalive forking hourly at a full cache write each
// (2026-11-07, 09:15-14:12) with nothing left to refresh.
func TestRecordCacheEvidence_ForkMissDoesNotWarmSource(t *testing.T) {
	t.Parallel()
	a, idx := newCacheEvidenceTestAgent(t)
	grandchild := orchestratorTestKey + "/b1700000002"
	seedCacheTestRow(idx, grandchild)

	a.recordCacheEvidence(orchestratorTestKey, 5000, 20)
	a.recordCacheEvidence(orchestratorTestBranchKey, 5000, 20)
	idx.ClearCacheTouch(orchestratorTestKey)
	idx.ClearCacheTouch(orchestratorTestBranchKey)

	// A miss: the fork read only the static prefix and wrote the rest.
	a.NoteBranchInheritsRoot(grandchild, orchestratorTestBranchKey)
	a.recordCacheEvidence(grandchild, 1200, 3800)
	for _, k := range []string{orchestratorTestKey, orchestratorTestBranchKey} {
		if _, ok := idx.LastCacheTouch(k); ok {
			t.Errorf("a fork that missed its source's cache stamped %s warm", k)
		}
	}
	if _, ok := idx.LastCacheTouch(grandchild); !ok {
		t.Error("the fork's own cache write did not stamp the fork")
	}

	// A hit still warms both.
	a.NoteBranchInheritsRoot(grandchild, orchestratorTestBranchKey)
	a.recordCacheEvidence(grandchild, 5020, 30)
	for _, k := range []string{orchestratorTestKey, orchestratorTestBranchKey} {
		if _, ok := idx.LastCacheTouch(k); !ok {
			t.Errorf("a fork that read its source's whole prefix did not stamp %s", k)
		}
	}
}

// TestNoteBranchInheritsRoot_BaselineIsSourcesLastCall: the miss baseline is
// the source's last evidenced call, not its turn-end usage. On 2026-11-07
// 03:43 depth leaves read the post-compaction parent's 85876 cached tokens,
// a hit, but were compared with the pre-compaction turn figure of 202224 and
// warned. Not parallel: captures the global log.
func TestNoteBranchInheritsRoot_BaselineIsSourcesLastCall(t *testing.T) {
	logs := captureLogs(t)
	a, idx := newCacheEvidenceTestAgent(t)
	a.getSessionMeta(orchestratorTestKey).prevCacheRead = 202224
	a.recordCacheEvidence(orchestratorTestKey, 85876, 2589)
	idx.ClearCacheTouch(orchestratorTestKey)

	a.NoteBranchInheritsRoot(orchestratorTestBranchKey, orchestratorTestKey)
	a.recordCacheEvidence(orchestratorTestBranchKey, 85876, 5001)
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); !ok {
		t.Error("a fork that read the source's last cached prefix did not stamp the root")
	}
	if strings.Contains(logs.String(), "fork "+orchestratorTestBranchKey) {
		t.Fatalf("warned on a fork that read the source's last cached prefix:\n%s", logs.String())
	}
}

// TestNoteBranchInheritsRoot_BaselineSurvivesARestart is #2932: the source's
// last cached read is in memory (lastCallCacheRead), so after a foci restart a
// source that has not called since had baseline 0, a missed keepalive fork
// stamped the root warm, and keepalive kept forking hourly (7 Nov, c7026 and
// polaris). The ledger holds the source's last call; the fresh Agent (the
// restart) must take its baseline from there. Not parallel: sets the live
// ledger.
func TestNoteBranchInheritsRoot_BaselineSurvivesARestart(t *testing.T) {
	l, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{})
	if err != nil {
		t.Fatalf("accounting.Open: %v", err)
	}
	accounting.SetLive(l)
	t.Cleanup(func() { accounting.SetLive(nil); _ = l.Close() })

	const root = "test-agent/c2932"
	const fork = root + "/b1700002932"
	when := time.Now().Add(-50 * time.Minute)
	tn := accounting.Turn{TurnID: root + "@1", Session: root, AgentID: "test-agent",
		Backend: accounting.BackendCCStream, Source: accounting.SourceUser, StartedAt: when}
	r := accounting.APIResponse{ID: "m2932", Kind: accounting.KindCall, Provider: "anthropic",
		Model: "claude-opus-5-5", Session: root, AgentID: "test-agent", TurnID: tn.TurnID,
		Start: when, StopReason: "end_turn",
		Tokens: modelinfo.Tokens{modelinfo.ClassInput: 2, modelinfo.ClassCacheRead: 12199, modelinfo.ClassCacheWrite1h: 44744}}
	if err := accounting.Record(tn, r.Call()); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// The restart: an Agent with no session meta for root.
	a, idx := newCacheEvidenceTestAgent(t)
	seedCacheTestRow(idx, root)
	seedCacheTestRow(idx, fork)

	// A miss: the fork read only the static prefix.
	a.NoteBranchInheritsRoot(fork, root)
	a.recordCacheEvidence(fork, 4096, 52000)
	if _, ok := idx.LastCacheTouch(root); ok {
		t.Fatal("after a restart, a fork that missed its source's cache stamped the root warm")
	}

	// A hit still warms the root.
	a.NoteBranchInheritsRoot(fork, root)
	a.recordCacheEvidence(fork, 12199, 300)
	if _, ok := idx.LastCacheTouch(root); !ok {
		t.Fatal("after a restart, a fork that read its source's cache did not stamp the root")
	}
}

// TestClearSessionState_MemoryBranchDoesNotRewarmResetSession is #2967. A
// reset makes its session-end-memory branch BEFORE it clears the session
// (ResetSession: PrepareSessionEndMemory, then ClearSessionState). The
// branch's first call then reads the discarded conversation's cache. That is
// no evidence for the reset session, which has no conversation and no cache
// yet. Stamping the root kept keepalive firing 55 min after polaris's 04:30
// reset (8-11 Nov): an in-place turn, then an hourly fork miss and alert.
func TestClearSessionState_MemoryBranchDoesNotRewarmResetSession(t *testing.T) {
	t.Parallel()
	a, idx := newCacheEvidenceTestAgent(t)
	a.Sessions = session.NewStore(t.TempDir())
	if err := a.Sessions.TestAppend(orchestratorTestKey, provider.Message{Role: "user", Content: provider.TextContent("hi")}); err != nil {
		t.Fatal(err)
	}
	a.recordCacheEvidence(orchestratorTestKey, 100000, 500)

	// The order ResetSession uses.
	branchKey, ok := a.createMemoryBranch(orchestratorTestKey, "session-end-memory", "")
	if !ok {
		t.Fatal("createMemoryBranch failed")
	}
	seedCacheTestRow(idx, branchKey)
	a.ClearSessionState(orchestratorTestKey)
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); ok {
		t.Fatal("ClearSessionState left last_cache_touch set")
	}

	// The memory branch's first call: a full read of the old lineage.
	a.recordCacheEvidence(branchKey, 104712, 10378)
	if at, ok := idx.LastCacheTouch(orchestratorTestKey); ok {
		t.Fatalf("the reset session was stamped warm at %s by its memory branch's call; it has no cache to keep alive", at)
	}
	if _, ok := idx.LastCacheTouch(branchKey); !ok {
		t.Error("the memory branch's own call did not stamp the branch")
	}
}

// TestClearSessionState_ResetRootStillLetsFacetBeWarmed: a reset clears only
// the session that was reset. A fork pending on an open facet (source) whose
// root is reset still read the facet's prefix, so the facet is stamped; the
// root is not.
func TestClearSessionState_ResetRootStillLetsFacetBeWarmed(t *testing.T) {
	t.Parallel()
	a, idx := newCacheEvidenceTestAgent(t)
	grandchild := orchestratorTestKey + "/b1700000003"
	seedCacheTestRow(idx, grandchild)

	a.NoteBranchInheritsRoot(grandchild, orchestratorTestBranchKey)
	a.ClearSessionState(orchestratorTestKey)
	a.recordCacheEvidence(grandchild, 100, 0)
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); ok {
		t.Error("the reset root was stamped by a fork made before the reset")
	}
	if _, ok := idx.LastCacheTouch(orchestratorTestBranchKey); !ok {
		t.Error("the facet source was not stamped; only the root was reset")
	}
}

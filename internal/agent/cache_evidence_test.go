package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	a.getSessionMeta(orchestratorTestKey).prevCacheRead = 5000

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

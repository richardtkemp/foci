package periodic

import (
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
)

// TestMaybeReflection_IntervalRunsFromSessionsLastReflection is #3088
// requirement 2: a reflection that ran outside the interval pass (the
// pre-compaction one, stamped on the session) counts as that session's
// reflection, so the interval pass does not reflect it again until one
// interval after it. On 13 Nov the interval pass forked a chat ten minutes
// after its compaction (#3086). Other sessions are unaffected: one last
// reflected two hours ago, and one never reflected (only the seed its row
// gets at birth), are both still due.
func TestMaybeReflection_IntervalRunsFromSessionsLastReflection(t *testing.T) {
	now := time.Now()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	add := func(key string, born time.Time) {
		idx.Upsert(session.SessionIndexEntry{
			SessionKey: key, FilePath: "/tmp/" + key, CreatedAt: born, LastActivityAt: born,
			SessionType: session.SessionTypeChat, Status: session.SessionStatusActive,
		})
		idx.UpdateActivity(key, now.Add(-time.Minute)) // activity since any stamp
	}
	add("test/c1", now.Add(-24*time.Hour))
	idx.StampReflection("test/c1", now.Add(-10*time.Minute)) // the pre-compaction pass
	add("test/c2", now.Add(-24*time.Hour))
	idx.StampReflection("test/c2", now.Add(-2*time.Hour))
	add("test/c3", now.Add(-20*time.Minute)) // new: only the birth seed

	var mu sync.Mutex
	var reflected []string
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		reflectCfg: config.ResolvedReflection{
			IntervalEnabled: true,
			Interval:        "1h",
			IntervalPrompt:  "reflection.md",
		},
		sessionIndex:    idx,
		lastInteraction: now.Add(-time.Minute),
		lastReflection:  now.Add(-2 * time.Hour),
		agent: &fakeBackgroundAgent{
			branchFn: func(_, parentKey, _ string, _ bool) bool {
				mu.Lock()
				reflected = append(reflected, parentKey)
				mu.Unlock()
				return true
			},
		},
		done: make(chan struct{}),
	}

	r.maybeReflection()
	waitIdle(t, r)

	mu.Lock()
	defer mu.Unlock()
	sort.Strings(reflected)
	if len(reflected) != 2 || reflected[0] != "test/c2" || reflected[1] != "test/c3" {
		t.Fatalf("reflected %v, want [test/c2 test/c3]: test/c1 was reflected 10 min ago and is not due for another 50", reflected)
	}
}

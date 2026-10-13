package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func tempCheckboxStore(t *testing.T) *checkboxStore {
	t.Helper()
	s, err := newCheckboxStore(filepath.Join(t.TempDir(), "app-checkboxes.db"))
	if err != nil {
		t.Fatalf("newCheckboxStore: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// seedCheckboxDocs fills the store to exactly n documents by direct SQL in one
// transaction — the fast seeding path for cap tests, where n is the resolved
// document cap
// and put()ing each row would be ten thousand round-trips.
func seedCheckboxDocs(t *testing.T, s *checkboxStore, n int, nowMs int64) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("seed begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO app_checkboxes (doc_key, state, updated_ms) VALUES (?, ?, ?)`)
	if err != nil {
		t.Fatalf("seed prepare: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("doc-%05d", i), "[1]", nowMs); err != nil {
			t.Fatalf("seed exec: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
}

// storedCheckboxState decodes one document's stored state, failing the test if
// the row is absent or unreadable.
func storedCheckboxState(t *testing.T, s *checkboxStore, key string) []int {
	t.Helper()
	var state string
	err := s.db.QueryRow(`SELECT state FROM app_checkboxes WHERE doc_key = ?`, key).Scan(&state)
	if err != nil {
		t.Fatalf("stored state for %q: %v", key, err)
	}
	lines, ok := decodeCheckboxState(state)
	if !ok {
		t.Fatalf("stored state for %q = %q, undecodable", key, state)
	}
	return lines
}

// TestCheckboxStore_PutThenReplayRoundTrip proves a put is stored and read
// back whole, that a second put REPLACES the first (no merge), and that the
// replay orders most-recently-updated first.
func TestCheckboxStore_PutThenReplayRoundTrip(t *testing.T) {
	s := tempCheckboxStore(t)
	now := time.Now().UnixMilli()

	if err := s.put("doc-a", encodeCheckboxState([]int{1, 2}), now-2000); err != nil {
		t.Fatalf("put doc-a: %v", err)
	}
	if err := s.put("doc-b", encodeCheckboxState([]int{5}), now-1000); err != nil {
		t.Fatalf("put doc-b: %v", err)
	}
	got := s.replay(10, 0)
	if len(got) != 2 || got[0].key != "doc-b" || got[1].key != "doc-a" {
		t.Fatalf("replay = %+v, want doc-b (newest) then doc-a", got)
	}

	// A second put replaces: last-write-wins by arrival, never a merge.
	if err := s.put("doc-a", encodeCheckboxState([]int{9}), now); err != nil {
		t.Fatalf("re-put doc-a: %v", err)
	}
	if lines := storedCheckboxState(t, s, "doc-a"); len(lines) != 1 || lines[0] != 9 {
		t.Errorf("doc-a after re-put = %v, want [9] with no merge of [1 2]", lines)
	}
	got = s.replay(10, 0)
	if got[0].key != "doc-a" {
		t.Errorf("re-put must move a document to the front of the replay, got %+v", got)
	}
}

// TestCheckboxStore_EmptyPutDeletes proves an empty state is a delete, not a
// stored empty row: the document leaves the store (nothing for a replay to
// send) and the delete is idempotent for a key that is not stored.
func TestCheckboxStore_EmptyPutDeletes(t *testing.T) {
	s := tempCheckboxStore(t)
	if err := s.put("doc-a", "[1]", time.Now().UnixMilli()); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := s.put("doc-a", "", time.Now().UnixMilli()); err != nil {
		t.Fatalf("empty put: %v", err)
	}
	if got := s.replay(10, 0); len(got) != 0 {
		t.Errorf("replay after clear = %+v, want none", got)
	}
	if err := s.put("ghost", "", time.Now().UnixMilli()); err != nil {
		t.Errorf("empty put for an absent key must be a no-op success, got %v", err)
	}
}

// TestCheckboxStore_ReopenOnSamePathPersists proves the state survives a store
// rebuild on the same file — the gateway-restart case (requirement 2).
func TestCheckboxStore_ReopenOnSamePathPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-checkboxes.db")
	s1, err := newCheckboxStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s1.put("doc-a", "[3,7]", time.Now().UnixMilli()); err != nil {
		t.Fatalf("put: %v", err)
	}
	s1.Close()

	s2, err := newCheckboxStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(s2.Close)
	if lines := storedCheckboxState(t, s2, "doc-a"); len(lines) != 2 || lines[0] != 3 || lines[1] != 7 {
		t.Errorf("reopened state = %v, want [3 7]", lines)
	}
}

// TestCheckboxStore_CapRefusesNewDocumentAllowsExisting proves the 10000-doc
// cap: a NEW key at the cap is refused with errCheckboxCap and not stored,
// while an EXISTING key is always updatable (the spec's "existing document
// can always be updated" rule).
func TestCheckboxStore_CapRefusesNewDocumentAllowsExisting(t *testing.T) {
	s := tempCheckboxStore(t)
	seedCheckboxDocs(t, s, defaultCheckboxDocCap, time.Now().UnixMilli())

	err := s.put("doc-new", "[1]", time.Now().UnixMilli())
	if !errors.Is(err, errCheckboxCap) {
		t.Fatalf("put new key at cap: err=%v, want errCheckboxCap", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM app_checkboxes WHERE doc_key = 'doc-new'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("refused put must not store: n=%d err=%v", n, err)
	}

	if err := s.put("doc-00042", "[8,9]", time.Now().UnixMilli()); err != nil {
		t.Fatalf("put existing key at cap: %v, want success", err)
	}
	if lines := storedCheckboxState(t, s, "doc-00042"); len(lines) != 2 || lines[0] != 8 || lines[1] != 9 {
		t.Errorf("existing key at cap = %v, want [8 9]", lines)
	}
}

// TestCheckboxStore_CapAllowsCorruptExisting is review finding B's fail-arm
// target: whether a document exists must not depend on whether its stored
// state decodes. A corrupt row still counts as existing, so at the cap its
// update REPLACES it (no cap error) and leaves a valid entry; a NEW key at the
// cap is still refused.
func TestCheckboxStore_CapAllowsCorruptExisting(t *testing.T) {
	s := tempCheckboxStore(t)
	seedCheckboxDocs(t, s, defaultCheckboxDocCap, time.Now().UnixMilli())
	if _, err := s.db.Exec(`UPDATE app_checkboxes SET state = 'not json' WHERE doc_key = 'doc-00042'`); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}

	if err := s.put("doc-00042", encodeCheckboxState([]int{4, 6}), time.Now().UnixMilli()); err != nil {
		t.Fatalf("put existing corrupt key at cap: err=%v, want success (existence is row presence, not state validity)", err)
	}
	if lines := storedCheckboxState(t, s, "doc-00042"); len(lines) != 2 || lines[0] != 4 || lines[1] != 6 {
		t.Errorf("corrupt entry after replace = %v, want [4 6]", lines)
	}

	if err := s.put("doc-new", "[1]", time.Now().UnixMilli()); !errors.Is(err, errCheckboxCap) {
		t.Fatalf("put new key at cap: err=%v, want errCheckboxCap still refused", err)
	}
}

// TestCheckboxStore_ConcurrentPutsRespectCapAndStayConsistent proves the
// mutex's two guarantees under concurrency: racing puts of NEW keys can never
// overshoot the cap (the count check and the insert are one critical section),
// and racing puts on ONE key leave exactly one row holding one of the written
// states (never a torn or duplicated row).
func TestCheckboxStore_ConcurrentPutsRespectCapAndStayConsistent(t *testing.T) {
	s := tempCheckboxStore(t)
	now := time.Now().UnixMilli()
	seedCheckboxDocs(t, s, defaultCheckboxDocCap-50, now)

	// 8 goroutines × 20 distinct NEW keys for the 50 slots below the cap.
	var wg sync.WaitGroup
	errs := make(chan error, 8*20)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				errs <- s.put(fmt.Sprintf("r-%d-%d", g, i), "[1]", now)
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, errCheckboxCap) {
			t.Fatalf("concurrent put: unexpected error %v", err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM app_checkboxes`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != defaultCheckboxDocCap {
		t.Errorf("document count after concurrent puts = %d, want exactly %d (no overshoot, no refusal gap)", n, defaultCheckboxDocCap)
	}

	// Same key from every goroutine: one row, one coherent state afterwards.
	s2 := tempCheckboxStore(t)
	wg = sync.WaitGroup{}
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_ = s2.put("shared", encodeCheckboxState([]int{g, i}), now)
			}
		}(g)
	}
	wg.Wait()
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM app_checkboxes`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows for the shared key = %d (err=%v), want 1", n, err)
	}
	lines := storedCheckboxState(t, s2, "shared")
	if len(lines) == 0 || !linesAreSortedSet(lines) {
		t.Errorf("shared key state = %v, want a sorted set (one coherent write won)", lines)
	}
}

func linesAreSortedSet(lines []int) bool {
	for i := 1; i < len(lines); i++ {
		if lines[i] <= lines[i-1] {
			return false
		}
	}
	return true
}

// TestCheckboxStore_RemoveOlderThan proves the sweep's cutoff: only documents
// updated before the cutoff go, and the return counts them.
func TestCheckboxStore_RemoveOlderThan(t *testing.T) {
	s := tempCheckboxStore(t)
	now := time.Now().UnixMilli()
	if err := s.put("old-1", "[1]", now-(defaultCheckboxRetention+time.Second).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	if err := s.put("old-2", "[1]", now-(defaultCheckboxRetention+time.Hour).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	if err := s.put("new-1", "[1]", time.Now().Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	if n := s.removeOlderThan(now - defaultCheckboxRetention.Milliseconds()); n != 2 {
		t.Errorf("removeOlderThan removed %d, want 2", n)
	}
	got := s.replay(10, 0)
	if len(got) != 1 || got[0].key != "new-1" {
		t.Errorf("replay after sweep = %+v, want only new-1", got)
	}
}

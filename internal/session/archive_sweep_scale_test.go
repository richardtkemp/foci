package session

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sweepRow inserts an index-only row (no session file: gzipFile treats a
// missing file as already archived) with explicit timestamps and parent.
func sweepRow(t testing.TB, idx *SessionIndex, dir, key, parent string, created, active time.Time) {
	t.Helper()
	idx.Upsert(SessionIndexEntry{
		SessionKey:       key,
		FilePath:         filepath.Join(dir, strings.ReplaceAll(key, "/", "_")+".jsonl"),
		CreatedAt:        created,
		LastActivityAt:   active,
		ParentSessionKey: parent,
		SessionType:      SessionTypeChat,
		Status:           SessionStatusActive,
	})
}

func sweepStatus(t *testing.T, idx *SessionIndex, key string) SessionStatus {
	t.Helper()
	e, err := idx.Get(key)
	if err != nil {
		t.Fatalf("Get %s: %v", key, err)
	}
	return e.Status
}

// #3079: several candidates share parents, in one sweep. Branch protection
// must hold for each: a parent with any active child stays; a parent whose
// children are all inactive is archived; and a child archived earlier in the
// SAME sweep no longer protects its parent (the per-candidate check is live,
// not a snapshot taken at sweep start).
func TestArchiveSweep_SeveralCandidatesSharingParents(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	idx := tempIndex(t)

	now := time.Now().UTC()
	old := now.Add(-72 * time.Hour)
	at := func(h int) time.Time { return old.Add(time.Duration(h) * time.Minute) }

	// P1: two old children and one recent active child -> P1 stays; the old
	// children (no children of their own) are archived.
	sweepRow(t, idx, dir, "bot/c100", "", at(0), old)
	sweepRow(t, idx, dir, "bot/c100/b1000000001", "bot/c100", at(1), old)
	sweepRow(t, idx, dir, "bot/c100/b1000000002", "bot/c100", at(2), old)
	sweepRow(t, idx, dir, "bot/c100/b1000000003", "bot/c100", at(3), now)

	// P2: children already archived / cleared -> P2 archived.
	sweepRow(t, idx, dir, "bot/c200", "", at(4), old)
	sweepRow(t, idx, dir, "bot/c200/b2000000001", "bot/c200", at(5), old)
	sweepRow(t, idx, dir, "bot/c200/b2000000002", "bot/c200", at(6), old)
	idx.UpdateStatus("bot/c200/b2000000001", SessionStatusArchived)
	idx.UpdateStatus("bot/c200/b2000000002", SessionStatusCleared)

	// Chain G -> M -> L(recent): M is protected by L, G by M.
	sweepRow(t, idx, dir, "bot/c300", "", at(7), old)
	sweepRow(t, idx, dir, "bot/c300/b3000000001", "bot/c300", at(8), old)
	sweepRow(t, idx, dir, "bot/c300/b3000000001/b3000000002", "bot/c300/b3000000001", at(9), now)

	// P4: its only child is old and created later, so the sweep (newest
	// first) archives the child before reaching P4; P4 is then archived in
	// the same sweep, as before #3079.
	sweepRow(t, idx, dir, "bot/c400", "", at(10), old)
	sweepRow(t, idx, dir, "bot/c400/b4000000001", "bot/c400", at(11), old)

	if _, err := ArchiveSweep(store, idx, 24*time.Hour); err != nil {
		t.Fatalf("ArchiveSweep: %v", err)
	}

	want := map[string]SessionStatus{
		"bot/c100":                         SessionStatusActive,
		"bot/c100/b1000000001":             SessionStatusArchived,
		"bot/c100/b1000000002":             SessionStatusArchived,
		"bot/c100/b1000000003":             SessionStatusActive,
		"bot/c200":                         SessionStatusArchived,
		"bot/c300":                         SessionStatusActive,
		"bot/c300/b3000000001":             SessionStatusActive,
		"bot/c300/b3000000001/b3000000002": SessionStatusActive,
		"bot/c400":                         SessionStatusArchived,
		"bot/c400/b4000000001":             SessionStatusArchived,
	}
	for key, w := range want {
		if got := sweepStatus(t, idx, key); got != w {
			t.Errorf("%s: status %q, want %q", key, got, w)
		}
	}
}

// BenchmarkArchiveSweep measures one sweep over a synthetic index:
// 5000 sessions, 500 of them archive candidates (old, no active child),
// plus 100 old parents protected by a recent active child.
// Run: GOFLAGS='-bench=BenchmarkArchiveSweep -benchtime=5x' make local-test-one PKG=./internal/session/ RUN=NONE
func BenchmarkArchiveSweep(b *testing.B) {
	dir := b.TempDir()
	store := NewStore(dir)
	idx, err := NewSessionIndex(filepath.Join(dir, "bench_index.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer idx.Close() // nolint:errcheck

	now := time.Now().UTC()
	old := now.Add(-72 * time.Hour)
	const total, candidates, protected = 5000, 500, 100
	i := 0
	for ; i < candidates; i++ {
		sweepRow(b, idx, dir, fmt.Sprintf("bot/c%d", i+1), "", old.Add(time.Duration(i)*time.Second), old)
	}
	for p := 0; p < protected; p++ {
		parent := fmt.Sprintf("bot/c%d", i+1)
		sweepRow(b, idx, dir, parent, "", old.Add(time.Duration(i)*time.Second), old)
		i++
		sweepRow(b, idx, dir, fmt.Sprintf("%s/b%d", parent, 1000000000+p), parent, old.Add(time.Duration(i)*time.Second), now)
		i++
	}
	for ; i < total; i++ {
		sweepRow(b, idx, dir, fmt.Sprintf("bot/c%d", i+1), "", old.Add(time.Duration(i)*time.Second), now)
	}

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		b.StopTimer()
		if _, err := idx.db.Exec(`UPDATE session_index SET status = 'active'`); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		got, err := ArchiveSweep(store, idx, 24*time.Hour)
		if err != nil {
			b.Fatal(err)
		}
		if got != candidates {
			b.Fatalf("archived %d, want %d", got, candidates)
		}
	}
}

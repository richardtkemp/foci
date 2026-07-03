package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/timeutil"
)

func newProvenanceIndex(t *testing.T) *SessionIndex {
	t.Helper()
	idx, err := NewSessionIndex(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

// seedArchive inserts a session_archives row with a controlled timestamp.
func seedArchive(t *testing.T, idx *SessionIndex, key string, at time.Time, path string) {
	t.Helper()
	if _, err := idx.db.Exec(
		`INSERT INTO session_archives (session_key, archived_at, file_path, reason) VALUES (?, ?, ?, 'compaction')`,
		key, timeutil.Format(at), path,
	); err != nil {
		t.Fatal(err)
	}
}

// TestArchiveFileAt proves the point-in-time archive lookup: a moment before
// the first rotation resolves to the earliest archive at-or-after it (the
// file holding history up to its stamp), a moment between rotations resolves
// to the later archive, and a moment after the last rotation resolves to
// nothing (the live file covers it).
func TestArchiveFileAt(t *testing.T) {
	idx := newProvenanceIndex(t)
	t1 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	seedArchive(t, idx, "main/c1", t1, "/s/main/c1/root.A.jsonl")
	seedArchive(t, idx, "main/c1", t2, "/s/main/c1/root.B.jsonl")

	if p, at, ok := idx.ArchiveFileAt("main/c1", t1.Add(-time.Hour)); !ok || p != "/s/main/c1/root.A.jsonl" || !at.Equal(t1) {
		t.Errorf("before first rotation: (%q, %v, %v), want archive A", p, at, ok)
	}
	if p, _, ok := idx.ArchiveFileAt("main/c1", t1.Add(time.Hour)); !ok || p != "/s/main/c1/root.B.jsonl" {
		t.Errorf("between rotations: (%q, %v), want archive B", p, ok)
	}
	if _, _, ok := idx.ArchiveFileAt("main/c1", t2.Add(time.Hour)); ok {
		t.Error("after last rotation: expected no archive (live file covers it)")
	}
	if _, _, ok := idx.ArchiveFileAt("other/c9", t1); ok {
		t.Error("unknown session: expected no archive")
	}
}

// TestRecordArchive proves RecordArchive rows are retrievable through
// ArchiveFileAt for a moment before the rotation.
func TestRecordArchive(t *testing.T) {
	idx := newProvenanceIndex(t)
	idx.RecordArchive("main/c2", "/s/main/c2/root.X.jsonl", "reset")
	if p, _, ok := idx.ArchiveFileAt("main/c2", time.Now().Add(-time.Minute)); !ok || p != "/s/main/c2/root.X.jsonl" {
		t.Errorf("recorded archive not found: (%q, %v)", p, ok)
	}
}

// TestCCResumeHistory proves the resume-ID timeline: observations are
// retrievable by moment (newest at-or-before wins), moments before the first
// observation resolve to nothing, and consecutive duplicate observations are
// collapsed into one row.
func TestCCResumeHistory(t *testing.T) {
	idx := newProvenanceIndex(t)
	t1 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		at time.Time
		id string
	}{{t1, "uuid-1"}, {t2, "uuid-2"}} {
		if _, err := idx.db.Exec(
			`INSERT INTO cc_resume_history (session_key, observed_at, resume_id) VALUES (?, ?, ?)`,
			"main/c1", timeutil.Format(row.at), row.id,
		); err != nil {
			t.Fatal(err)
		}
	}

	if id, at, ok := idx.CCResumeAt("main/c1", t1.Add(time.Hour)); !ok || id != "uuid-1" || !at.Equal(t1) {
		t.Errorf("during uuid-1 era: (%q, %v, %v)", id, at, ok)
	}
	if id, _, ok := idx.CCResumeAt("main/c1", t2.Add(time.Hour)); !ok || id != "uuid-2" {
		t.Errorf("during uuid-2 era: (%q, %v)", id, ok)
	}
	if _, _, ok := idx.CCResumeAt("main/c1", t1.Add(-time.Hour)); ok {
		t.Error("before first observation: expected none")
	}

	// Duplicate collapse: recording the latest ID again adds no row.
	idx.RecordCCResume("main/c1", "uuid-2")
	var n int
	if err := idx.db.QueryRow(`SELECT COUNT(*) FROM cc_resume_history WHERE session_key = 'main/c1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rows after duplicate record = %d, want 2", n)
	}
	// A genuinely new ID appends.
	idx.RecordCCResume("main/c1", "uuid-3")
	if id, _, ok := idx.CCResumeAt("main/c1", time.Now().Add(time.Minute)); !ok || id != "uuid-3" {
		t.Errorf("after new observation: (%q, %v), want uuid-3", id, ok)
	}
}

// TestStoreArchiveFileAt proves the filesystem fallback: archive filename
// stamps alone (current zone-offset format, legacy Z format, counter and .gz
// variants) answer the same point-in-time query without state.db.
func TestStoreArchiveFileAt(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	sessDir := filepath.Join(dir, "main", "c1")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}

	t1 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	nameA := "root." + t1.Format("2006-01-02T15-04-05Z") + ".jsonl" // legacy Z stamp
	nameB := "root." + t2.Format("2006-01-02T15-04-05-0700") + ".2.jsonl.gz"
	for _, name := range []string{"root.jsonl", nameA, nameB} {
		if err := os.WriteFile(filepath.Join(sessDir, name), []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	if p, at, ok := store.ArchiveFileAt("main/c1", t1.Add(-time.Hour)); !ok || filepath.Base(p) != nameA || !at.Equal(t1) {
		t.Errorf("before first rotation: (%q, %v, %v), want %s", p, at, ok, nameA)
	}
	if p, _, ok := store.ArchiveFileAt("main/c1", t1.Add(time.Hour)); !ok || filepath.Base(p) != nameB {
		t.Errorf("between rotations: (%q, %v), want %s", p, ok, nameB)
	}
	if _, _, ok := store.ArchiveFileAt("main/c1", t2.Add(time.Hour)); ok {
		t.Error("after last rotation: expected live-file answer (no archive)")
	}
}

package defersend

import (
	"path/filepath"
	"testing"
	"time"

	"foci/internal/sqlite"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "deferred.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStore_Roundtrip(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	id, err := s.Enqueue(Record{
		AgentID: "clutch", SessionKey: "clutch/c1", Text: "hi", Policy: "fallback",
		WaitCold: "1m", CreatedAt: now, DeadlineAt: now.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("len=%d, want 1", len(all))
	}
	r := all[0]
	if r.SessionKey != "clutch/c1" || r.WaitCold != "1m" || r.Text != "hi" {
		t.Errorf("roundtrip mismatch: %+v", r)
	}
	if !r.DeadlineAt.Equal(now.Add(2 * time.Hour)) {
		t.Errorf("deadline = %v, want %v", r.DeadlineAt, now.Add(2*time.Hour))
	}

	if err := s.Delete(id); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.All(); len(all) != 0 {
		t.Errorf("count after delete = %d, want 0", len(all))
	}
}

// TestStore_KindAndBranchOptionsRoundtrip proves the queue remembers WHAT was
// deferred — a branch record keeps its kind, branch options and model override
// (#1272) — and that an empty Kind (the pre-kind wire default) normalises to a
// send through EffectiveKind, the single "" → send mapping.
func TestStore_KindAndBranchOptionsRoundtrip(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := s.Enqueue(Record{
		Kind: KindBranch, AgentID: "helen", SessionKey: "helen/i0", Text: "morning check",
		Model: "fast", NoCompact: true, NoResetHook: true, Silent: true,
		WaitCold: "55m", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("len=%d, want 1", len(all))
	}
	r := all[0]
	if r.Kind != KindBranch || !r.NoCompact || !r.NoResetHook || !r.Silent {
		t.Errorf("kind/branch options roundtrip mismatch: %+v", r)
	}
	if r.Model != "fast" || r.Text != "morning check" || r.SessionKey != "helen/i0" {
		t.Errorf("send-field roundtrip mismatch: %+v", r)
	}

	// An empty Kind is the legacy send shape: EffectiveKind is the ONE
	// normaliser, so consumers never map "" themselves.
	legacy := Record{AgentID: "a", SessionKey: "a/i0", Text: "x"}
	if legacy.EffectiveKind() != KindSend {
		t.Errorf("empty Kind EffectiveKind() = %q, want %q", legacy.EffectiveKind(), KindSend)
	}
	if r.EffectiveKind() != KindBranch {
		t.Errorf("branch EffectiveKind() = %q, want %q", r.EffectiveKind(), KindBranch)
	}
}

// TestStore_OldSchemaMigration proves NewStore opens a database created by the
// pre-kind code (no kind/no_compact/no_reset_hook/silent columns): the old
// rows survive and read back as Kind "" — i.e. plain sends.
func TestStore_OldSchemaMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE deferred_sends (
			id                 INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_id           TEXT NOT NULL,
			session_key        TEXT NOT NULL,
			text               TEXT NOT NULL,
			policy             TEXT NOT NULL DEFAULT '',
			model              TEXT NOT NULL DEFAULT '',
			wait_warm          TEXT NOT NULL DEFAULT '',
			wait_cold          TEXT NOT NULL DEFAULT '',
			wait_user_active   TEXT NOT NULL DEFAULT '',
			wait_user_inactive TEXT NOT NULL DEFAULT '',
			created_at         TEXT NOT NULL,
			deadline_at        TEXT NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO deferred_sends
		(agent_id, session_key, text, policy, model, wait_warm, wait_cold, wait_user_active, wait_user_inactive, created_at, deadline_at)
		VALUES ('clutch', 'clutch/i0', 'queued before kinds', '', '', '', '1m', '', '', '2026-01-01T00:00:00Z', '2026-01-01T02:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore on old-schema db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("old row lost: len=%d, want 1", len(all))
	}
	r := all[0]
	if r.Text != "queued before kinds" || r.SessionKey != "clutch/i0" {
		t.Errorf("old row corrupted: %+v", r)
	}
	if r.EffectiveKind() != KindSend {
		t.Errorf("old row EffectiveKind() = %q, want %q (a plain send)", r.EffectiveKind(), KindSend)
	}

	// The migrated store accepts new-kind rows alongside the old one.
	if _, err := s.Enqueue(Record{Kind: KindCommand, AgentID: "clutch", SessionKey: "clutch/i0", Text: "/reset", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.All(); len(all) != 2 {
		t.Errorf("len=%d, want 2 (old row kept + new row)", len(all))
	}
}

// TestStore_AlterFailureSurfaces proves NewStore fails loudly when a migration
// ALTER fails for a real reason (not the expected "duplicate column"): here
// the deferred_sends name is taken by a view, so every ADD COLUMN fails — the
// error must surface at OPEN, not later as a "no such column" on the first
// Enqueue/All.
func TestStore_AlterFailureSurfaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deferred.db")
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE VIEW deferred_sends AS SELECT 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if s, err := NewStore(path); err == nil {
		_ = s.Close()
		t.Fatal("NewStore succeeded although the kind columns could not be added — the ALTER failure was swallowed")
	}
}

// TestStore_ReopenSameFile proves opening the same database twice (a restart,
// or a second handle) neither fails nor loses the queued rows.
func TestStore_ReopenSameFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deferred.db")
	s1, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Enqueue(Record{Kind: KindBranch, AgentID: "a", SessionKey: "a/i0", Text: "br"}); err != nil {
		t.Fatal(err)
	}

	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("second NewStore on the same file: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	t.Cleanup(func() { _ = s1.Close() })

	all, err := s2.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Kind != KindBranch {
		t.Errorf("reopen lost the branch record: %+v", all)
	}
}

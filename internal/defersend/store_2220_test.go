package defersend

import (
	"path/filepath"
	"testing"
	"time"

	"foci/internal/sqlite"
)

// dedupeBase is a fully populated record; each identity field is varied
// from it in TestStore_EnqueueDedupe_AnyIdentityFieldDiffers.
func dedupeBase(now time.Time) Record {
	return Record{
		Kind: KindCommand, AgentID: "a", SessionKey: "a/i0", Text: "/reset",
		Policy: "fallback", Model: "m",
		WaitWarm: "", WaitCold: "55m", WaitUserActive: "", WaitUserInactive: "",
		CreatedAt: now, DeadlineAt: now.Add(24 * time.Hour),
	}
}

// TestStore_EnqueueDedupe_Identical proves #2220's dedupe: an identical
// pending request is REPLACED — one record remains, it is the newer one
// (new id, its own created time and deadline).
func TestStore_EnqueueDedupe_Identical(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	first, _, err := s.Enqueue(dedupeBase(now))
	if err != nil {
		t.Fatal(err)
	}
	second := dedupeBase(now.Add(15 * time.Minute))
	id2, replaced, err := s.Enqueue(second)
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced) != 1 || replaced[0] != first {
		t.Errorf("replaced = %v, want [%d]", replaced, first)
	}
	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("records = %d, want 1 (identical requests must not pile up)", len(all))
	}
	if all[0].ID != id2 || id2 == first {
		t.Errorf("kept id %d, want the newer %d", all[0].ID, id2)
	}
	if !all[0].CreatedAt.Equal(second.CreatedAt) || !all[0].DeadlineAt.Equal(second.DeadlineAt) {
		t.Errorf("kept created=%v deadline=%v, want the newer record's %v / %v",
			all[0].CreatedAt, all[0].DeadlineAt, second.CreatedAt, second.DeadlineAt)
	}
}

// TestStore_EnqueueDedupe_AnyIdentityFieldDiffers proves that a
// different value in any identity field is a different request.
func TestStore_EnqueueDedupe_AnyIdentityFieldDiffers(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for name, mut := range map[string]func(*Record){
		"kind":               func(r *Record) { r.Kind = KindBranch },
		"agent":              func(r *Record) { r.AgentID = "b" },
		"session":            func(r *Record) { r.SessionKey = "a/i1" },
		"text":               func(r *Record) { r.Text = "/compact" },
		"model":              func(r *Record) { r.Model = "other" },
		"policy":             func(r *Record) { r.Policy = "strict" },
		"wait_warm":          func(r *Record) { r.WaitWarm = "1m" },
		"wait_cold":          func(r *Record) { r.WaitCold = "56m" },
		"wait_user_active":   func(r *Record) { r.WaitUserActive = "1m" },
		"wait_user_inactive": func(r *Record) { r.WaitUserInactive = "1m" },
		"no_compact":         func(r *Record) { r.NoCompact = true },
		"no_reset_hook":      func(r *Record) { r.NoResetHook = true },
		"silent":             func(r *Record) { r.Silent = true },
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			if _, _, err := s.Enqueue(dedupeBase(now)); err != nil {
				t.Fatal(err)
			}
			r := dedupeBase(now)
			mut(&r)
			if _, replaced, err := s.Enqueue(r); err != nil || len(replaced) != 0 {
				t.Fatalf("replaced=%v err=%v, want nothing replaced", replaced, err)
			}
			if all, _ := s.All(); len(all) != 2 {
				t.Errorf("records = %d, want 2", len(all))
			}
		})
	}
}

// TestStore_EnqueueDedupe_LegacySendRow proves a legacy row (kind "",
// written before the kind column) is a send for dedupe, as everywhere else.
func TestStore_EnqueueDedupe_LegacySendRow(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	legacy := Record{AgentID: "a", SessionKey: "a/i0", Text: "hi", WaitCold: "1m", CreatedAt: now, DeadlineAt: now.Add(time.Hour)}
	if _, _, err := s.Enqueue(legacy); err != nil {
		t.Fatal(err)
	}
	legacy.Kind = KindSend
	if _, replaced, err := s.Enqueue(legacy); err != nil || len(replaced) != 1 {
		t.Fatalf("replaced=%v err=%v, want the legacy send replaced", replaced, err)
	}
}

// TestStore_FailedAttemptsPersist proves the transient-failure count
// survives reopening the store (a gateway restart).
func TestStore_FailedAttemptsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deferred.db")
	s1, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := s1.Enqueue(Record{AgentID: "a", SessionKey: "a/i0", Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 2; want++ {
		if n, err := s1.RecordFailedAttempt(id); err != nil || n != want {
			t.Fatalf("RecordFailedAttempt = %d, %v; want %d", n, err, want)
		}
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	all, err := s2.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Attempts != 2 {
		t.Fatalf("after reopen: %+v, want one record with Attempts=2", all)
	}
	if n, err := s2.RecordFailedAttempt(id); err != nil || n != 3 {
		t.Errorf("third RecordFailedAttempt = %d, %v; want 3", n, err)
	}
}

// TestStore_PreAttemptsSchemaMigration proves a store file written with the
// schema just before #2220 (no attempts column) opens, its row reads back
// with Attempts 0, and the new column then works.
func TestStore_PreAttemptsSchemaMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deferred.db")
	createPreAttemptsFixture(t, path)
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore on a pre-attempts db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Attempts != 0 || all[0].Text != "queued before attempts" {
		t.Fatalf("migrated rows = %+v", all)
	}
	if n, err := s.RecordFailedAttempt(all[0].ID); err != nil || n != 1 {
		t.Errorf("RecordFailedAttempt on migrated row = %d, %v", n, err)
	}
}

// createPreAttemptsFixture writes a store file with the deferred_sends
// schema as it was immediately before #2220 (every column but attempts) and
// one pending send row (wait_cold 1m, deadline far ahead).
func createPreAttemptsFixture(t *testing.T, path string) {
	t.Helper()
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
			steer              TEXT NOT NULL DEFAULT '',
			wait_warm          TEXT NOT NULL DEFAULT '',
			wait_cold          TEXT NOT NULL DEFAULT '',
			wait_user_active   TEXT NOT NULL DEFAULT '',
			wait_user_inactive TEXT NOT NULL DEFAULT '',
			created_at         TEXT NOT NULL,
			deadline_at        TEXT NOT NULL,
			kind               TEXT NOT NULL DEFAULT '',
			no_compact         INTEGER NOT NULL DEFAULT 0,
			no_reset_hook      INTEGER NOT NULL DEFAULT 0,
			silent             INTEGER NOT NULL DEFAULT 0
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO deferred_sends
		(agent_id, session_key, text, policy, wait_cold, kind, created_at, deadline_at)
		VALUES ('clutch', 'clutch/i0', 'queued before attempts', 'fallback', '1m', 'send', '2026-01-01T00:00:00Z', '2099-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

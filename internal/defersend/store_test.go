package defersend

import (
	"path/filepath"
	"testing"
	"time"
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

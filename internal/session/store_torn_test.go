package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/provider"
)

// appendRaw writes raw bytes to the end of a session file, as a write cut off
// by a crash or power loss would leave them.
func appendRaw(t *testing.T, path, raw string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(raw); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func tornSidecars(t *testing.T, path string) []string {
	t.Helper()
	m, err := filepath.Glob(path + ".torn-*")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// #3065 F13: a final record cut off part way must not cost the session its
// whole history. Load keeps the intact prefix, the torn bytes are kept in a
// sidecar, and the next append lands on a clean line.
func TestLoadRecoversTornFinalRecord(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/imain"
	if err := s.TestAppend(key, msg("user", "intact one")); err != nil {
		t.Fatal(err)
	}
	if err := s.TestAppend(key, msg("assistant", "intact two")); err != nil {
		t.Fatal(err)
	}
	path, _ := s.SessionPath(key)
	torn := `{"role":"user","content":[{"type":"text","te`
	appendRaw(t, path, torn)

	got, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load after torn tail: %v", err)
	}
	if len(got) != 2 || got[1].Content[0].Text != "intact two" {
		t.Fatalf("Load = %+v, want the two intact messages", got)
	}

	side := tornSidecars(t, path)
	if len(side) != 1 {
		t.Fatalf("sidecars = %v, want exactly one", side)
	}
	kept, err := os.ReadFile(side[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != torn {
		t.Fatalf("sidecar holds %q, want the torn bytes %q", kept, torn)
	}

	// The sidecar must not be taken for a session by the scans.
	entries, err := s.ScanAllSessions()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.FilePath, ".torn-") {
			t.Fatalf("ScanAllSessions indexed the sidecar: %+v", e)
		}
	}

	if err := s.TestAppend(key, msg("user", "after repair")); err != nil {
		t.Fatal(err)
	}
	got, err = s.Load(key)
	if err != nil {
		t.Fatalf("Load after append: %v", err)
	}
	if len(got) != 3 || got[2].Content[0].Text != "after repair" {
		t.Fatalf("Load = %+v, want 3 messages ending in the new one", got)
	}
}

// An append that comes before any Load (e.g. the first write after a restart)
// must not glue its record onto the torn one.
func TestAppendAfterTornTailStartsCleanLine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		append func(s *Store, key string) error
	}{
		{"Append", func(s *Store, key string) error { return s.TestAppend(key, msg("user", "new")) }},
		{"AppendAll", func(s *Store, key string) error {
			return s.TestAppendAll(key, []provider.Message{msg("user", "new")})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore(t.TempDir())
			key := "test/imain"
			if err := s.TestAppend(key, msg("user", "intact")); err != nil {
				t.Fatal(err)
			}
			path, _ := s.SessionPath(key)
			appendRaw(t, path, `{"role":"assist`)

			if err := tc.append(s, key); err != nil {
				t.Fatal(err)
			}
			got, err := s.Load(key)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(got) != 2 || got[1].Content[0].Text != "new" {
				t.Fatalf("Load = %+v, want [intact, new]", got)
			}
		})
	}
}

// A write cut off after the JSON but before its newline: the record is
// complete and must be kept, with the newline restored so the next append
// does not share its line.
func TestTailMissingOnlyNewlineIsKept(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/imain"
	if err := s.TestAppend(key, msg("user", "intact")); err != nil {
		t.Fatal(err)
	}
	path, _ := s.SessionPath(key)
	appendRaw(t, path, `{"role":"assistant","content":[{"type":"text","text":"complete"}]}`)

	if err := s.TestAppend(key, msg("user", "next")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 3 || got[1].Content[0].Text != "complete" || got[2].Content[0].Text != "next" {
		t.Fatalf("Load = %+v, want [intact, complete, next]", got)
	}
	if side := tornSidecars(t, path); len(side) != 0 {
		t.Fatalf("a complete record was quarantined: %v", side)
	}
}

// Interior corruption is not a torn tail: it must still fail loudly, and the
// file must be left exactly as it was.
func TestInteriorCorruptionStillFails(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/imain"
	if err := s.TestAppend(key, msg("user", "one")); err != nil {
		t.Fatal(err)
	}
	path, _ := s.SessionPath(key)
	appendRaw(t, path, "{not json}\n")
	if err := s.TestAppend(key, msg("user", "three")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Load(key); err == nil {
		t.Fatal("Load accepted a session with a corrupt interior record")
	}
	n, err := s.RepairOrphans()
	if err != nil || n != 0 {
		t.Fatalf("RepairOrphans = (%d, %v), want (0, nil): a corrupt session is reported, not fatal", n, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("interior corruption was rewritten; it must be left for a human")
	}
	if side := tornSidecars(t, path); len(side) != 0 {
		t.Fatalf("interior corruption was quarantined: %v", side)
	}
}

// RepairOrphans runs at startup, right after the crash that tore the record:
// it must leave the session loadable, and still repair an orphan tool_use
// that the torn record followed.
func TestRepairOrphansRepairsTornTail(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/imain"
	if err := s.TestAppend(key, msg("user", "hello")); err != nil {
		t.Fatal(err)
	}
	if err := s.TestAppend(key, toolUseMsg("toolu_9")); err != nil {
		t.Fatal(err)
	}
	path, _ := s.SessionPath(key)
	appendRaw(t, path, `{"role":"user","content":[{"type":"tool_result","tool_`)

	n, err := s.RepairOrphans()
	if err != nil || n != 1 {
		t.Fatalf("RepairOrphans = (%d, %v), want (1, nil)", n, err)
	}
	got, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4 (user, tool_use, repair, ack)", len(got))
	}
}

// #3065 F18: whatever Append accepts, Load must read back. A single valid
// message over the old 10 MiB reader cap made the whole session unreadable.
func TestOversizedRecordRoundTrips(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/imain"
	big := strings.Repeat("a", 11*1024*1024)
	if err := s.TestAppend(key, msg("user", "before")); err != nil {
		t.Fatal(err)
	}
	if err := s.TestAppend(key, msg("assistant", big)); err != nil {
		t.Fatal(err)
	}
	if err := s.TestAppend(key, msg("user", "after")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 3 || got[1].Content[0].Text != big || got[2].Content[0].Text != "after" {
		t.Fatalf("Load returned %d messages; oversized record did not round-trip", len(got))
	}
}

// A branch_meta line over the old 64 KiB first-line cap (a long orientation)
// must still be read as branch metadata.
func TestLargeBranchMetaIsRead(t *testing.T) {
	s := NewStore(t.TempDir())
	parent := "test/imain"
	if err := s.TestAppend(parent, msg("user", "p")); err != nil {
		t.Fatal(err)
	}
	orientation := strings.Repeat("o", 200*1024)
	key, err := s.CreateBranchWithOptions(parent, BranchOptions{OrientationTemplate: orientation})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := s.GetBranchMeta(key)
	if err != nil {
		t.Fatalf("GetBranchMeta: %v", err)
	}
	if meta == nil || meta.Orientation != orientation {
		t.Fatal("large branch_meta was not read as branch metadata")
	}
}

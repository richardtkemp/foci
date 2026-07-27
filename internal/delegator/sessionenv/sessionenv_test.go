package sessionenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uniqueID gives each case its own binding in the shared run temp dir (which
// FOCI_TMPDIR pins for the whole test binary — it resolves once, so a
// per-test override would silently apply to every other test too).
func uniqueID(t *testing.T) string {
	t.Helper()
	id := "test-" + strings.ReplaceAll(t.Name(), "/", "-")
	t.Cleanup(func() { _ = Remove(id) })
	return id
}

func TestEntryFrom_TakesOnlyBridgeVars(t *testing.T) {
	e := EntryFrom(map[string]string{
		"FOCI_SOCK":        "/tmp/x.sock",
		"BASH_ENV":         "/tmp/x-funcs.sh",
		"FOCI_SESSION_KEY": "agent/c1",
		"HOME":             "/home/nope",
	})
	if e.FociSock != "/tmp/x.sock" || e.BashEnv != "/tmp/x-funcs.sh" || e.SessionKey != "agent/c1" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	if e.IsZero() {
		t.Error("entry with bridge vars must not be zero")
	}
	if EntryFrom(map[string]string{"HOME": "/tmp"}).IsZero() != true {
		t.Error("entry with no bridge vars must be zero")
	}
}

func TestEntryVars_StableOrderAndSkipsEmpty(t *testing.T) {
	vars := Entry{SessionKey: "k", BashEnv: "b"}.Vars()
	if len(vars) != 2 {
		t.Fatalf("want 2 vars, got %d (%+v)", len(vars), vars)
	}
	if vars[0].Name != "FOCI_SESSION_KEY" || vars[1].Name != "BASH_ENV" {
		t.Errorf("unstable order: %+v", vars)
	}
}

func TestWriteLoadRemove_RoundTrip(t *testing.T) {
	id := uniqueID(t)
	env := map[string]string{"FOCI_SOCK": "/tmp/s.sock", "FOCI_SESSION_KEY": "agent/c7"}

	if err := Write(id, env); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Load(Dir(), id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.FociSock != "/tmp/s.sock" || got.SessionKey != "agent/c7" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if err := Remove(id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got, err := Load(Dir(), id); err != nil || !got.IsZero() {
		t.Fatalf("after Remove: entry=%+v err=%v; want zero entry, nil error", got, err)
	}
}

// A missing binding must read as "no override", not as a failure — every
// injector treats an error the same as absence, so a noisy error here would
// only hide real ones.
func TestLoad_MissingIsNotAnError(t *testing.T) {
	got, err := Load(Dir(), uniqueID(t))
	if err != nil || !got.IsZero() {
		t.Fatalf("entry=%+v err=%v; want zero entry, nil error", got, err)
	}
}

func TestWrite_SkipsEmptyIDAndEmptyEntry(t *testing.T) {
	id := uniqueID(t)

	if err := Write("", map[string]string{"FOCI_SOCK": "/tmp/x"}); err != nil {
		t.Fatalf("Write with empty id: %v", err)
	}
	if err := Write(id, map[string]string{"HOME": "/tmp"}); err != nil {
		t.Fatalf("Write with no bridge vars: %v", err)
	}
	if _, err := os.Stat(Path(id)); !os.IsNotExist(err) {
		t.Error("an entry with no bridge vars must not create a file")
	}
}

func TestRemove_MissingIsNotAnError(t *testing.T) {
	if err := Remove(uniqueID(t)); err != nil {
		t.Fatalf("Remove of missing file: %v", err)
	}
}

func TestEnsureFile_WritesThenSkipsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "artifact.ts")

	wrote, err := EnsureFile(path, "v1", 0o644)
	if err != nil || !wrote {
		t.Fatalf("first EnsureFile: wrote=%v err=%v", wrote, err)
	}
	wrote, err = EnsureFile(path, "v1", 0o644)
	if err != nil || wrote {
		t.Fatalf("unchanged EnsureFile must not rewrite: wrote=%v err=%v", wrote, err)
	}
	wrote, err = EnsureFile(path, "v2", 0o644)
	if err != nil || !wrote {
		t.Fatalf("stale EnsureFile must rewrite: wrote=%v err=%v", wrote, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "v2" {
		t.Fatalf("content=%q err=%v", data, err)
	}
}

func TestEnsureFile_AppliesPerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hook")
	if _, err := EnsureFile(path, "#!/bin/sh\n", 0o755); err != nil {
		t.Fatalf("EnsureFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("mode = %v, want executable", info.Mode())
	}
}

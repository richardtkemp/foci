package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/provider"
	"foci/internal/timeutil"
)

// TestLegacyKeyToStable proves the legacy→stable key conversion: versioned
// roots and children convert, while stable keys, derived bases, and
// non-key strings are rejected.
func TestLegacyKeyToStable(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"main/c123/1709590000", "main/c123", true},
		{"main/c123/1709590000/b1709596800", "main/c123/b1709596800", true},
		{"main/iwork/0", "main/iwork", true},
		{"main/i1709596800/1709596800", "main/i1709596800", true},
		{"main/c123", "", false},            // already stable
		{"main/c123/b1700", "", false},      // stable branch
		{"main/c123/1709/x1700", "", false}, // bad child type
		{"main/x123/1709", "", false},       // bad type
		{"/c123/1709", "", false},           // empty agent
		{"main", "", false},                 // not a key
		{"main/c1/1/b1/b2", "", false},      // too many segments
	}
	for _, c := range cases {
		got, ok := LegacyKeyToStable(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("LegacyKeyToStable(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

// writeJSONL writes a session_meta line plus one text message per entry.
func writeJSONL(t *testing.T, path string, texts ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString(`{"type":"session_meta","created_at":"2026-01-01T00:00:00Z"}` + "\n")
	for _, text := range texts {
		msg := provider.Message{Role: "user", Content: provider.TextContent(text)}
		data, _ := json.Marshal(msg)
		sb.Write(data)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateLegacyLayout proves the on-disk migration: the newest version's
// root becomes the live file, an older version's root becomes an archive
// stamped with the SUPERSEDING version's time (archive stamps mean "archived
// at"), children move up with their branch_meta parent keys rewritten to the
// stable form, version directories are removed, and a second run no-ops.
func TestMigrateLegacyLayout(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	// Legacy layout: two versions of main/c123 plus a branch in v1.
	v1 := filepath.Join(dir, "main", "c123", "1000")
	v2 := filepath.Join(dir, "main", "c123", "2000")
	writeJSONL(t, filepath.Join(v1, "root.jsonl"), "old-1", "old-2")
	writeJSONL(t, filepath.Join(v2, "root.jsonl"), "new-1")

	branchPath := filepath.Join(v1, "b1500.jsonl")
	meta := BranchMeta{Type: "branch_meta", ParentKey: "main/c123/1000", BranchPoint: 2}
	metaLine, _ := json.Marshal(meta)
	if err := os.WriteFile(branchPath, append(metaLine, '\n'), 0600); err != nil {
		t.Fatal(err)
	}

	n, err := store.MigrateLegacyLayout()
	if err != nil {
		t.Fatalf("MigrateLegacyLayout: %v", err)
	}
	if n != 1 {
		t.Fatalf("migrated = %d, want 1", n)
	}

	// Live root is the newest version's content.
	msgs, err := store.Load("main/c123")
	if err != nil || len(msgs) != 1 || provider.TextOf(msgs[0].Content) != "new-1" {
		t.Fatalf("live root after migration = %v msgs (%v), want [new-1]", len(msgs), err)
	}

	// The old root is archived with the superseding version's stamp.
	wantStamp := timeutil.FormatFilename(time.Unix(2000, 0))
	archived := filepath.Join(dir, "main", "c123", "root."+wantStamp+".jsonl")
	if _, err := os.Stat(archived); err != nil {
		t.Fatalf("expected archive %s: %v", archived, err)
	}

	// The branch moved up and its parent key was rewritten.
	bm, err := store.GetBranchMeta("main/c123/b1500")
	if err != nil || bm == nil {
		t.Fatalf("branch meta after migration: %v (%v)", bm, err)
	}
	if bm.ParentKey != "main/c123" {
		t.Errorf("branch parent = %q, want main/c123", bm.ParentKey)
	}
	if bm.BranchPoint != 2 {
		t.Errorf("branch point = %d, want 2 (preserved)", bm.BranchPoint)
	}

	// Version dirs are gone.
	for _, vd := range []string{v1, v2} {
		if _, err := os.Stat(vd); !os.IsNotExist(err) {
			t.Errorf("version dir %s still exists", vd)
		}
	}

	// Second run is a no-op.
	if n, err := store.MigrateLegacyLayout(); err != nil || n != 0 {
		t.Errorf("second run = (%d, %v), want (0, nil)", n, err)
	}
}

// TestMigrateLegacyLayout_BranchLoadsParentPrefix proves that after
// migration, a branch whose parent content was superseded still assembles its
// full history: the live parent is shorter than the branch point, so the
// prefix is recovered from the migration-stamped archive (P2-5 through the
// migrated layout).
func TestMigrateLegacyLayout_BranchLoadsParentPrefix(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	v1 := filepath.Join(dir, "main", "c9", "1000")
	v2 := filepath.Join(dir, "main", "c9", "2000")
	writeJSONL(t, filepath.Join(v1, "root.jsonl"), "p1", "p2", "p3")
	writeJSONL(t, filepath.Join(v2, "root.jsonl"), "compacted")

	branchPath := filepath.Join(v1, "b1500.jsonl")
	meta := BranchMeta{Type: "branch_meta", ParentKey: "main/c9/1000", BranchPoint: 3}
	metaLine, _ := json.Marshal(meta)
	branchMsg := provider.Message{Role: "user", Content: provider.TextContent("branch-own")}
	msgLine, _ := json.Marshal(branchMsg)
	content := string(metaLine) + "\n" + string(msgLine) + "\n"
	if err := os.WriteFile(branchPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.MigrateLegacyLayout(); err != nil {
		t.Fatalf("MigrateLegacyLayout: %v", err)
	}

	full, err := store.LoadFull("main/c9/b1500")
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	if len(full) != 4 {
		t.Fatalf("LoadFull = %d msgs, want 4 (3 archived parent prefix + 1 own)", len(full))
	}
	if provider.TextOf(full[0].Content) != "p1" || provider.TextOf(full[3].Content) != "branch-own" {
		t.Errorf("unexpected assembly: first=%q last=%q", provider.TextOf(full[0].Content), provider.TextOf(full[3].Content))
	}
}

// TestMigrateLegacyStateDB proves the state.db migration: session_metadata
// re-keys with newest-version-wins, chat_metadata session_key rows become
// registered ownership rows (app independent adoptions rewritten in place),
// facet and tmux agent_metadata values are re-keyed, legacy session_index
// rows are cleared, and the clean-shutdown marker is dropped so startup
// rebuilds from disk.
func TestMigrateLegacyStateDB(t *testing.T) {
	dbPath := t.TempDir() + "/state.db"
	idx, err := NewSessionIndex(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// Seed legacy rows directly.
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := idx.db.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// session_metadata: two versions of the same session — v2000 must win.
	exec(`INSERT INTO session_metadata (session_key, key, value) VALUES ('main/c1/1000', 'cc_resume_id', 'old-uuid')`)
	exec(`INSERT INTO session_metadata (session_key, key, value) VALUES ('main/c1/2000', 'cc_resume_id', 'new-uuid')`)
	exec(`INSERT INTO session_metadata (session_key, key, value) VALUES ('main/c1/2000', 'effort', 'high')`)
	// chat_metadata: telegram session_key row + app adoption of a named session.
	exec(`INSERT INTO chat_metadata (agent_id, platform, chat_id, key, value) VALUES ('main', 'telegram', 1, 'session_key', 'main/c1/2000')`)
	exec(`INSERT INTO chat_metadata (agent_id, platform, chat_id, key, value) VALUES ('main', 'app', 7, 'session_key', 'main/iwork/0')`)
	// agent_metadata: facet binding + tmux ownership map.
	exec(`INSERT INTO agent_metadata (agent_id, key, value) VALUES ('_system', 'facet:mybot', 'main/c1/2000/b1500')`)
	exec(`INSERT INTO agent_metadata (agent_id, key, value) VALUES ('main', 'tmux_owned', '{"work":"main/c1/2000"}')`)
	// session_index: a legacy row, plus the clean-shutdown marker.
	exec(`INSERT INTO session_index (session_key, file_path, created_at, session_type, status) VALUES ('main/c1/2000', 'x', '2026-01-01T00:00:00Z', 'chat', 'active')`)
	exec(`INSERT INTO system_state (key, value) VALUES ('last_clean_shutdown', '2026-01-01T00:00:00Z')`)
	_ = idx.Close()

	// Reopen — NewSessionIndex runs the migration.
	idx, err = NewSessionIndex(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	// session_metadata re-keyed, newest version's value winning.
	if v, _ := idx.GetSessionMetadata("main/c1", "cc_resume_id"); v != "new-uuid" {
		t.Errorf("cc_resume_id = %q, want new-uuid (newest version wins)", v)
	}
	if v, _ := idx.GetSessionMetadata("main/c1", "effort"); v != "high" {
		t.Errorf("effort = %q, want high", v)
	}
	if v, _ := idx.GetSessionMetadata("main/c1/2000", "effort"); v != "" {
		t.Errorf("legacy-keyed effort row survived: %q", v)
	}

	// telegram session_key row → registered ownership, key row dropped.
	if v, _ := idx.GetChatMetadata("main", "telegram", 1, "registered"); v != "true" {
		t.Errorf("telegram chat not registered after migration")
	}
	if v, _ := idx.GetChatMetadata("main", "telegram", 1, "session_key"); v != "" {
		t.Errorf("telegram session_key row survived: %q", v)
	}
	if got := idx.PlatformForChat("main", 1); got != "telegram" {
		t.Errorf("PlatformForChat = %q, want telegram", got)
	}

	// app adoption of an independent session is preserved, re-keyed.
	if v, _ := idx.GetChatMetadata("main", "app", 7, "session_key"); v != "main/iwork" {
		t.Errorf("app adoption = %q, want main/iwork", v)
	}

	// facet + tmux values re-keyed.
	if v, _ := idx.GetAgentMetadata("_system", "facet:mybot"); v != "main/c1/b1500" {
		t.Errorf("facet value = %q, want main/c1/b1500", v)
	}
	if v, _ := idx.GetAgentMetadata("main", "tmux_owned"); v != `{"work":"main/c1"}` {
		t.Errorf("tmux_owned = %q, want re-keyed map", v)
	}

	// Legacy index rows cleared and the clean-shutdown marker dropped.
	if n := idx.IndexCount(); n != 0 {
		t.Errorf("session_index rows = %d, want 0 (cleared for rebuild)", n)
	}
	if v, _ := idx.GetSystemState("last_clean_shutdown"); v != "" {
		t.Errorf("last_clean_shutdown survived: %q", v)
	}
}

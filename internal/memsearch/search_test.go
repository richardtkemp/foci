package memsearch

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/memory"
	"foci/internal/sqlite"
)

// fakeBackend returns fixed results and records the options it was given.
type fakeBackend struct {
	results []memory.Result
	limit   int
	opts    *memory.SearchOptions
}

func (f *fakeBackend) Search(_, _ string, opts *memory.SearchOptions) ([]memory.Result, error) {
	f.opts = opts
	n := f.limit
	if opts != nil && opts.Limit > 0 {
		n = opts.Limit
	}
	if n > 0 && len(f.results) > n {
		return f.results[:n], nil
	}
	return f.results, nil
}

func (f *fakeBackend) ReturnLimit() int { return f.limit }

func openTestIndex(t *testing.T) *Index {
	t.Helper()
	x, err := OpenIndex(filepath.Join(t.TempDir(), "own.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = x.Close() })
	return x
}

// seedConv writes a conversation.db for agent "clutch" with rows
// (session, direction, turn_kind, text) one minute apart from base and
// returns its path.
func seedConv(t *testing.T, base time.Time, rows [][4]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "conversation.db")
	db, err := sqlite.OpenInit(p, `CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, direction TEXT NOT NULL,
		user_id TEXT NOT NULL, username TEXT NOT NULL, chat_id INTEGER NOT NULL, text TEXT NOT NULL,
		parse_mode TEXT, session TEXT, error TEXT, content_type TEXT NOT NULL DEFAULT 'text',
		subagent_group TEXT, subagent_run INTEGER, turn_kind TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for i, r := range rows {
		var kind any
		if r[2] != "" {
			kind = r[2]
		}
		if _, err := db.Exec(`INSERT INTO messages (ts, direction, user_id, username, chat_id, text, session, turn_kind)
			VALUES (?, ?, '', '', 0, ?, ?, ?)`, base.Add(time.Duration(i)*time.Minute).Format(time.RFC3339Nano),
			r[1], r[3], r[0], kind); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func paths(hits []Hit) string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Path)
	}
	return strings.Join(out, ",")
}

func reasons(d []Dropped) map[string]string {
	out := map[string]string{}
	for _, x := range d {
		out[x.Path] = x.Reason
	}
	return out
}

// Own-family conversation rows inside the window are dropped (root and
// branch alike); another family's rows, and own rows older than the Nth-last
// compaction, are kept. Fail-arm: with N = 0 nothing is dropped.
func TestSearch_OwnConversationWindow(t *testing.T) {
	x := openTestIndex(t)
	x.NoteCompaction("clutch/c1", 2) // compaction 1
	x.NoteCompaction("clutch/c1", 2) // compaction 2: the window of 2 starts here
	w := x.Window("clutch/c1", 2)
	if w.MinGen != 1 || w.Since.IsZero() {
		t.Fatalf("window = %+v", w)
	}
	// Compaction 1's time is the window start: a row a minute before it is old.
	old := w.Since.Add(-time.Minute)
	recent := time.Now()
	be := &fakeBackend{limit: 20, results: []memory.Result{
		{Path: "clutch/c1", Source: ConversationSource, RowID: 1, Time: recent},
		{Path: "clutch/c1/b1795000000", Source: ConversationSource, RowID: 2, Time: recent},
		{Path: "clutch/c1", Source: ConversationSource, RowID: 3, Time: old},
		{Path: "clutch/c2", Source: ConversationSource, RowID: 4, Time: recent},
	}}
	n := 2
	s := &Searcher{Backends: map[string]memory.Searcher{"b": be}, Own: x, Window: func() int { return n }}
	out, err := s.Search(Request{Query: "q", SessionKey: "clutch/c1/b1795000001"})
	if err != nil {
		t.Fatal(err)
	}
	var kept []int64
	for _, h := range out.Hits {
		kept = append(kept, h.RowID)
	}
	if len(kept) != 2 || kept[0] != 3 || kept[1] != 4 {
		t.Fatalf("kept rows %v, want [3 4] (the old own row and the other family's)", kept)
	}
	if len(out.Dropped) != 2 || out.Dropped[0].Reason != DropOwnConversation || out.Dropped[1].Reason != DropOwnConversation {
		t.Fatalf("dropped = %+v", out.Dropped)
	}
	if be.opts.ExcludePath != "" {
		t.Fatalf("with an index the own session is not excluded in the query: %+v", be.opts)
	}
	if be.opts.Limit != 20*overfetch {
		t.Fatalf("no over-fetch: limit %d", be.opts.Limit)
	}
	// Fail-arm: no window, nothing dropped.
	n = 0
	out, _ = s.Search(Request{Query: "q", SessionKey: "clutch/c1"})
	if len(out.Hits) != 4 || len(out.Dropped) != 0 {
		t.Fatalf("N=0: kept %d dropped %d", len(out.Hits), len(out.Dropped))
	}
}

// With fewer compactions recorded than the window, every own row is own.
func TestSearch_OwnConversationBeforeNCompactions(t *testing.T) {
	x := openTestIndex(t)
	be := &fakeBackend{limit: 20, results: []memory.Result{
		{Path: "clutch/c1", Source: ConversationSource, RowID: 1, Time: time.Now().Add(-20 * 24 * time.Hour)},
	}}
	s := &Searcher{Backends: map[string]memory.Searcher{"b": be}, Own: x, Window: func() int { return 3 }}
	out, _ := s.Search(Request{Query: "q", SessionKey: "clutch/c1"})
	if len(out.Hits) != 0 || len(out.Dropped) != 1 {
		t.Fatalf("kept %d dropped %d", len(out.Hits), len(out.Dropped))
	}
}

// Without an index the asking session's rows are excluded whole in the query
// (the behaviour before #3185).
func TestSearch_NoIndexExcludesSessionInQuery(t *testing.T) {
	be := &fakeBackend{limit: 20}
	s := &Searcher{Backends: map[string]memory.Searcher{"b": be}}
	if _, err := s.Search(Request{Query: "q", SessionKey: "clutch/c1"}); err != nil {
		t.Fatal(err)
	}
	if be.opts.ExcludePath != "clutch/c1" || be.opts.Limit != 0 {
		t.Fatalf("opts = %+v", be.opts)
	}
}

// Non-delivered rows (a kind from the conversation DB, or a branch session)
// are dropped only when the request asks (JIT); the tool keeps them,
// labelled. Context lines carry the speaker.
func TestSearch_NonDeliveredAndContext(t *testing.T) {
	base := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	conv := memory.NewConversationReader(map[string]string{"clutch": seedConv(t, base, [][4]string{
		{"clutch/c2", "recv", "", "what is the anna mirror"},
		{"clutch/c2", "sent", "", "annas-archive.pk works"},
		{"clutch/c2", "sent", "reflection", "noted the mirror"},
	})})
	be := &fakeBackend{limit: 20, results: []memory.Result{
		{Path: "clutch/c2", Source: ConversationSource, RowID: 2, Time: base.Add(time.Minute)},
		{Path: "clutch/c2", Source: ConversationSource, RowID: 3, Time: base.Add(2 * time.Minute)},
		{Path: "clutch/c2/b1795000000", Source: ConversationSource, RowID: 9, Time: base},
	}}
	s := &Searcher{Backends: map[string]memory.Searcher{"b": be}, Conv: conv}

	// The tool: all kept, the reflection row labelled.
	out, _ := s.Search(Request{Query: "q", SessionKey: "clutch/c1"})
	if len(out.Hits) != 3 || out.Hits[1].Kind != "reflection" {
		t.Fatalf("tool hits = %+v", out.Hits)
	}
	// JIT: the reflection row and the branch row dropped.
	out, _ = s.Search(Request{Query: "q", SessionKey: "clutch/c1", DropNonDelivered: true, ContextLines: 4})
	if len(out.Hits) != 1 || out.Hits[0].RowID != 2 {
		t.Fatalf("jit hits = %+v", out.Hits)
	}
	if len(out.Dropped) != 2 || out.Dropped[0].Reason != DropNonDelivered || out.Dropped[1].Detail != "branch session" {
		t.Fatalf("dropped = %+v", out.Dropped)
	}
	ctx := out.Hits[0].Context
	if len(ctx) < 2 || ctx[0].Direction != "recv" || ctx[1].Direction != "sent" {
		t.Fatalf("context = %+v", ctx)
	}
}

// A memory-file section whose probe lines the family wrote through a tool
// (2 of 3) within the window is the family's own note; one matching line is
// not enough; another family, or the same lines after N compactions, keep it.
func TestSearch_OwnNote(t *testing.T) {
	dir := t.TempDir()
	l1 := "- The vault sync moved to Syncthing on the NUC, 19 Nov afternoon."
	l2 := "- Obsidian Sync stays as the phone path until the trial ends in December."
	l3 := "- Dick ruled: no third-party plugin for the sync, the built-in one only."
	note := "# 2026-11-19\n\n## Vault sync\n\n" + l1 + "\n" + l2 + "\n" + l3 + "\n\n## Other\n\nunrelated words\n"
	if err := os.WriteFile(filepath.Join(dir, "2026-11-19.md"), []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
	x := openTestIndex(t)
	be := &fakeBackend{limit: 20, results: []memory.Result{
		{Path: "2026-11-19.md", Source: "clutch", Snippet: "The vault >sync< moved to Syncthing"},
	}}
	n := 3
	s := &Searcher{Backends: map[string]memory.Searcher{"b": be}, Own: x, Window: func() int { return n },
		SourceDirs: func() map[string]string { return map[string]string{"clutch": dir} }}
	search := func(sk string) Outcome {
		t.Helper()
		out, err := s.Search(Request{Query: "vault sync", SessionKey: sk})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Fail-arm: one matching line of three is not own.
	x.Observe("clutch/c1/b1795000000", `{"command":"echo \"`+l1+`\" >> memory/2026-11-19.md"}`)
	x.Flush()
	if out := search("clutch/c1"); len(out.Hits) != 1 {
		t.Fatalf("1 of 3 lines dropped the note: %+v", out.Dropped)
	}
	// The reflection branch's heredoc writes the rest: own for the family.
	x.Observe("clutch/c1/b1795000000", `{"command":"cat >> memory/2026-11-19.md <<'EOF'\n`+l2+`\n`+l3+`\nEOF"}`)
	x.Flush()
	out := search("clutch/c1")
	if len(out.Hits) != 0 || reasons(out.Dropped)["2026-11-19.md"] != DropOwnNote {
		t.Fatalf("own note kept: hits %s dropped %+v", paths(out.Hits), out.Dropped)
	}
	if out.Dropped[0].Section == nil || !strings.Contains(out.Dropped[0].Detail, "3/3") {
		t.Fatalf("drop detail = %+v", out.Dropped[0])
	}
	// Another family still gets it.
	if out := search("clutch/c2"); len(out.Hits) != 1 {
		t.Fatalf("another family lost the note: %+v", out.Dropped)
	}
	// After N compactions the lines are outside the window (and pruned).
	for i := 0; i < n; i++ {
		x.NoteCompaction("clutch/c1", n)
	}
	if out := search("clutch/c1"); len(out.Hits) != 1 {
		t.Fatalf("note still own after %d compactions: %+v", n, out.Dropped)
	}
	var left int
	if err := x.db.QueryRow(`SELECT COUNT(*) FROM lines WHERE family = 'clutch/c1'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d lines left after pruning", left)
	}
}

// The window survives a restart: compaction counts and lines are in the
// database.
func TestIndex_Persists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "own.db")
	x, err := OpenIndex(p)
	if err != nil {
		t.Fatal(err)
	}
	x.NoteCompaction("clutch/c1/b17", 3)
	x.Observe("clutch/c1", strings.Repeat("persisted line ", 4))
	if err := x.Close(); err != nil {
		t.Fatal(err)
	}
	x.Observe("clutch/c1", strings.Repeat("after close ", 5)) // dropped, no panic
	y, err := OpenIndex(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = y.Close() }()
	w := y.Window("clutch/c1", 3)
	if w.Gen != 1 || w.Family != "clutch/c1" {
		t.Fatalf("window after restart = %+v", w)
	}
	n, err := y.Matches(Window{Family: "clutch/c1", N: 3, MinGen: 1}, []int64{HashLine(strings.Repeat("persisted line ", 4))})
	if err != nil || n != 1 {
		t.Fatalf("matches = %d, %v", n, err)
	}
	var cnt int
	if err := y.db.QueryRow(`SELECT COUNT(*) FROM lines`).Scan(&cnt); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Fatalf("%d lines stored, want 1", cnt)
	}
}

// Kept hits are cut back to the backend's normal limit after the drops.
func TestSearch_OverfetchCutsToLimit(t *testing.T) {
	x := openTestIndex(t)
	var rs []memory.Result
	for i := 0; i < 10; i++ {
		rs = append(rs, memory.Result{Path: "clutch/c1", Source: ConversationSource, RowID: int64(i + 1), Time: time.Now()})
	}
	for i := 0; i < 10; i++ {
		rs = append(rs, memory.Result{Path: "clutch/c2", Source: ConversationSource, RowID: int64(i + 100), Time: time.Now()})
	}
	be := &fakeBackend{limit: 4, results: rs}
	s := &Searcher{Backends: map[string]memory.Searcher{"b": be}, Own: x, Window: func() int { return 3 }}
	out, _ := s.Search(Request{Query: "q", SessionKey: "clutch/c1"})
	// limit 4 x3 = 12 fetched: 10 own dropped, 2 kept.
	if len(out.Hits) != 2 || len(out.Dropped) != 10 {
		t.Fatalf("kept %d dropped %d", len(out.Hits), len(out.Dropped))
	}
	be.limit = 2
	out, _ = s.Search(Request{Query: "q", SessionKey: "clutch/c9"})
	if len(out.Hits) != 2 {
		t.Fatalf("kept %d, want the limit 2", len(out.Hits))
	}
}

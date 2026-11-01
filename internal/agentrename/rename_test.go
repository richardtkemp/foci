package agentrename

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"foci/internal/config"
	"foci/internal/session"
)

// oldIDCheck is one column the rename must leave with no reference to the
// old id. It is the test's own list, written from the spec's inventory, not
// read from the production rules — so a store the rules forget fails here.
type oldIDCheck struct {
	db, table, col string
	where          string // SQL true when the cell still names bob
}

func oldIDChecks(f *fixture, ws string) []oldIDCheck {
	eq := func(db, t, c string) oldIDCheck { return oldIDCheck{db, t, c, c + " = 'bob'"} }
	pre := func(db, t, c string) oldIDCheck { return oldIDCheck{db, t, c, "substr(" + c + ",1,4) = 'bob/'"} }
	state := filepath.Join(f.data, "state.db")
	cs := []oldIDCheck{
		pre(state, "session_index", "session_key"), pre(state, "session_index", "parent_session_key"),
		eq(state, "session_index", "agent_id"),
		{state, "session_index", "file_path", "instr(file_path, '/sessions/bob/') > 0"},
		pre(state, "session_metadata", "session_key"), pre(state, "backend_resume_history", "session_key"),
		pre(state, "session_archives", "session_key"),
		{state, "session_archives", "file_path", "instr(file_path, '/sessions/bob/') > 0"},
		eq(state, "agent_metadata", "agent_id"),
		{state, "agent_metadata", "key", "instr(key, ':bob/') > 0"},
		{state, "agent_metadata", "value", `instr(value, '"bob/') > 0 OR value = 'bob/c1'`},
		eq(state, "chat_metadata", "agent_id"),
		{state, "chat_metadata", "value", "key = 'session_key' AND substr(value,1,4) = 'bob/'"},
	}
	for _, name := range []string{"api.db", "api-shadow.db"} {
		p := filepath.Join(f.data, name)
		cs = append(cs,
			pre(p, "turns", "turn_id"), pre(p, "turns", "session"), eq(p, "turns", "agent_id"),
			pre(p, "api_calls", "session"), pre(p, "api_calls", "turn_id"), pre(p, "api_calls", "call_key"),
			eq(p, "api_calls", "agent_id"), oldIDCheck{p, "api_calls", "detail", `instr(detail, '"bob/') > 0`},
			pre(p, "backend_reports", "session"), pre(p, "backend_reports", "scope_key"))
	}
	frames, defer_ := filepath.Join(f.data, "app-frames.db"), filepath.Join(f.data, "deferred-sends.db")
	cs = append(cs, eq(frames, "app_frames", "agent_id"), eq(frames, "app_prompts", "agent_id"),
		eq(defer_, "deferred_sends", "agent_id"), pre(defer_, "deferred_sends", "session_key"))
	d := func(n string) string { return filepath.Join(ws, ".data", n) }
	cs = append(cs, eq(d("todo.db"), "todos", "agent_id"), eq(d("reminders.db"), "reminders", "agent_id"),
		pre(d("reminders.db"), "reminders", "session_key"), eq(d("scratchpad.db"), "scratchpad", "agent_id"),
		eq(d("tasklist.db"), "tasks", "agent_id"), pre(d("conversation.db"), "messages", "session"),
		oldIDCheck{d("memory.db"), "memory_meta", "source", "source = 'agent/bob'"},
		oldIDCheck{d("memory.db"), "memory_fts", "source", "source = 'agent/bob'"})
	return cs
}

type dbState struct {
	counts map[string]int
	bobby  []string
}

func snapshotDBs(t *testing.T, f *fixture, ws string) map[string]dbState {
	out := map[string]dbState{}
	seen := map[string]bool{}
	for _, c := range oldIDChecks(f, ws) {
		if seen[c.db] {
			continue
		}
		seen[c.db] = true
		counts, bobby := tableDump(t, c.db)
		out[filepath.Base(filepath.Dir(c.db))+"/"+filepath.Base(c.db)] = dbState{counts, bobby}
	}
	return out
}

// assertRenamed checks everything requirement 5 says must be true after a
// rename of bob → rob, workspace kept at ws (where its databases now are).
func assertRenamed(t *testing.T, f *fixture, ws string, before map[string]dbState) {
	t.Helper()
	for _, c := range oldIDChecks(f, ws) {
		if n := queryInt(t, c.db, "SELECT count(*) FROM "+c.table+" WHERE "+c.where); n != 0 {
			t.Errorf("%s %s.%s: %d row(s) still name bob", filepath.Base(c.db), c.table, c.col, n)
		}
	}
	// Row counts unchanged; the unrelated agent bobby's rows byte-identical.
	after := snapshotDBs(t, f, ws)
	for k, b := range before {
		a := after[k]
		for tb, n := range b.counts {
			if a.counts[tb] != n {
				t.Errorf("%s %s: %d rows, want %d", k, tb, a.counts[tb], n)
			}
		}
		if strings.Join(a.bobby, "\n") != strings.Join(b.bobby, "\n") {
			t.Errorf("%s: bobby's rows changed:\nbefore %q\nafter  %q", k, b.bobby, a.bobby)
		}
	}
	state := filepath.Join(f.data, "state.db")
	// JSON values: keys rewritten, prose and other agents' keys not.
	if got := queryString(t, state, `SELECT value FROM agent_metadata WHERE agent_id='rob' AND key='tmux_owned'`); got != `{"w1":"rob/c1","w2":"bobby/c1"}` {
		t.Errorf("tmux_owned = %s", got)
	}
	if got := queryString(t, state, `SELECT value FROM agent_metadata WHERE agent_id='rob' AND key='ask_pending'`); got != `[{"request_id":"ask-bob-1","session_key":"rob/c1","text":"see bob/c1 and bobby"}]` {
		t.Errorf("ask_pending = %s", got)
	}
	if got := queryString(t, state, `SELECT value FROM agent_metadata WHERE agent_id='rob' AND key='depth:rob/c1/b100'`); got != `{"key":"rob/c1/b100","level":1,"parent":"rob/c1","prompt":"mentions bob/c1 in prose"}` {
		t.Errorf("depth node = %s", got)
	}
	if got := queryString(t, state, `SELECT value FROM agent_metadata WHERE agent_id='_system' AND key='facet:tgbot'`); got != "rob/c1" {
		t.Errorf("facet binding = %s", got)
	}
	if got := queryString(t, state, `SELECT value FROM chat_metadata WHERE agent_id='rob' AND key='alias'`); got != "talks about bob/c1" {
		t.Errorf("alias prose changed: %s", got)
	}
	if got := queryString(t, state, `SELECT value FROM chat_metadata WHERE agent_id='rob' AND chat_id=2`); got != "/c2/b1" {
		t.Errorf("empty-agent session_key row changed: %s", got)
	}
	// Integrity: foreign keys and unique indexes hold.
	for _, name := range []string{"api.db", "api-shadow.db"} {
		p := filepath.Join(f.data, name)
		if n := queryInt(t, p, `SELECT count(*) FROM pragma_foreign_key_check`); n != 0 {
			t.Errorf("%s: %d foreign key violation(s)", name, n)
		}
		if got := queryString(t, p, `PRAGMA integrity_check`); got != "ok" {
			t.Errorf("%s: integrity_check = %s", name, got)
		}
		if got := queryString(t, p, `SELECT call_key FROM api_calls WHERE agent_id='rob'`); got != "rob/c1@1:q1:m" {
			t.Errorf("%s call_key = %s", name, got)
		}
	}
	// Sessions tree: under rob/, every indexed path exists, gzip stays gzip.
	if _, err := os.Stat(filepath.Join(f.sessions, "bob")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sessions/bob still exists: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+state+"?mode=ro")
	must(t, err)
	rows, err := db.Query(`SELECT file_path FROM session_index WHERE agent_id='rob' AND instr(file_path, '/sessions/') > 0`)
	must(t, err)
	n := 0
	for rows.Next() {
		var p string
		must(t, rows.Scan(&p))
		n++
		if _, err := os.Stat(p); err != nil {
			t.Errorf("indexed file missing: %v", err)
		}
	}
	_ = rows.Close()
	_ = db.Close()
	if n != 3 {
		t.Errorf("%d indexed session files for rob, want 3", n)
	}
	gzPath := filepath.Join(f.sessions, "rob/c1/b100.jsonl.gz")
	if p, ok, err := session.ReadBranchParent(gzPath); err != nil || !ok || p != "rob/c1" {
		t.Errorf("gz branch parent = %q %v %v", p, ok, err)
	}
	raw, _ := os.ReadFile(gzPath)
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("b100 is no longer gzip: %v", err)
	}
	body, _ := io.ReadAll(zr)
	if !strings.Contains(string(body), `"orientation":"you are bob/c1/b100"`) || !strings.HasSuffix(string(body), `{"role":"user","content":"x"}`+"\n") {
		t.Errorf("b100 body changed beyond parent_key: %s", body)
	}
	if p, _, _ := session.ReadBranchParent(filepath.Join(f.sessions, "bobby/c1/b5.jsonl")); p != "bobby/c1" {
		t.Errorf("bobby's branch parent changed: %s", p)
	}
	// Rebuild-from-disk agrees: every scanned rob file has a rob key and a rob parent.
	entries, err := session.NewStore(f.sessions).ScanAllSessions()
	must(t, err)
	robs := 0
	for _, e := range entries {
		if strings.HasPrefix(e.SessionKey, "bob/") || strings.HasPrefix(e.ParentSessionKey, "bob/") {
			t.Errorf("rebuild from disk derives %s (parent %s)", e.SessionKey, e.ParentSessionKey)
		}
		if strings.HasPrefix(e.SessionKey, "rob/") {
			robs++
		}
	}
	if robs < 3 {
		t.Errorf("rebuild from disk found %d rob sessions, want >= 3", robs)
	}
	// foci.toml: loads; ids, pins, comments.
	cfg, err := config.Load(f.cfgPath, nil)
	must(t, err)
	if cfg.MasterAgent != "rob" || cfg.Askgw.DefaultAgent != "rob" {
		t.Errorf("master_agent %q, askgw default_agent %q", cfg.MasterAgent, cfg.Askgw.DefaultAgent)
	}
	rob := findAgent(cfg, "rob")
	if rob == nil || findAgent(cfg, "bob") != nil || findAgent(cfg, "bobby") == nil {
		t.Fatalf("agents after rename: %+v", cfg.Agents)
	}
	if tg := rob.Platform("telegram"); tg == nil || tg.Bot != "bob" {
		t.Errorf("rob's telegram bot = %+v, want bob (same bot, same token)", tg)
	}
	if rob.Workspace != ws {
		t.Errorf("rob's workspace = %s, want %s", rob.Workspace, ws)
	}
	data, _ := os.ReadFile(f.cfgPath)
	if !strings.Contains(string(data), "# bob's own comment, which must survive") {
		t.Error("a foci.toml comment was lost")
	}
}

// TestRenameRewritesEveryStore runs a full rename on the fixture and checks
// requirement 5 store by store: no old id left in any inventory column, row
// counts unchanged, bobby untouched, JSON values rewritten in shape, the
// sessions tree consistent for both a trusted index and a rebuild from disk,
// foci.toml/secrets.toml/crontab rewritten exactly, the search index moved.
func TestRenameRewritesEveryStore(t *testing.T) {
	f := newFixture(t)
	ws := filepath.Join(f.root, "bob")
	before := snapshotDBs(t, f, ws)
	out, err := f.run(Options{})
	if err != nil {
		t.Fatalf("rename: %v\n%s", err, out)
	}
	assertRenamed(t, f, ws, before)

	sec, _ := os.ReadFile(f.secretsPath)
	want := strings.NewReplacer(`allowed_agents = ["bob", "bobby"]`, `allowed_agents = ["rob", "bobby"]`, "[agents.bob.telegram]", "[agents.rob.telegram]").Replace(fixtureSecrets)
	if string(sec) != want {
		t.Errorf("secrets.toml:\n%s\nwant:\n%s", sec, want)
	}
	if !strings.Contains(f.cron.content, "foci send -a rob hello") || !strings.Contains(f.cron.content, "-a bobby hello") ||
		!strings.Contains(f.cron.content, "--oneshot -a rob -mf") || !strings.Contains(f.cron.content, "-a bob-2 hello") {
		t.Errorf("crontab:\n%s", f.cron.content)
	}
	if strings.Contains(f.cron.content, f.root+"/rob/") || !strings.Contains(f.cron.content, "# 20 * * * * foci send -a rob -mf bob/prompts/y.md") {
		t.Errorf("keep mode: paused job not renamed, or a workspace path rewritten:\n%s", f.cron.content)
	}
	if _, err := os.Stat(filepath.Join(ws, ".data", "search.bleve")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("search.bleve not moved aside: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.data, journalName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("journal left behind after a full success: %v", err)
	}
	// Keep mode moved nothing on disk.
	if _, err := os.Stat(filepath.Join(ws, "character", "SOUL.md")); err != nil {
		t.Errorf("workspace moved in keep mode: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "rob")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("~/rob created in keep mode: %v", err)
	}
	if !strings.Contains(out, "Checklist:") || !strings.Contains(out, "Start the gateway again") {
		t.Errorf("no final checklist:\n%s", out)
	}
}

// TestRenameValidationRefusesBeforeAnyChange covers requirement 2 and the
// offline gate of requirement 1: each bad input aborts with nothing changed.
func TestRenameValidationRefusesBeforeAnyChange(t *testing.T) {
	cases := []struct {
		name, old, new, want string
		setup                func(t *testing.T, f *fixture) func()
	}{
		{name: "bad format", old: "bob", new: "Rob", want: "lowercase slug"},
		{name: "slash", old: "bob", new: "ro/b", want: "lowercase slug"},
		{name: "reserved", old: "bob", new: "data", want: "reserved"},
		{name: "existing agent", old: "bob", new: "bobby", want: "already exists"},
		{name: "missing old", old: "nobody", new: "rob", want: "no agent"},
		{name: "new workspace dir exists", old: "bob", new: "rob", want: "already exists", setup: func(t *testing.T, f *fixture) func() {
			must(t, os.MkdirAll(filepath.Join(f.root, "rob"), 0o755))
			return func() { _ = os.Remove(filepath.Join(f.root, "rob")) }
		}},
		{name: "new id in a store", old: "bob", new: "rob", want: "already used", setup: func(t *testing.T, f *fixture) func() {
			p := filepath.Join(f.data, "app-frames.db")
			execAll(t, p, `INSERT INTO app_frames (conv_id, seq, wire, sent_ms, agent_id) VALUES ('conv-r',1,'{}',1,'rob')`)
			return func() { execAll(t, p, `DELETE FROM app_frames WHERE agent_id='rob'`) }
		}},
		{name: "gateway socket answers", old: "bob", new: "rob", want: "gateway is running", setup: func(t *testing.T, f *fixture) func() {
			sock := filepath.Join(f.data, "foci-gw.sock")
			if len(sock) >= 100 {
				t.Skipf("socket path too long for this OS: %s", sock)
			}
			l, err := net.Listen("unix", sock)
			must(t, err)
			return func() { _ = l.Close() }
		}},
		{name: "database held open", old: "bob", new: "rob", want: "open in another process", setup: func(t *testing.T, f *fixture) func() {
			// An idle WAL connection, as the gateway keeps.
			db, err := sql.Open("sqlite", "file:"+filepath.Join(f.data, "state.db")+"?_pragma=journal_mode(WAL)")
			must(t, err)
			var n int
			must(t, db.QueryRow(`SELECT count(*) FROM session_index`).Scan(&n))
			return func() { _ = db.Close() }
		}},
	}
	f := newFixtureAt(t, shortRoot(t))
	skip := []string{"data/foci-gw.sock"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var undo func()
			if c.setup != nil {
				undo = c.setup(t, f)
			}
			before := hashTree(t, f.root, skip...)
			out, err := f.run(Options{Old: c.old, New: c.new})
			after := hashTree(t, f.root, skip...)
			if undo != nil {
				undo()
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q\n%s", err, c.want, out)
			}
			diffHashes(t, before, after)
		})
	}
	// Fail-arm of the held-open case: with the holder closed the same run is
	// refused for no reason, i.e. the lock probe itself is what refused.
	if out, err := f.run(Options{PlanOnly: true}); err != nil {
		t.Fatalf("plan with nothing holding the files: %v\n%s", err, out)
	}
}

// TestRenamePlanOnlyChangesNothing: requirement 3. A plan-only run lists the
// steps with counts and sudo marks, exits 0, and changes no byte anywhere.
func TestRenamePlanOnlyChangesNothing(t *testing.T) {
	f := newFixture(t)
	before := hashTree(t, f.root)
	out, err := f.run(Options{PlanOnly: true})
	if err != nil {
		t.Fatalf("plan: %v\n%s", err, out)
	}
	diffHashes(t, before, hashTree(t, f.root))
	for _, want := range []string{"[sudo] secrets", "[sudo] crontab of foci", "session_index.session_key", "row(s)", "Plan only: nothing was changed.", "+[agents.rob.telegram]"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SECRET-VALUE") || strings.Contains(out, "TOKEN-BOB") {
		t.Errorf("the plan printed a secret value:\n%s", out)
	}
	if len(f.priv.calls) != 0 {
		t.Errorf("plan ran sudo: %v", f.priv.calls)
	}
}

// TestRenameNeedsConfirmation: without --yes, a non-interactive run refuses;
// an interactive "n" cancels; both change nothing.
func TestRenameNeedsConfirmation(t *testing.T) {
	f := newFixture(t)
	before := hashTree(t, f.root)
	var out bytes.Buffer
	err := Run(context.Background(), Options{ConfigPath: f.cfgPath, Old: "bob", New: "rob", Out: &out, Priv: f.priv, Crontab: f.cron})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-interactive without --yes: %v", err)
	}
	err = Run(context.Background(), Options{ConfigPath: f.cfgPath, Old: "bob", New: "rob", Out: &out, Priv: f.priv, Crontab: f.cron,
		Interactive: true, In: strings.NewReader("n\n")})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("interactive no: %v", err)
	}
	diffHashes(t, before, hashTree(t, f.root))
}

// TestRenameBackupRestores: requirement 4. The backup holds every changed
// file and restoring it reproduces the pre-rename tree byte for byte (the
// privileged files excepted: restore prints their commands instead).
func TestRenameBackupRestores(t *testing.T) {
	f := newFixture(t)
	skip := []string{"data/backups", "config/secrets.toml"}
	before := hashTree(t, f.root, skip...)
	out, err := f.run(Options{})
	if err != nil {
		t.Fatalf("rename: %v\n%s", err, out)
	}
	backup := filepath.Join(f.data, "backups", "rename-bob-rob-20261101T120000Z")
	if !strings.Contains(out, backup) {
		t.Fatalf("backup path not printed:\n%s", out)
	}
	for _, rel := range []string{manifestName, "config/foci.toml", "sessions/c1/b100.jsonl.gz", "crontab.before"} {
		if _, err := os.Stat(filepath.Join(backup, rel)); err != nil {
			t.Errorf("backup lacks %s: %v", rel, err)
		}
	}
	var rout bytes.Buffer
	must(t, Restore(context.Background(), RestoreOptions{ConfigPath: f.cfgPath, BackupDir: backup, Yes: true, Out: &rout}))
	diffHashes(t, before, hashTree(t, f.root, skip...))
	if !strings.Contains(rout.String(), "crontab.before") {
		t.Errorf("restore does not say how to put the crontab back:\n%s", rout.String())
	}
}

// TestRenameResumesAfterSessionsFailure: requirement 6. A failure part-way
// through the sessions-tree step stops the run with a resume hint; the same
// command then finishes, and the result equals a clean rename (no step ran twice).
func TestRenameResumesAfterSessionsFailure(t *testing.T) {
	f := newFixture(t)
	ws := filepath.Join(f.root, "bob")
	before := snapshotDBs(t, f, ws)
	out, err := f.run(Options{failSessionsAfter: 1})
	if err == nil {
		t.Fatalf("simulated failure did not fail:\n%s", out)
	}
	for _, want := range []string{"FAILED", "re-run the same command", "--restore"} {
		if !strings.Contains(out, want) {
			t.Errorf("failure output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(f.data, journalName)); err != nil {
		t.Fatalf("no journal after the failure: %v", err)
	}
	// The databases are untouched: the run stopped before them.
	if n := queryInt(t, filepath.Join(f.data, "state.db"), `SELECT count(*) FROM session_index WHERE agent_id='bob'`); n != 4 {
		t.Errorf("state.db changed before its step: %d bob rows", n)
	}
	out, err = f.run(Options{})
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Resuming") {
		t.Errorf("second run did not resume:\n%s", out)
	}
	assertRenamed(t, f, ws, before)
}

// TestRenameDatabaseFailureIsAllOrNothing: a failure inside one database's
// step leaves that database byte-identical and stops the command.
func TestRenameDatabaseFailureIsAllOrNothing(t *testing.T) {
	f := newFixture(t)
	api := filepath.Join(f.data, "api.db")
	before := hashTree(t, f.data, "backups", "sessions", "state.db")
	out, err := f.run(Options{failDB: "db:api"})
	if err == nil {
		t.Fatalf("simulated failure did not fail:\n%s", out)
	}
	after := hashTree(t, f.data, "backups", "sessions", "state.db")
	if before["api.db"] != after["api.db"] {
		t.Error("api.db changed although its step failed")
	}
	if n := queryInt(t, api, `SELECT count(*) FROM turns WHERE agent_id='bob'`); n != 1 {
		t.Errorf("api.db turns for bob = %d", n)
	}
	// The earlier state.db step stays done.
	if n := queryInt(t, filepath.Join(f.data, "state.db"), `SELECT count(*) FROM session_index WHERE agent_id='rob'`); n != 4 {
		t.Errorf("state.db not renamed by its earlier step: %d", n)
	}
	// The command stopped: later steps did not run.
	if n := queryInt(t, filepath.Join(f.data, "app-frames.db"), `SELECT count(*) FROM app_frames WHERE agent_id='bob'`); n != 1 {
		t.Errorf("a step after the failure ran: %d", n)
	}
}

// TestRenamePrivilegedEditsMatchTheShownDiff: requirement 5g, success arm.
// The secrets.toml and crontab changes go through the (fake) sudo, and what
// lands equals the diff the plan showed.
func TestRenamePrivilegedEditsMatchTheShownDiff(t *testing.T) {
	f := newFixture(t)
	secBefore, _ := os.ReadFile(f.secretsPath)
	cronBefore := f.cron.content
	out, err := f.run(Options{})
	if err != nil {
		t.Fatalf("rename: %v\n%s", err, out)
	}
	secAfter, _ := os.ReadFile(f.secretsPath)
	for _, d := range []string{unifiedDiff("secrets.toml", string(secBefore), string(secAfter), 0), unifiedDiff("crontab", cronBefore, f.cron.content, 1)} {
		if d == "" {
			t.Fatal("a privileged file did not change")
		}
		for _, l := range indent(d) {
			if !strings.Contains(out, "       "+l+"\n") {
				t.Errorf("applied change line %q was not in the shown plan:\n%s", l, out)
			}
		}
	}
	var sawTee, sawCron bool
	for _, c := range f.priv.calls {
		sawTee = sawTee || (c[0] == "tee" && c[1] == f.secretsPath)
		sawCron = sawCron || (c[0] == "crontab" && c[2] == "foci")
	}
	if !sawTee || !sawCron {
		t.Errorf("privileged edits did not go through sudo: %v", f.priv.calls)
	}
	if info, _ := os.Stat(f.secretsPath); info.Mode().Perm() != 0o660 {
		t.Errorf("secrets.toml mode changed to %v", info.Mode().Perm())
	}
}

// TestRenamePrivilegedDeclinedIsPending: requirement 5g, declined arm. The
// rest of the rename completes; the two steps are PENDING with exact manual
// commands, the files are untouched, and the run exits non-zero.
func TestRenamePrivilegedDeclinedIsPending(t *testing.T) {
	f := newFixture(t)
	f.priv.decline = true
	secBefore, _ := os.ReadFile(f.secretsPath)
	cronBefore := f.cron.content
	out, err := f.run(Options{})
	if !errors.Is(err, ErrPending) {
		t.Fatalf("err = %v, want ErrPending\n%s", err, out)
	}
	for _, want := range []string{"PENDING  secrets", "PENDING  crontab", "sudo sed -i -E", "sudo crontab -u foci " + filepath.Join(f.data, "backups", "rename-bob-rob-20261101T120000Z", "crontab.after")} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if sec, _ := os.ReadFile(f.secretsPath); string(sec) != string(secBefore) || f.cron.content != cronBefore {
		t.Error("a declined privileged edit changed a file")
	}
	if n := queryInt(t, filepath.Join(f.data, "state.db"), `SELECT count(*) FROM session_index WHERE agent_id='rob'`); n != 4 {
		t.Errorf("the non-privileged steps did not complete: %d", n)
	}
	// Re-run (approver now says yes): only the pending steps run.
	f.priv.decline = false
	out, err = f.run(Options{})
	if err != nil {
		t.Fatalf("retry: %v\n%s", err, out)
	}
	if sec, _ := os.ReadFile(f.secretsPath); !strings.Contains(string(sec), "[agents.rob.telegram]") || !strings.Contains(f.cron.content, "-a rob hello") {
		t.Error("the retry did not apply the pending edits")
	}
}

// TestRenameMoveWorkspace: requirement 5f, move arm. The workspace and the CC
// project folder move; cwd-keyed rows in opencode.db and codex's state DB,
// indexed transcript paths and foci.toml paths follow; bobby's do not.
func TestRenameMoveWorkspace(t *testing.T) {
	f := newFixture(t)
	oldWS, newWS := filepath.Join(f.root, "bob"), filepath.Join(f.root, "rob")
	oc := filepath.Join(f.root, "xdg", "opencode", "opencode.db")
	cx := filepath.Join(f.root, ".codex", "state_5.sqlite")
	must(t, os.MkdirAll(filepath.Dir(oc), 0o755))
	must(t, os.MkdirAll(filepath.Dir(cx), 0o755))
	execAll(t, oc, `CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT)`,
		`INSERT INTO session VALUES ('a','`+oldWS+`'), ('b','`+oldWS+`/sub'), ('c','`+f.root+`/bobby')`)
	execAll(t, cx, `CREATE TABLE threads (id TEXT PRIMARY KEY, cwd TEXT)`,
		`INSERT INTO threads VALUES ('a','`+oldWS+`'), ('c','`+f.root+`/bobby')`)
	before := snapshotDBs(t, f, oldWS)
	out, err := f.run(Options{Move: true})
	if err != nil {
		t.Fatalf("rename: %v\n%s", err, out)
	}
	if !strings.Contains(out, "WARNING: Claude Code, opencode and codex resume") {
		t.Errorf("no resume-id warning in the plan:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(newWS, "character", "SOUL.md")); err != nil {
		t.Errorf("workspace not moved: %v", err)
	}
	if _, err := os.Stat(oldWS); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old workspace still there: %v", err)
	}
	ccNew := filepath.Join(f.root, ".claude", "projects", ccSlug(newWS), "u1.jsonl")
	if _, err := os.Stat(ccNew); err != nil {
		t.Errorf("CC project folder not moved: %v", err)
	}
	assertRenamed(t, f, newWS, before)
	if got := queryInt(t, oc, `SELECT count(*) FROM session WHERE directory IN (?, ?)`, newWS, newWS+"/sub"); got != 2 {
		t.Errorf("opencode rows moved: %d of 2", got)
	}
	if got := queryString(t, oc, `SELECT directory FROM session WHERE id='c'`); got != f.root+"/bobby" {
		t.Errorf("bobby's opencode row changed: %s", got)
	}
	if got := queryString(t, cx, `SELECT cwd FROM threads WHERE id='a'`); got != newWS {
		t.Errorf("codex cwd = %s", got)
	}
	state := filepath.Join(f.data, "state.db")
	if got := queryString(t, state, `SELECT file_path FROM session_index WHERE session_key='rob/i9'`); got != ccNew {
		t.Errorf("transcript path = %s, want %s", got, ccNew)
	}
	if got := queryString(t, filepath.Join(f.data, "api.db"), `SELECT session_file FROM api_calls WHERE agent_id='rob'`); got != ccNew {
		t.Errorf("api_calls.session_file = %s", got)
	}
	cfg, err := config.Load(f.cfgPath, nil)
	must(t, err)
	if cfg.WelcomeFile != newWS+"/welcome.md" {
		t.Errorf("foci.toml path not moved: %s", cfg.WelcomeFile)
	}
	if !strings.Contains(f.cron.content, "# 20 * * * * foci send -a rob -mf rob/prompts/y.md") {
		t.Errorf("crontab relative path or paused job not moved:\n%s", f.cron.content)
	}
	if !strings.Contains(f.cron.content, newWS+"/prompts/x.md") {
		t.Errorf("crontab path not moved:\n%s", f.cron.content)
	}
}

// TestRenameKeepModeListsPaths: in keep mode the foci.toml workspace paths
// are listed in the plan and left alone, and the agent resolves to its old dir.
func TestRenameKeepModeListsPaths(t *testing.T) {
	f := newFixture(t)
	out, err := f.run(Options{})
	if err != nil {
		t.Fatalf("rename: %v\n%s", err, out)
	}
	if !strings.Contains(out, "listed only; rewritten with --move-workspace") || !strings.Contains(out, "welcome_file") {
		t.Errorf("plan does not list the foci.toml paths:\n%s", out)
	}
	cfg, err := config.Load(f.cfgPath, nil)
	must(t, err)
	if cfg.WelcomeFile != f.root+"/bob/welcome.md" {
		t.Errorf("keep mode rewrote a path: %s", cfg.WelcomeFile)
	}
}

// TestRewriteValue: requirement 5b's JSON rule on its own: a session key in a
// JSON value changes; prose, substrings and other agents' keys do not.
func TestRewriteValue(t *testing.T) {
	r := ids{"bob", "rob"}
	for in, want := range map[string]string{
		`bob/c1`:                           `rob/c1`,
		`bob/c1/b2`:                        `rob/c1/b2`,
		`bob/c1@123`:                       `rob/c1@123`,
		`bobby/c1`:                         `bobby/c1`,
		`bob`:                              `bob`,
		`see bob/c1`:                       `see bob/c1`,
		`bob/notakey`:                      `bob/notakey`,
		`{"w":"bob/c1","x":"bobby/c1"}`:    `{"w":"rob/c1","x":"bobby/c1"}`,
		`{"t":"bob/c1 in prose","n":1.50}`: `{"t":"bob/c1 in prose","n":1.50}`,
		`[{"session_key":"bob/i9","n":12345678901234567890}]`: `[{"n":12345678901234567890,"session_key":"rob/i9"}]`,
		`{"bob/c1":"x"}`:     `{"rob/c1":"x"}`,
		`not json {"bob/c1"`: `not json {"bob/c1"`,
	} {
		if got, _ := r.rewriteValue(in); got != want {
			t.Errorf("rewriteValue(%s) = %s, want %s", in, got, want)
		}
	}
}

// TestCronAndSecretsBoundaries: only exact references change — -a bob, not
// -a bobby or -a bob-2; [agents.bob.x], not [agents.bobby.x].
func TestCronAndSecretsBoundaries(t *testing.T) {
	r := ids{"bob", "rob"}
	for in, want := range map[string]string{
		"* * * * * foci send -a bob hi":               "* * * * * foci send -a rob hi",
		"* * * * * foci send -a bobby hi":             "* * * * * foci send -a bobby hi",
		"* * * * * foci send -a bob-2 hi":             "* * * * * foci send -a bob-2 hi",
		"* * * * * foci send --agent=bob hi":          "* * * * * foci send --agent=rob hi",
		"* * * * * foci send -a 'bob' hi":             "* * * * * foci send -a 'rob' hi",
		"* * * * * FOCI_AGENT=bob foci send x":        "* * * * * FOCI_AGENT=rob foci send x",
		"* * * * * foci send -a bob;foci send -a bob": "* * * * * foci send -a rob;foci send -a rob",
		"* * * * * echo -a bob":                       "* * * * * echo -a bob",
		// A paused job is renamed too, so re-enabling it works.
		"# PAUSED # 0 * * * * foci send -a bob x": "# PAUSED # 0 * * * * foci send -a rob x",
	} {
		if got, _ := rewriteCronLine(in, r, "", ""); got != want {
			t.Errorf("cron %q → %q, want %q", in, got, want)
		}
	}
	// A move also rewrites the workspace's absolute and cron-relative paths
	// (cron runs in the install root); keep mode leaves relative paths.
	in := "0 * * * * foci send -a bob -mf bob/prompts/x.md -x /h/bob/y /h/bobby/z bobby/w"
	wantMove := "0 * * * * foci send -a rob -mf rob/prompts/x.md -x /h/rob/y /h/bobby/z bobby/w"
	if got, _ := rewriteCronLine(in, r, "/h/bob", "/h/rob"); got != wantMove {
		t.Errorf("move: %q, want %q", got, wantMove)
	}
	if got, _ := rewriteCronLine("0 * * * * foci send -a bob -mf bob/p.md", r, "", ""); got != "0 * * * * foci send -a rob -mf bob/p.md" {
		t.Errorf("keep mode rewrote a relative path: %q", got)
	}
	sec := "[agents.bob]\n[agents.\"bob\".tg]\n[agents.bobby.tg]\nallowed_agents = [\n  \"bobby\",\n  \"bob\", # bob\n]\ndenied_agents = ['bob']\nx = \"bob\"\n"
	want := "[agents.rob]\n[agents.\"rob\".tg]\n[agents.bobby.tg]\nallowed_agents = [\n  \"bobby\",\n  \"rob\", # bob\n]\ndenied_agents = ['rob']\nx = \"bob\"\n"
	if got := rewriteSecrets(sec, "bob", "rob"); got != want {
		t.Errorf("rewriteSecrets:\n%s\nwant\n%s", got, want)
	}
}

// TestRenameAndRestoreKeepConfigInode (#479 × #1281): foci.toml is
// root:foci-secrets 0664 and the gateway refuses it in any other shape, so
// neither the rename's rewrite nor the restore may replace the file. Both must
// write in place: the inode before the rename, after it and after the
// restore is the same.
func TestRenameAndRestoreKeepConfigInode(t *testing.T) {
	f := newFixture(t)
	inode := func() uint64 {
		t.Helper()
		info, err := os.Stat(f.cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Skip("no inode on this platform")
		}
		return st.Ino
	}
	before := inode()
	out, err := f.run(Options{})
	if err != nil {
		t.Fatalf("rename: %v\n%s", err, out)
	}
	if got := inode(); got != before {
		t.Fatalf("rename replaced foci.toml (inode %d -> %d); it must edit in place", before, got)
	}
	backup := filepath.Join(f.data, "backups", "rename-bob-rob-20261101T120000Z")
	var rout bytes.Buffer
	must(t, Restore(context.Background(), RestoreOptions{ConfigPath: f.cfgPath, BackupDir: backup, Yes: true, Out: &rout}))
	if got := inode(); got != before {
		t.Fatalf("restore replaced foci.toml (inode %d -> %d); it must write in place", before, got)
	}
}

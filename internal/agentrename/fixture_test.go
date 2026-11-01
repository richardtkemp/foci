package agentrename

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"foci/internal/app"
	"foci/internal/convo"
	"foci/internal/defersend"
	"foci/internal/delegator/accounting"
	"foci/internal/fociroot"
	"foci/internal/memory"
	"foci/internal/session"
)

// fixtureConfig is the fixture's foci.toml: agent bob (renamed in the tests)
// and agent bobby, whose id contains "bob" and must never change.
const fixtureConfig = `# fixture config
master_agent = "bob"
welcome_file = "%[1]s/bob/welcome.md"

[logging]
api_shadow_db = "api-shadow.db"

[askgw]
default_agent = "bob"

# An app platform block with no [platforms.app] table (as on the live host).
[[platforms]]
id = "app"

[[agents]]
id = "bob"
backend = "claude-code"
# bob's own comment, which must survive
name = "Bob"

[[agents]]
id = "bobby"
backend = "claude-code"
`

const fixtureSecrets = `[telegram]
bob = "TOKEN-BOB"
bobby = "TOKEN-BOBBY"

[http]
api_key = "SECRET-VALUE"
allowed_agents = ["bob", "bobby"]

[agents.bob.telegram]
extra = "BOB-ONLY-SECRET"

[agents.bobby.telegram]
extra = "BOBBY-ONLY-SECRET"
`

const fixtureCrontab = `# m h dom mon dow command
0 * * * * foci send -a bob hello
5 * * * * foci send -a bobby hello
10 * * * * foci branch --oneshot -a bob -mf %[1]s/bob/prompts/x.md
15 * * * * foci send -a bob-2 hello
# PAUSED 2026-03-15 # 20 * * * * foci send -a bob -mf bob/prompts/y.md
`

// fixture is a whole foci install root on a temp dir, built with the
// production stores' own constructors (their real schemas).
type fixture struct {
	t                     *testing.T
	root, data, cfgPath   string
	secretsPath, sessions string
	cron                  *fakeCron
	priv                  *fakePriv
}

type fakeCron struct{ content string }

func (c *fakeCron) Read() (string, error) { return c.content, nil }

// fakePriv stands in for sudo: it records every command and runs the few the
// rename uses (cat, tee, crontab -u U -) without privileges; decline makes
// every call fail like a declined approval.
type fakePriv struct {
	decline bool
	cron    *fakeCron
	calls   [][]string
}

func (f *fakePriv) Run(argv []string, stdin []byte) ([]byte, error) {
	f.calls = append(f.calls, argv)
	if f.decline {
		return nil, errors.New("sudo: request declined by the approver")
	}
	switch {
	case argv[0] == "cat" && len(argv) == 2:
		return os.ReadFile(argv[1])
	case argv[0] == "tee" && len(argv) == 2:
		fh, err := os.OpenFile(argv[1], os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return nil, err
		}
		_, err = fh.Write(stdin)
		if cerr := fh.Close(); err == nil {
			err = cerr
		}
		return stdin, err
	case argv[0] == "crontab" && len(argv) == 4 && argv[1] == "-u" && argv[3] == "-":
		f.cron.content = string(stdin)
		return nil, nil
	}
	return nil, fmt.Errorf("fake sudo: unexpected command %v", argv)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func execAll(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	must(t, err)
	defer func() { _ = db.Close() }()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v\n%s", filepath.Base(path), err, s)
		}
	}
}

func gz(t *testing.T, s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, err := w.Write([]byte(s))
	must(t, err)
	must(t, w.Close())
	return b.Bytes()
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, data, 0o644))
}

// newFixture builds the install root. Every environment variable that points
// foci (or a backend) at a home is set into the temp root.
func newFixture(t *testing.T) *fixture {
	return newFixtureAt(t, t.TempDir())
}

// shortRoot is a temp root short enough for a Unix socket path inside it
// (macOS caps a socket path at 104 bytes; the test temp dirs are longer).
func shortRoot(t *testing.T) string {
	const sockRel = "/data/foci-gw.sock"
	for _, base := range []string{os.TempDir(), "/tmp"} {
		dir, err := os.MkdirTemp(base, "ar")
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		real, err := filepath.EvalSymlinks(dir)
		must(t, err)
		if len(real)+len(sockRel) < 100 {
			return real
		}
	}
	return t.TempDir() // the socket case skips itself if the path is too long
}

func newFixtureAt(t *testing.T, root string) *fixture {
	for k, v := range map[string]string{
		"FOCI_HOME": root, "HOME": root, "FOCI_CONFIG": "",
		"CLAUDE_CONFIG_DIR": filepath.Join(root, ".claude"),
		"XDG_DATA_HOME":     filepath.Join(root, "xdg"),
		"CODEX_HOME":        filepath.Join(root, ".codex"),
	} {
		t.Setenv(k, v)
	}
	f := &fixture{t: t, root: root, data: filepath.Join(root, "data"),
		cfgPath: filepath.Join(root, "config", "foci.toml"), secretsPath: filepath.Join(root, "config", "secrets.toml")}
	f.sessions = filepath.Join(f.data, "sessions")
	f.cron = &fakeCron{content: fmt.Sprintf(fixtureCrontab, root)}
	f.priv = &fakePriv{cron: f.cron}
	writeFile(t, f.cfgPath, []byte(fmt.Sprintf(fixtureConfig, root)))
	writeFile(t, f.secretsPath, []byte(fixtureSecrets))
	must(t, os.Chmod(f.secretsPath, 0o660))

	// Sessions tree.
	s := f.sessions
	writeFile(t, filepath.Join(s, "bob/c1/root.jsonl"), []byte(`{"role":"user","content":"hi bob/c1"}`+"\n"))
	writeFile(t, filepath.Join(s, "bob/c1/root.2026-01-01T00-00-00Z.jsonl"), []byte(`{"role":"user","content":"old"}`+"\n"))
	writeFile(t, filepath.Join(s, "bob/c1/b100.jsonl.gz"), gz(t, `{"type":"branch_meta","parent_key":"bob/c1","branch_point":1,"orientation":"you are bob/c1/b100"}`+"\n"+`{"role":"user","content":"x"}`+"\n"))
	writeFile(t, filepath.Join(s, "bob/c1/b200.jsonl"), []byte(`{"type":"branch_meta","parent_key":"bob/c1","branch_point":2}`+"\n"+`{"role":"user","content":"y"}`+"\n"))
	writeFile(t, filepath.Join(s, "bobby/c1/root.jsonl"), []byte(`{"role":"user","content":"bobby"}`+"\n"))
	writeFile(t, filepath.Join(s, "bobby/c1/b5.jsonl"), []byte(`{"type":"branch_meta","parent_key":"bobby/c1","branch_point":1}`+"\n"))
	sp := func(rel string) string { return filepath.Join(s, rel) }

	// state.db
	idx, err := session.NewSessionIndex(filepath.Join(f.data, "state.db"))
	must(t, err)
	must(t, idx.Close())
	ccBob := filepath.Join(root, ".claude", "projects", fociroot.CCProjectSlug(filepath.Join(root, "bob")), "u1.jsonl")
	execAll(t, filepath.Join(f.data, "state.db"),
		`INSERT INTO session_index (session_key, file_path, created_at, session_type, agent_id, chat_id, is_root, parent_session_key) VALUES
			('bob/c1', '`+sp("bob/c1/root.jsonl")+`', 't', 'chat', 'bob', 1, 1, NULL),
			('bob/c1/b100', '`+sp("bob/c1/b100.jsonl.gz")+`', 't', 'facet', 'bob', 1, 0, 'bob/c1'),
			('bob/c1/b200', '`+sp("bob/c1/b200.jsonl")+`', 't', 'facet', 'bob', 1, 0, 'bob/c1'),
			('bob/i9', '`+ccBob+`', 't', 'independent', 'bob', 0, 1, NULL),
			('bobby/c1', '`+sp("bobby/c1/root.jsonl")+`', 't', 'chat', 'bobby', 1, 1, NULL),
			('bobby/c1/b5', '`+sp("bobby/c1/b5.jsonl")+`', 't', 'facet', 'bobby', 1, 0, 'bobby/c1')`,
		`INSERT INTO session_metadata VALUES ('bob/c1','cc_resume_id','uuid-1'), ('bob/c1','model','opus'), ('bobby/c1','cc_resume_id','uuid-2')`,
		`INSERT INTO backend_resume_history (session_key, observed_at, resume_id) VALUES ('bob/c1','t','uuid-1'), ('bobby/c1','t','uuid-2')`,
		`INSERT INTO session_archives VALUES ('bob/c1','t','`+sp("bob/c1/root.2026-01-01T00-00-00Z.jsonl")+`','compact'), ('bobby/c1','t','x','compact')`,
		`INSERT INTO agent_metadata VALUES
			('bob','tmux_owned','{"w1":"bob/c1","w2":"bobby/c1"}'),
			('bob','ask_pending','[{"request_id":"ask-bob-1","session_key":"bob/c1","text":"see bob/c1 and bobby"}]'),
			('bob','depth:bob/c1/b100','{"key":"bob/c1/b100","parent":"bob/c1","prompt":"mentions bob/c1 in prose","level":1}'),
			('bob','reflection_last','2026-01-01T00:00:00Z'),
			('bob','tmux_state','map[cc-1:agent:bob:cron:background-1]'),
			('bobby','tmux_owned','{"w":"bobby/c1"}'),
			('_system','facet:tgbot','bob/c1'),
			('_system','facet:other','bobby/c1')`,
		`INSERT INTO chat_metadata VALUES
			('bob','telegram',1,'session_key','bob/c1'),
			('bob','telegram',1,'alias','talks about bob/c1'),
			('bob','app',2,'session_key','/c2/b1'),
			('bobby','telegram',1,'session_key','bobby/c1')`,
		`INSERT INTO system_state VALUES ('last_clean_shutdown','2026-01-01T00:00:00Z')`,
	)

	// api.db and api-shadow.db: the ledger's own schema, with its foreign key
	// and the unique call_key index.
	for _, name := range []string{"api.db", "api-shadow.db"} {
		l, _, err := accounting.Open(filepath.Join(f.data, name), accounting.Options{NoBackup: true})
		must(t, err)
		must(t, l.Close())
		execAll(t, filepath.Join(f.data, name),
			`INSERT INTO turns (turn_id, session, agent_id, backend, source, started_at) VALUES
				('bob/c1@1','bob/c1','bob','ccstream','user','t'),
				('bobby/c1@1','bobby/c1','bobby','ccstream','user','t')`,
			`INSERT INTO api_calls (call_key, billed_at, backend, model, session, agent_id, turn_id, kind, finality, class_method, detail, session_file) VALUES
				('bob/c1@1:q1:m','t','ccstream','m','bob/c1','bob','bob/c1@1','call','completed','observed','{"scope":"bob/c1@1","window":0}','`+ccBob+`'),
				('bobby/c1@1:q1:m','t','ccstream','m','bobby/c1','bobby','bobby/c1@1','call','completed','observed','{"scope":"bobby/c1@1","window":0}',NULL)`,
			`INSERT INTO backend_reports (at, backend, session, scope_key, model, grain) VALUES
				('t','ccstream','bob/c1','bob/c1@1','m','per_call'), ('t','ccstream','bobby/c1','bobby/c1','m','per_call')`,
		)
	}

	// app-frames.db
	must(t, app.InitReplayStore(filepath.Join(f.data, "app-frames.db")))
	execAll(t, filepath.Join(f.data, "app-frames.db"),
		`INSERT INTO app_frames (conv_id, seq, wire, sent_ms, agent_id) VALUES ('conv-b',1,'{"t":"ask-bob-1"}',1,'bob'), ('conv-y',1,'{}',1,'bobby')`,
		`INSERT INTO app_prompts VALUES ('ask-bob-X-q0','conv-b','bob',1), ('ask-bobby-Y-q0','conv-y','bobby',1)`,
	)

	// deferred-sends.db
	ds, err := defersend.NewStore(filepath.Join(f.data, "deferred-sends.db"))
	must(t, err)
	must(t, ds.Close())
	execAll(t, filepath.Join(f.data, "deferred-sends.db"),
		`INSERT INTO deferred_sends (agent_id, session_key, text, created_at, deadline_at) VALUES ('bob','bob/c1','x','t','t'), ('bobby','bobby/c1','y','t','t')`)

	// Workspaces.
	f.workspaceDBs(filepath.Join(root, "bob"), "bob")
	f.workspaceDBs(filepath.Join(root, "bobby"), "bobby")
	writeFile(t, filepath.Join(root, "bob", ".data", "search.bleve", "index_meta.json"), []byte(`{}`))
	writeFile(t, filepath.Join(root, "bob", "character", "SOUL.md"), []byte("I am bob"))
	writeFile(t, ccBob, []byte(`{"cwd":"x"}`+"\n"))
	return f
}

// workspaceDBs creates one workspace's .data databases with rows for id (and
// one row for the unrelated agent "bobby" in the shared-schema todo.db).
func (f *fixture) workspaceDBs(ws, id string) {
	t := f.t
	d := filepath.Join(ws, ".data")
	must(t, os.MkdirAll(d, 0o755))
	ts, err := memory.NewTodoStore(filepath.Join(d, "todo.db"))
	must(t, err)
	must(t, ts.Close())
	rs, err := memory.NewReminderStore(filepath.Join(d, "reminders.db"))
	must(t, err)
	must(t, rs.Close())
	sc, err := memory.NewScratchpad(filepath.Join(d, "scratchpad.db"))
	must(t, err)
	must(t, sc.Close())
	tl, err := memory.NewTaskListStore(filepath.Join(d, "tasklist.db"))
	must(t, err)
	must(t, tl.Close())
	closeLog, err := convo.OpenAgent(id, filepath.Join(d, "conversation.db"))
	must(t, err)
	closeLog()
	mi, err := memory.NewIndex(filepath.Join(d, "memory.db"), map[string]memory.SourceConfig{}, time.Second, 1)
	must(t, err)
	must(t, mi.Close())
	q := func(s string) string { return strings.ReplaceAll(s, "ID", id) }
	execAll(t, filepath.Join(d, "todo.db"),
		q(`INSERT INTO todos (id, text, agent_id, created_at) VALUES (1,'do ID/c1 things','ID','t')`),
		`INSERT INTO todos (id, text, agent_id, created_at) VALUES (7,'bobby''s','bobby','t')`)
	execAll(t, filepath.Join(d, "reminders.db"), q(`INSERT INTO reminders (agent_id, text, due_at, due_tag, created, session_key) VALUES ('ID','r','t','t','t','ID/c1')`))
	execAll(t, filepath.Join(d, "scratchpad.db"), q(`INSERT INTO scratchpad VALUES ('ID','k','c','t')`))
	execAll(t, filepath.Join(d, "tasklist.db"), q(`INSERT INTO tasks (id, agent_id, subject, created_at) VALUES (1,'ID','s','t')`))
	execAll(t, filepath.Join(d, "conversation.db"),
		q(`INSERT INTO messages (ts, direction, user_id, username, chat_id, text, session) VALUES ('t','in','u','u',1,'hi','ID/c1'), ('t','in','u','u',1,'legacy','agent:ID:chat:1')`))
	execAll(t, filepath.Join(d, "memory.db"),
		// The gateway names an agent memory source "agent/<name>"
		// (buildAgentMemorySources); "agent:<name>" is a legacy leftover.
		q(`INSERT INTO memory_meta VALUES ('agent/ID','a.md',1), ('agent:ID','old.md',1), ('conversation','c',1)`),
		q(`INSERT INTO memory_fts (content, path, source) VALUES ('text','a.md','agent/ID'), ('c','c','conversation')`))
}

// run renames bob → rob with the fixture's fakes.
func (f *fixture) run(o Options) (string, error) {
	var out bytes.Buffer
	o.ConfigPath = f.cfgPath
	if o.Old == "" {
		o.Old, o.New = "bob", "rob"
	}
	o.Out = &out
	o.Priv = f.priv
	o.Crontab = f.cron
	o.CrontabUser = "foci"
	if !o.PlanOnly && !o.Interactive {
		o.Yes = true
	}
	o.Now = func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }
	err := Run(context.Background(), o)
	return out.String(), err
}

// hashTree maps every regular file under root (relative path) to its sha256,
// skipping relative paths with any of the skip prefixes.
func hashTree(t *testing.T, root string, skip ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	must(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		for _, s := range skip {
			if strings.HasPrefix(rel, s) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	}))
	return out
}

func diffHashes(t *testing.T, before, after map[string]string) {
	t.Helper()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s: changed or missing", k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("%s: new file", k)
		}
	}
}

func queryInt(t *testing.T, path, q string, args ...any) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	must(t, err)
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %s: %v", filepath.Base(path), q, err)
	}
	return n
}

func queryString(t *testing.T, path, q string, args ...any) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	must(t, err)
	defer func() { _ = db.Close() }()
	var s string
	if err := db.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %s: %v", filepath.Base(path), q, err)
	}
	return s
}

// tableDump returns every table's row count, and the rows that mention the
// unrelated agent bobby, of one database — compared before and after.
func tableDump(t *testing.T, path string) (counts map[string]int, bobby []string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	must(t, err)
	defer func() { _ = db.Close() }()
	// FTS5 shadow tables (<vtable>_data, _idx, ...) are the index's own
	// storage: an UPDATE of an FTS row changes their row counts legitimately.
	rows, err := db.Query(`SELECT name, coalesce(sql, '') FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	must(t, err)
	var tables, virtual []string
	for rows.Next() {
		var n, ddl string
		must(t, rows.Scan(&n, &ddl))
		tables = append(tables, n)
		if strings.HasPrefix(strings.ToUpper(ddl), "CREATE VIRTUAL TABLE") {
			virtual = append(virtual, n)
		}
	}
	_ = rows.Close()
	isShadow := func(n string) bool {
		for _, v := range virtual {
			if strings.HasPrefix(n, v+"_") {
				return true
			}
		}
		return false
	}
	counts = map[string]int{}
	for _, tb := range tables {
		if isShadow(tb) {
			continue
		}
		var n int
		must(t, db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM %q`, tb)).Scan(&n))
		counts[tb] = n
		r, err := db.Query(fmt.Sprintf(`SELECT * FROM %q`, tb))
		must(t, err)
		cols, _ := r.Columns()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			must(t, r.Scan(ptrs...))
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					vals[i] = string(b)
				}
			}
			line := fmt.Sprint(vals...)
			// bobby's own rows: they mention bobby, and neither bob's keys
			// (before) nor rob's (after) — bob's rows may also name bobby.
			if strings.Contains(line, "bobby") && !strings.Contains(line, "bob/") && !strings.Contains(line, "rob") {
				bobby = append(bobby, tb+": "+line)
			}
		}
		_ = r.Close()
	}
	return counts, bobby
}

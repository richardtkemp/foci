package agentrename

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"foci/internal/ccaccount"
	"foci/internal/config"
	"foci/internal/fociroot"
	"foci/internal/trash"
)

// The agent delete (#3210): the disk half of the live `/agents delete`. It
// runs inside the gateway after the agent's live stop, as the gateway user,
// and removes everything foci keeps for the agent, after a backup:
//
//   - foci.toml: the agent's [[agents]] block and its sub-tables;
//   - secrets.toml: its [agents.<id>…] tables and its entries in
//     allowed_agents / denied_agents lists;
//   - the crontab: its foci lines and lines naming its workspace;
//   - its rows in state.db, the app replay store, deferred-sends.db and
//     jit_memory.db;
//   - its sessions tree (moved into the backup);
//   - its workspace and its Claude Code transcript folders (to the trash).
//
// The cost and usage ledgers (api.db, api-shadow.db, usage-history.db) keep
// the agent's rows: they record money spent, not the agent's state (Dick,
// 2026-11-21). The delete refuses, changing nothing, while another setting
// names the agent (config.AgentDeleteBlockers) or when it is the last agent.
// Like the rename it journals its steps, so a re-run resumes after a failure.

// deleteJournalName is the in-progress record under the data dir.
const deleteJournalName = "delete-agent.json"

// Delete step ids recorded in the journal (database steps use their spec id).
const (
	delStepBackup      = "backup"
	delStepConfig      = "foci.toml"
	delStepSecrets     = "secrets.toml"
	delStepCrontab     = "crontab"
	delStepSessions    = "sessions"
	delStepTranscripts = "transcripts"
	delStepWorkspace   = "workspace"
)

// DeleteOptions configures one delete run.
type DeleteOptions struct {
	ConfigPath string   // foci.toml (required)
	Backends   []string // registered backend names, for the config load check
	ID         string
	PlanOnly   bool // print the plan and return; change nothing
	Out        io.Writer
	Crontab    CrontabIO // default SystemCrontab
	Now        func() time.Time
	// Trash moves a path to the trash and returns where it went (default
	// trash.Move).
	Trash func(path string) (string, error)

	failDB string // tests: this database step fails just before its commit
}

// DeleteResult describes a delete (or its plan): Summary is a few lines
// saying what goes, for a confirmation prompt; Backup is the backup dir.
type DeleteResult struct {
	Summary []string
	Backup  string
}

// deletePaths are the locations a delete reads and removes, resolved once
// from the config and kept in the journal (foci.toml no longer names the
// agent once its block is gone).
type deletePaths struct {
	DataDir     string   `json:"data_dir"`
	SessionsDir string   `json:"sessions_dir"`
	Workspace   string   `json:"workspace"`
	CCProjects  []string `json:"cc_projects"`
	StateDB     string   `json:"state_db"`
	FramesDB    string   `json:"frames_db"`
	DeferDB     string   `json:"defer_db"`
	JITDB       string   `json:"jit_db"`
	// KeepWorkspace, when set, says why the workspace is not trashed (it
	// holds or sits in something else foci or another agent uses); its
	// databases then lose the agent's rows instead.
	KeepWorkspace       string `json:"keep_workspace,omitempty"`
	DefaultMemorySource bool   `json:"default_memory_source"`
}

// deleteJournal records a delete in progress.
type deleteJournal struct {
	ID        string          `json:"id"`
	BackupDir string          `json:"backup_dir"`
	Paths     deletePaths     `json:"paths"`
	Done      map[string]bool `json:"done"`
}

type deleter struct {
	ctx    context.Context
	o      DeleteOptions
	cfg    *config.Config
	p      deletePaths
	jr     *deleteJournal
	jpath  string
	backup string
	man    *manifest
	dbs    map[string]*dbHandle
	dbList []*dbHandle
	out    *printer
	sum    []string
}

// Delete deletes agent o.ID. See the comment at the top of this file.
func Delete(ctx context.Context, o DeleteOptions) (DeleteResult, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Crontab == nil {
		o.Crontab = SystemCrontab{}
	}
	if o.Trash == nil {
		o.Trash = func(p string) (string, error) { return trash.Move(p, o.Now) }
	}
	cfg, err := config.Load(o.ConfigPath, o.Backends)
	if err != nil {
		return DeleteResult{}, fmt.Errorf("load config: %w", err)
	}
	d := &deleter{ctx: ctx, o: o, cfg: cfg, out: &printer{w: o.Out}, dbs: map[string]*dbHandle{}}
	defer d.closeDBs()

	d.jpath = cfg.DataPath(deleteJournalName)
	if d.jr, err = readDeleteJournal(d.jpath); err != nil {
		return DeleteResult{}, err
	}
	if d.jr != nil && d.jr.ID != o.ID {
		return DeleteResult{}, fmt.Errorf("an unfinished delete of agent %q is recorded in %s: finish it first by re-running /agents delete %s (its backup is %s)",
			d.jr.ID, d.jpath, d.jr.ID, d.jr.BackupDir)
	}
	resume := d.jr != nil
	if resume {
		d.p, d.backup = d.jr.Paths, d.jr.BackupDir
		if d.man, err = readManifest(d.backup); err != nil {
			return DeleteResult{}, fmt.Errorf("read the backup manifest of the unfinished delete: %w", err)
		}
	} else {
		if err := validateDelete(cfg, o.ID); err != nil {
			return DeleteResult{}, err
		}
		if d.p, err = resolveDeletePaths(cfg, o.ID); err != nil {
			return DeleteResult{}, err
		}
		d.backup = filepath.Join(cfg.DataPath("backups"), fmt.Sprintf("delete-%s-%s", o.ID, o.Now().UTC().Format("20060102T150405Z")))
		d.man = &manifest{Old: o.ID, Created: o.Now().UTC()}
		d.jr = &deleteJournal{ID: o.ID, BackupDir: d.backup, Paths: d.p, Done: map[string]bool{}}
	}

	specs := d.specs()
	for _, s := range specs {
		if err := d.open(s.path); err != nil {
			return DeleteResult{}, err
		}
	}
	steps, err := d.plan(specs)
	if err != nil {
		return DeleteResult{}, err
	}
	d.printPlan(steps, resume)
	res := DeleteResult{Summary: d.sum, Backup: d.backup}
	if o.PlanOnly {
		return res, d.out.err
	}
	return res, d.execute(steps)
}

// validateDelete applies the fresh-run refusals; any failure changes nothing.
func validateDelete(cfg *config.Config, id string) error {
	if findAgent(cfg, id) == nil {
		return fmt.Errorf("no agent %q in %s", id, cfg.SourcePath)
	}
	if len(cfg.Agents) < 2 {
		return fmt.Errorf("%q is the only agent: the last agent cannot be deleted", id)
	}
	if b := config.AgentDeleteBlockers(cfg, id); len(b) > 0 {
		return fmt.Errorf("these settings name agent %q: %s. Point them at another agent first", id, strings.Join(b, "; "))
	}
	return nil
}

// resolveDeletePaths derives every location the delete touches.
func resolveDeletePaths(cfg *config.Config, id string) (deletePaths, error) {
	a := findAgent(cfg, id)
	cc, err := agentCCProjects(a)
	if err != nil {
		return deletePaths{}, err
	}
	replay := config.DefaultReplayStoreFile
	if pl := cfg.Platform("app"); pl != nil && pl.App != nil && pl.App.ReplayStorePath != "" {
		replay = pl.App.ReplayStorePath
	}
	p := deletePaths{
		DataDir:     config.ResolvePath(cfg.DataDir),
		SessionsDir: cfg.Sessions.Dir,
		Workspace:   filepath.Clean(config.ResolvePath(a.Workspace)),
		CCProjects:  cc,
		StateDB:     cfg.DataPath("state.db"),
		FramesDB:    cfg.DataPath(replay),
		DeferDB:     cfg.DataPath("deferred-sends.db"),
		JITDB:       cfg.DataPath("jit_memory.db"),
	}
	if p.DefaultMemorySource, err = usesDefaultMemorySource(cfg.SourcePath, id); err != nil {
		return deletePaths{}, err
	}
	p.KeepWorkspace = workspaceKeepReason(cfg, id, p)
	return p, nil
}

// workspaceKeepReason returns why the agent's workspace must not go to the
// trash, or "" when it may: it is not a plain directory of the agent's own,
// or it is, contains or sits in something foci or another agent uses.
func workspaceKeepReason(cfg *config.Config, id string, p deletePaths) string {
	ws := p.Workspace
	if !filepath.IsAbs(ws) || ws == string(filepath.Separator) {
		return fmt.Sprintf("the workspace path %q is not a directory of the agent's own", ws)
	}
	info, err := os.Lstat(ws)
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		return fmt.Sprintf("cannot read %s: %v", ws, err)
	}
	if !info.IsDir() {
		return fmt.Sprintf("%s is not a directory (a symlink or a file)", ws)
	}
	overlaps := func(other string) bool {
		other = filepath.Clean(other)
		return other == ws || strings.HasPrefix(other+"/", ws+"/") || strings.HasPrefix(ws+"/", other+"/")
	}
	protected := map[string]string{
		p.DataDir:                    "the data directory",
		p.SessionsDir:                "the sessions directory",
		filepath.Dir(cfg.SourcePath): "the config directory",
	}
	if home, err := os.UserHomeDir(); err == nil {
		protected[home] = "the home directory"
	}
	if root, err := fociroot.Dir(); err == nil {
		protected[root] = "the foci install root"
		protected[filepath.Join(root, "shared")] = "the shared directory"
	}
	for path, what := range protected {
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		// The workspace may sit IN the install root or home (that is the
		// default, ~/<id>); it must not BE one or contain one.
		if path == ws || strings.HasPrefix(path+"/", ws+"/") {
			return fmt.Sprintf("the workspace %s is or contains %s", ws, what)
		}
		if what != "the home directory" && what != "the foci install root" && strings.HasPrefix(ws+"/", path+"/") {
			return fmt.Sprintf("the workspace %s is inside %s", ws, what)
		}
	}
	for i := range cfg.Agents {
		other := cfg.Agents[i]
		if other.ID == id {
			continue
		}
		if overlaps(config.ResolvePath(other.Workspace)) {
			return fmt.Sprintf("the workspace %s is shared with agent %q", ws, other.ID)
		}
	}
	return ""
}

// agentCCProjects returns the Claude Code projects folder of every account of
// the agent (#3186): each one its backend_config.accounts lists; without a
// list, the host's.
func agentCCProjects(a *config.AgentConfig) ([]string, error) {
	folders := a.BackendConfig.Accounts
	if len(folders) == 0 {
		folders = []string{""}
	}
	var out []string
	for _, folder := range folders {
		cc, err := ccaccount.ForFolder(folder)
		if err != nil {
			return nil, err
		}
		if dir := cc.ProjectsDir(); !slices.Contains(out, dir) {
			out = append(out, dir)
		}
	}
	return out, nil
}

// delCond is one condition selecting a row to delete.
type delCond struct {
	col  string
	kind ruleKind // kindEq or kindPrefix
	val  string
}

// delTable deletes the rows of one table that match any of its conditions.
// systemFacets instead selects the "_system" agent_metadata rows whose value
// is a session key of the agent (the Telegram/Discord facet bindings).
type delTable struct {
	table        string
	conds        []delCond
	systemFacets bool
}

// delSpec is one database step of a delete.
type delSpec struct {
	id     string
	path   string
	tables []delTable
}

// specs returns every database step, in order.
func (d *deleter) specs() []delSpec {
	id := d.o.ID
	key := id + "/"
	agentID := delCond{"agent_id", kindEq, id}
	pre := func(c string) delCond { return delCond{c, kindPrefix, key} }
	t := func(name string, conds ...delCond) delTable { return delTable{table: name, conds: conds} }
	specs := []delSpec{
		{id: "db:state", path: d.p.StateDB, tables: []delTable{
			t("agent_metadata", agentID),
			{table: "agent_metadata", systemFacets: true},
			t("chat_metadata", agentID),
			t("session_metadata", pre("session_key")),
			t("session_index", pre("session_key"), agentID),
			t("session_archives", pre("session_key")),
			t("backend_resume_history", pre("session_key")),
		}},
		{id: "db:app-frames", path: d.p.FramesDB, tables: []delTable{
			t("app_frames", agentID), t("app_prompts", agentID), t("app_shell_runs", agentID),
		}},
		{id: "db:deferred-sends", path: d.p.DeferDB, tables: []delTable{t("deferred_sends", agentID, pre("session_key"))}},
		{id: "db:jit-memory", path: d.p.JITDB, tables: []delTable{
			t("runs", agentID, pre("session_key")), t("seen", pre("session_key")), t("sessions", pre("session_key")),
		}},
	}
	if d.p.KeepWorkspace != "" {
		// The workspace stays: its databases lose the agent's rows.
		ws := func(f string) string { return filepath.Join(d.p.Workspace, ".data", f) }
		specs = append(specs,
			delSpec{id: "ws:todo.db", path: ws("todo.db"), tables: []delTable{t("todos", agentID)}},
			delSpec{id: "ws:reminders.db", path: ws("reminders.db"), tables: []delTable{t("reminders", agentID, pre("session_key"))}},
			delSpec{id: "ws:scratchpad.db", path: ws("scratchpad.db"), tables: []delTable{t("scratchpad", agentID)}},
			delSpec{id: "ws:tasklist.db", path: ws("tasklist.db"), tables: []delTable{t("tasks", agentID)}},
			delSpec{id: "ws:conversation.db", path: ws("conversation.db"), tables: []delTable{t("messages", pre("session"))}},
		)
		if d.p.DefaultMemorySource {
			src := delCond{"source", kindEq, config.AgentMemorySourcePrefix + id}
			specs = append(specs, delSpec{id: "ws:memory.db", path: ws("memory.db"),
				tables: []delTable{t("memory_meta", src), t("memory_fts", src)}})
		}
	}
	return specs
}

func (d *deleter) open(path string) error {
	if path == "" || d.dbs[path] != nil {
		return nil
	}
	l, err := openDB(d.ctx, path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	d.dbs[path] = l
	d.dbList = append(d.dbList, l)
	return nil
}

func (d *deleter) closeDBs() {
	for _, l := range d.dbList {
		l.close()
	}
	d.dbList = nil
	d.dbs = map[string]*dbHandle{}
}

// live returns the table's conditions whose column exists (an older file may
// predate one), and whether the table can be used at all.
func (dt delTable) live(ctx context.Context, l *dbHandle) ([]delCond, bool, error) {
	if dt.systemFacets {
		ok, err := l.hasColumn(ctx, dt.table, "value")
		return nil, ok, err
	}
	var out []delCond
	for _, c := range dt.conds {
		ok, err := l.hasColumn(ctx, dt.table, c.col)
		if err != nil {
			return nil, false, err
		}
		if ok {
			out = append(out, c)
		}
	}
	return out, len(out) > 0, nil
}

// where builds the OR of conds with its arguments.
func where(conds []delCond) (string, []any) {
	var parts []string
	var args []any
	for _, c := range conds {
		if c.kind == kindPrefix {
			parts = append(parts, fmt.Sprintf("substr(%q, 1, length(?)) = ?", c.col))
			args = append(args, c.val, c.val)
		} else {
			parts = append(parts, fmt.Sprintf("%q = ?", c.col))
			args = append(args, c.val)
		}
	}
	return strings.Join(parts, " OR "), args
}

// facetRows returns the rowids of the "_system" rows valued by a session key
// of agent id.
func facetRows(ctx context.Context, q queryer, id string) ([]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT rowid, value FROM agent_metadata WHERE agent_id = '_system' AND value IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	r := ids{id, id}
	var out []int64
	for rows.Next() {
		var rid int64
		var v string
		if err := rows.Scan(&rid, &v); err != nil {
			return nil, err
		}
		if _, ok := r.rewriteKey(v); ok {
			out = append(out, rid)
		}
	}
	return out, rows.Err()
}

// count returns how many rows the table step deletes.
func (dt delTable) count(ctx context.Context, q queryer, id string, conds []delCond) (int, error) {
	if dt.systemFacets {
		r, err := facetRows(ctx, q, id)
		return len(r), err
	}
	w, args := where(conds)
	var n int
	err := q.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %q WHERE %s`, dt.table, w), args...).Scan(&n)
	return n, err
}

// apply deletes the table step's rows.
func (dt delTable) apply(ctx context.Context, q queryer, id string, conds []delCond) error {
	if dt.systemFacets {
		r, err := facetRows(ctx, q, id)
		if err != nil {
			return err
		}
		for _, rid := range r {
			if _, err := q.ExecContext(ctx, `DELETE FROM agent_metadata WHERE rowid = ?`, rid); err != nil {
				return err
			}
		}
		return nil
	}
	w, args := where(conds)
	_, err := q.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %q WHERE %s`, dt.table, w), args...)
	return err
}

// plan builds every step with its counts, in execution order.
func (d *deleter) plan(specs []delSpec) ([]step, error) {
	var steps []step
	id := d.o.ID

	// 1. Backup.
	steps = append(steps, step{
		id: delStepBackup, always: true,
		title:  "back up the databases, foci.toml and the crontab",
		detail: []string{"to " + d.backup},
		run:    d.runBackup,
	})

	// 2. foci.toml.
	cs, err := d.configStep()
	if err != nil {
		return nil, err
	}
	steps = append(steps, cs)

	// 3, 4. secrets.toml and the crontab.
	ss, err := d.secretsStep()
	if err != nil {
		return nil, err
	}
	steps = append(steps, ss, d.crontabStep())

	// 5. Databases.
	rows := 0
	for _, s := range specs {
		l := d.dbs[s.path]
		if l == nil {
			continue
		}
		st := step{id: s.id, title: "database " + s.path}
		type liveTable struct {
			t     delTable
			conds []delCond
		}
		var lts []liveTable
		for _, dt := range s.tables {
			conds, ok, err := dt.live(d.ctx, l)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			n, err := dt.count(d.ctx, l.conn, id, conds)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", s.path, dt.table, err)
			}
			lts = append(lts, liveTable{dt, conds})
			if n > 0 {
				label := dt.table
				if dt.systemFacets {
					label += " (facet bindings)"
				}
				st.detail = append(st.detail, fmt.Sprintf("delete %-36s %d row(s)", label, n))
				st.n += n
			}
		}
		rows += st.n
		spec := s
		st.run = func() error {
			return d.applyDB(spec, l, func(q queryer) error {
				for _, lt := range lts {
					if err := lt.t.apply(d.ctx, q, id, lt.conds); err != nil {
						return fmt.Errorf("%s: %w", lt.t.table, err)
					}
				}
				return nil
			})
		}
		steps = append(steps, st)
	}
	d.sum = append(d.sum, fmt.Sprintf("%d row(s) in foci's databases (cost and usage ledgers kept)", rows))

	// 6. Sessions tree.
	sess := filepath.Join(d.p.SessionsDir, id)
	st := step{id: delStepSessions, title: "sessions tree " + sess}
	if _, err := os.Stat(sess); err == nil {
		st.n = 1
		st.detail = []string{"move it into the backup"}
		d.sum = append(d.sum, "its sessions tree "+sess+" (moved into the backup)")
	}
	st.run = func() error { return d.moveIntoBackup(sess, "sessions") }
	steps = append(steps, st)

	// 7. Claude Code transcripts of the workspace (with the workspace only).
	if d.p.KeepWorkspace == "" {
		var dirs []string
		for _, dir := range d.p.CCProjects {
			p := filepath.Join(dir, ccSlug(d.p.Workspace))
			if _, err := os.Stat(p); err == nil {
				dirs = append(dirs, p)
			}
		}
		tt := step{id: delStepTranscripts, n: len(dirs), title: "Claude Code transcripts"}
		for _, p := range dirs {
			tt.detail = append(tt.detail, "move "+p+" to the trash")
		}
		if len(dirs) > 0 {
			d.sum = append(d.sum, fmt.Sprintf("its Claude Code transcripts (%d folder(s), to the trash)", len(dirs)))
		}
		tt.run = func() error { return d.trashAll(dirs) }
		steps = append(steps, tt)
	}

	// 8. Workspace.
	ws := step{id: delStepWorkspace, title: "workspace " + d.p.Workspace}
	switch {
	case d.p.KeepWorkspace != "":
		ws.detail = []string{"kept: " + d.p.KeepWorkspace + "; its databases lose the agent's rows (above)"}
		d.sum = append(d.sum, "workspace "+d.p.Workspace+" KEPT ("+d.p.KeepWorkspace+")")
	default:
		if _, err := os.Stat(d.p.Workspace); err == nil {
			ws.n = 1
			ws.detail = []string{"move it to the trash (character, memory, todos, search index, everything in it)"}
			d.sum = append(d.sum, "its workspace "+d.p.Workspace+" (to the trash)")
		}
		ws.run = func() error { return d.trashAll([]string{d.p.Workspace}) }
	}
	steps = append(steps, ws)
	return steps, nil
}

// applyDB runs fn on one database in one transaction: all or nothing.
func (d *deleter) applyDB(s delSpec, l *dbHandle, fn func(q queryer) error) error {
	tx, err := l.conn.BeginTx(d.ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if d.o.failDB == s.id {
		_ = tx.Rollback()
		return errors.New("simulated failure before commit")
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// runBackup snapshots every database the delete changes and foci.toml, and
// writes the manifest. The sessions tree, the secrets and crontab lines go
// into the backup in their own steps.
func (d *deleter) runBackup() error {
	if err := os.MkdirAll(filepath.Join(d.backup, "db"), 0o700); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}
	d.man.Files = nil
	for i, l := range d.dbList {
		rel := filepath.Join("db", fmt.Sprintf("%02d-%s", i, filepath.Base(l.path)))
		if err := l.backupTo(d.ctx, filepath.Join(d.backup, rel)); err != nil {
			return err
		}
		d.man.Files = append(d.man.Files, backupEntry{Orig: l.path, Backup: rel})
	}
	cfgPath := d.cfg.SourcePath
	rel := filepath.Join("config", filepath.Base(cfgPath))
	if err := copyFile(cfgPath, filepath.Join(d.backup, rel)); err != nil {
		return fmt.Errorf("back up %s: %w", cfgPath, err)
	}
	d.man.Files = append(d.man.Files, backupEntry{Orig: cfgPath, Backup: rel})
	return writeJSON(filepath.Join(d.backup, manifestName), d.man)
}

// configStep plans removing the agent's block from foci.toml and checks the
// result loads. Written in place (config.WriteConfigFile), as the rename does.
func (d *deleter) configStep() (step, error) {
	path := d.cfg.SourcePath
	st := step{id: delStepConfig, title: "config " + path}
	if findAgent(d.cfg, d.o.ID) == nil {
		return st, nil // resumed after this step
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	cur := string(data)
	next, err := config.PlanDeleteAgent(cur, d.o.ID)
	if err != nil {
		return st, err
	}
	if err := config.CheckFileContent(next, path, d.o.Backends); err != nil {
		return st, fmt.Errorf("foci.toml without agent %q would not load: %w", d.o.ID, err)
	}
	st.n = len(strings.Split(cur, "\n")) - len(strings.Split(next, "\n"))
	st.detail = indent(unifiedDiff(filepath.Base(path), cur, next, 1))
	if d.p.KeepWorkspace == "" {
		if refs := config.FindPathRefs(next, d.p.Workspace); len(refs) > 0 {
			st.detail = append(st.detail, fmt.Sprintf("WARNING: these lines still name the workspace %s, which goes to the trash (not edited):", d.p.Workspace))
			for _, r := range refs {
				st.detail = append(st.detail, fmt.Sprintf("  %s:%d: %s", filepath.Base(path), r.Line, r.Text))
			}
		}
	}
	d.sum = append(d.sum, fmt.Sprintf("its [[agents]] block in foci.toml (%d line(s))", st.n))
	st.run = func() error {
		now, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(now) != cur {
			return fmt.Errorf("%s changed since the plan was made; nothing written", path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		return config.WriteConfigFile(path, next, info.Mode().Perm())
	}
	return st, nil
}

// secretsStep plans removing the agent from secrets.toml. The plan shows the
// removed table headers and changed list lines only, never a secret value;
// the removed lines go into the backup (0600) before the file is written.
func (d *deleter) secretsStep() (step, error) {
	path := filepath.Join(filepath.Dir(d.cfg.SourcePath), "secrets.toml")
	st := step{id: delStepSecrets, title: "secrets " + path}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("read %s: %w", path, err)
	}
	cur := string(data)
	next, removed := removeSecrets(cur, d.o.ID)
	if next == cur {
		return st, nil
	}
	st.n = len(removed)
	var headers []string
	for _, l := range removed {
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			headers = append(headers, strings.TrimSpace(l))
		}
	}
	for _, h := range headers {
		st.detail = append(st.detail, "remove table "+h+" (values not shown)")
	}
	listChanges := 0
	for _, o := range diffOps(strings.Split(cur, "\n"), strings.Split(next, "\n")) {
		if o.kind == '+' {
			listChanges++
			st.detail = append(st.detail, "list now: "+strings.TrimSpace(o.text))
		}
	}
	st.n += listChanges
	what := []string{}
	if len(headers) > 0 {
		what = append(what, fmt.Sprintf("%d table(s)", len(headers)))
	}
	if listChanges > 0 {
		what = append(what, fmt.Sprintf("%d agent list(s)", listChanges))
	}
	if len(what) > 0 {
		d.sum = append(d.sum, "its secrets in secrets.toml ("+strings.Join(what, ", ")+")")
	}
	st.run = func() error {
		now, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(now) != cur {
			return fmt.Errorf("%s changed since the plan was made; nothing written", path)
		}
		if len(removed) > 0 {
			if err := writeBackupFile(filepath.Join(d.backup, "secrets.removed.toml"), []byte(strings.Join(removed, "\n")+"\n")); err != nil {
				return err
			}
		}
		return writeInPlace(path, next)
	}
	return st, nil
}

// crontabStep plans removing the agent's crontab lines.
func (d *deleter) crontabStep() step {
	st := step{id: delStepCrontab, title: "crontab"}
	cur, err := d.o.Crontab.Read()
	if err != nil {
		st.always = true
		st.detail = []string{fmt.Sprintf("cannot read the crontab: %v", err)}
		st.run = func() error { return fmt.Errorf("cannot read the crontab: %w", err) }
		return st
	}
	ws := ""
	if d.p.KeepWorkspace == "" {
		ws = d.p.Workspace
	}
	next, removed := removeCrontab(cur, d.o.ID, ws)
	st.n = len(removed)
	for _, l := range removed {
		st.detail = append(st.detail, "remove: "+l)
	}
	if st.n > 0 {
		d.sum = append(d.sum, fmt.Sprintf("%d crontab line(s)", st.n))
	}
	st.run = func() error { return installCrontab(d.o.Crontab, d.backup, d.man, cur, next) }
	return st
}

// moveIntoBackup moves src to <backup>/<name> (recorded first), copying when
// a rename cannot cross filesystems. A missing src is done already.
func (d *deleter) moveIntoBackup(src, name string) error {
	if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	dst := filepath.Join(d.backup, name)
	if err := recordMove(d.backup, d.man, src, dst); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	_ = os.RemoveAll(dst) // a partial copy from an earlier attempt
	if err := copyTree(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

// trashAll moves each existing path to the trash, recording where it went.
func (d *deleter) trashAll(paths []string) error {
	for _, p := range paths {
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		to, err := d.o.Trash(p)
		if err != nil {
			return err
		}
		if err := recordMove(d.backup, d.man, p, to); err != nil {
			return err
		}
		d.out.printf("         %s → %s\n", p, to)
	}
	return nil
}

func (d *deleter) saveJournal() error { return writeJSON(d.jpath, d.jr) }

// execute runs the steps in order; a failing step stops the run, and a re-run
// resumes after the last step done.
func (d *deleter) execute(steps []step) error {
	d.out.println()
	for i, s := range steps {
		if d.jr.Done[s.id] {
			continue
		}
		if s.n == 0 && !s.always {
			d.jr.Done[s.id] = true
			if err := d.saveJournal(); err != nil {
				return err
			}
			continue
		}
		if s.id == delStepTranscripts || s.id == delStepWorkspace {
			d.closeDBs()
		}
		if err := s.run(); err != nil {
			d.out.printf("FAILED   %s: %v\n", s.title, err)
			d.printFailure(steps, i)
			return fmt.Errorf("%s: %w", s.title, err)
		}
		d.out.printf("done     %s\n", s.title)
		d.jr.Done[s.id] = true
		if s.id == delStepBackup {
			d.out.printf("         backup: %s\n", d.backup)
		}
		if err := d.saveJournal(); err != nil {
			return err
		}
	}
	d.closeDBs()
	d.printChecklist()
	if err := os.Rename(d.jpath, filepath.Join(d.backup, "journal.json")); err != nil {
		return fmt.Errorf("finish: %w", err)
	}
	d.out.printf("\nDeleted agent %q.\n", d.o.ID)
	return nil
}

func (d *deleter) printFailure(steps []step, failed int) {
	d.out.println("\nThe delete stopped. The failed step changed nothing in its own database; earlier steps stay done.")
	for i, s := range steps {
		state := "not done"
		switch {
		case d.jr.Done[s.id]:
			state = "done"
		case i == failed:
			state = "FAILED"
		}
		d.out.printf("  %-8s %s\n", state, s.title)
	}
	d.out.printf("Fix the cause and re-run /agents delete %s to resume it. The backup is %s.\n", d.o.ID, d.backup)
}

func (d *deleter) printChecklist() {
	d.out.println("\nChecklist:")
	d.out.printf("  - Kept: the agent's rows in the cost and usage ledgers (api.db, api-shadow.db, usage-history.db).\n")
	d.out.printf("  - Not touched: opencode's and codex's own session stores, and helper scripts that name -a %s.\n", d.o.ID)
	d.out.printf("  - Backup (databases, foci.toml, sessions tree, removed secrets and crontab): %s\n", d.backup)
	if d.p.KeepWorkspace == "" {
		if dir, err := trash.Dir(); err == nil {
			d.out.printf("  - The workspace and transcripts are in the trash (%s) until it is emptied.\n", dir)
		}
	}
}

func (d *deleter) printPlan(steps []step, resume bool) {
	if resume {
		d.out.printf("Resuming the delete of agent %q.\n", d.o.ID)
	} else {
		d.out.printf("Delete agent %q.\n", d.o.ID)
	}
	printSteps(d.out, steps, d.jr.Done)
}

func readDeleteJournal(path string) (*deleteJournal, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j deleteJournal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if j.Done == nil {
		j.Done = map[string]bool{}
	}
	return &j, nil
}

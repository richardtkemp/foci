// Package agentrename renames a foci agent's id across everything that holds
// it (#1281): every SQLite store, the sessions tree, the per-agent search
// index, optionally the workspace directory, foci.toml, secrets.toml and the
// crontab. It runs INSIDE the gateway (#2787), between the live stop of the
// old agent and the live start of the new one, as the gateway's own user
// (which owns the data, may write foci.toml and secrets.toml in place, and
// owns the crontab). It shows a plan, backs up every file it changes, and
// resumes after a partial failure. docs/WIRING.md "Renaming an agent"
// describes the order.
package agentrename

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"foci/internal/config"
	"foci/internal/fociroot"
)

// journalName is the in-progress record under the data dir.
const journalName = "rename-agent.json"

// Step ids recorded in the journal (database steps use their dbSpec id).
const (
	stepBackup    = "backup"
	stepSessions  = "sessions"
	stepBleve     = "search-index"
	stepWorkspace = "workspace"
	stepConfig    = "foci.toml"
	stepSecrets   = "secrets.toml"
	stepCrontab   = "crontab"
)

// Options configures one rename run.
type Options struct {
	ConfigPath string   // foci.toml (required)
	Backends   []string // registered backend names, for the config load check
	Old, New   string
	// Move moves the workspace ~/<Old> to ~/<New>. Default (false) keeps it.
	Move     bool
	PlanOnly bool // print the plan and return; change nothing
	Out      io.Writer
	Crontab  CrontabIO // default SystemCrontab
	Now      func() time.Time

	failSessionsAfter int    // tests: the sessions-tree step fails after N file rewrites
	failDB            string // tests: this database step fails just before its commit
}

// step is one entry of the plan.
type step struct {
	id     string
	title  string
	detail []string
	n      int  // things to change; 0 = nothing to do
	always bool // runs even when n == 0
	// closeDBs: close the database handles before this step (a workspace
	// move must not happen under open SQLite handles).
	closeDBs bool
	run      func() error
}

type runner struct {
	ctx    context.Context
	o      Options
	cfg    *config.Config
	r      ids
	p      paths
	jr     *journal
	jpath  string
	backup string
	man    *manifest
	dbs    map[string]*dbHandle
	dbList []*dbHandle
	out    *printer
}

// printer writes the command's output and keeps the first write error.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, a...)
	}
}

func (p *printer) println(a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintln(p.w, a...)
	}
}

// Run renames o.Old to o.New. See the package doc.
func Run(ctx context.Context, o Options) error {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Crontab == nil {
		o.Crontab = SystemCrontab{}
	}
	cfg, err := config.Load(o.ConfigPath, o.Backends)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	rn := &runner{ctx: ctx, o: o, cfg: cfg, r: ids{o.Old, o.New}, out: &printer{w: o.Out}, dbs: map[string]*dbHandle{}}
	defer rn.closeDBs()

	rn.jpath = cfg.DataPath(journalName)
	if rn.jr, err = readJournal(rn.jpath); err != nil {
		return err
	}
	if rn.jr != nil && (rn.jr.Old != o.Old || rn.jr.New != o.New || rn.jr.Move != o.Move) {
		return fmt.Errorf("an unfinished rename %s → %s (move workspace: %v) is recorded in %s: finish it first by re-running it with the same arguments (its backup is %s)",
			rn.jr.Old, rn.jr.New, rn.jr.Move, rn.jpath, rn.jr.BackupDir)
	}
	resume := rn.jr != nil
	if resume {
		rn.p, rn.backup = rn.jr.Paths, rn.jr.BackupDir
		if rn.man, err = readManifest(rn.backup); err != nil {
			return fmt.Errorf("read the backup manifest of the unfinished rename: %w", err)
		}
		if findAgent(cfg, o.Old) == nil && findAgent(cfg, o.New) == nil {
			return fmt.Errorf("neither %q nor %q is an agent in %s", o.Old, o.New, o.ConfigPath)
		}
	} else {
		if err := validate(cfg, o); err != nil {
			return err
		}
		if rn.p, err = resolvePaths(cfg, o); err != nil {
			return err
		}
		rn.backup = filepath.Join(cfg.DataPath("backups"), fmt.Sprintf("rename-%s-%s-%s", o.Old, o.New, o.Now().UTC().Format("20060102T150405Z")))
		rn.man = &manifest{Old: o.Old, New: o.New, Move: o.Move, Created: o.Now().UTC()}
		rn.jr = &journal{Old: o.Old, New: o.New, Move: o.Move, BackupDir: rn.backup, Paths: rn.p, Done: map[string]bool{}}
	}

	specs := dbSpecs(rn.p, rn.r, rn.wsDBDir(), o.Move)
	if err := rn.openAll(specs); err != nil {
		return err
	}
	if !resume {
		if err := rn.checkNewUnused(); err != nil {
			return err
		}
	}

	steps, err := rn.plan(specs)
	if err != nil {
		return err
	}
	rn.printPlan(steps, resume)
	if o.PlanOnly {
		return rn.out.err
	}
	return rn.execute(steps)
}

// validate applies the fresh-run checks; any failure aborts before a change.
func validate(cfg *config.Config, o Options) error {
	if err := config.CheckAgentID(o.New); err != nil {
		return err
	}
	if o.New == o.Old {
		return fmt.Errorf("the new id is the old id (%q)", o.Old)
	}
	a := findAgent(cfg, o.Old)
	if a == nil {
		return fmt.Errorf("no agent %q in %s", o.Old, o.ConfigPath)
	}
	if findAgent(cfg, o.New) != nil {
		return fmt.Errorf("agent %q already exists in %s", o.New, o.ConfigPath)
	}
	root, err := fociroot.Dir()
	if err != nil {
		return err
	}
	ownWS := filepath.Clean(config.ResolvePath(a.Workspace))
	for _, p := range []string{filepath.Join(root, o.New), filepath.Join(cfg.Sessions.Dir, o.New)} {
		// ~/<new> may be the agent's own workspace, kept by an earlier
		// rename: the undo (renaming back) keeps it where it is.
		if p == ownWS && !o.Move {
			continue
		}
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("%s already exists: the new id %q is in use", p, o.New)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if o.Move {
		ws := config.ResolvePath(a.Workspace)
		if filepath.Clean(ws) != filepath.Join(root, o.Old) {
			return fmt.Errorf("--move-workspace moves %s to %s, but agent %q's workspace is %s; move it yourself or keep it (omit --move-workspace)",
				filepath.Join(root, o.Old), filepath.Join(root, o.New), o.Old, ws)
		}
	}
	return nil
}

func findAgent(cfg *config.Config, id string) *config.AgentConfig {
	for i := range cfg.Agents {
		if cfg.Agents[i].ID == id {
			return &cfg.Agents[i]
		}
	}
	return nil
}

// resolvePaths derives every location the rename touches from the config.
func resolvePaths(cfg *config.Config, o Options) (paths, error) {
	a := findAgent(cfg, o.Old)
	root, err := fociroot.Dir()
	if err != nil {
		return paths{}, err
	}
	cc, err := fociroot.CCConfigDir()
	if err != nil {
		return paths{}, err
	}
	ws := filepath.Clean(config.ResolvePath(a.Workspace))
	p := paths{
		DataDir:     config.ResolvePath(cfg.DataDir),
		SessionsDir: cfg.Sessions.Dir,
		OldWS:       ws,
		NewWS:       ws,
		CCProjects:  filepath.Join(cc, "projects"),
		StateDB:     cfg.DataPath("state.db"),
		APIDB:       cfg.Logging.APIDB,
		APIShadowDB: cfg.Logging.APIShadowDB,
		DeferDB:     cfg.DataPath("deferred-sends.db"),
	}
	replay := config.DefaultReplayStoreFile
	if pl := cfg.Platform("app"); pl != nil && pl.App != nil && pl.App.ReplayStorePath != "" {
		replay = pl.App.ReplayStorePath
	}
	p.FramesDB = cfg.DataPath(replay)
	if o.Move {
		p.NewWS = filepath.Join(root, o.New)
		home, err := os.UserHomeDir()
		if err != nil {
			return paths{}, err
		}
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" {
			data = filepath.Join(home, ".local", "share")
		}
		p.OpencodeDB = filepath.Join(data, "opencode", "opencode.db")
		codex := os.Getenv("CODEX_HOME")
		if codex == "" {
			codex = filepath.Join(home, ".codex")
		}
		p.CodexDBs, _ = filepath.Glob(filepath.Join(codex, "state_*.sqlite"))
	}
	p.DefaultMemorySource, err = usesDefaultMemorySource(cfg.SourcePath, o.Old)
	if err != nil {
		return paths{}, err
	}
	return p, nil
}

// usesDefaultMemorySource reports whether the agent's block in the config
// file names no memory sources, so its memory source is the default one,
// named after the agent id.
func usesDefaultMemorySource(configPath, id string) (bool, error) {
	var raw struct {
		Agents []struct {
			ID     string `toml:"id"`
			Memory struct {
				Sources []map[string]any `toml:"sources"`
			} `toml:"memory"`
		} `toml:"agents"`
	}
	if _, err := toml.DecodeFile(configPath, &raw); err != nil {
		return false, fmt.Errorf("read %s: %w", configPath, err)
	}
	for _, a := range raw.Agents {
		if a.ID == id {
			return len(a.Memory.Sources) == 0, nil
		}
	}
	return false, nil
}

func ccSlug(path string) string { return fociroot.CCProjectSlug(path) }

// wsDBDir is where the workspace databases are now.
func (rn *runner) wsDBDir() string {
	if rn.jr.Done[stepWorkspace] {
		return rn.p.NewWS
	}
	return rn.p.OldWS
}

// openAll opens every existing database the rename reads.
func (rn *runner) openAll(specs []dbSpec) error {
	for _, s := range specs {
		if s.path == "" || rn.dbs[s.path] != nil {
			continue
		}
		l, err := openDB(rn.ctx, s.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		rn.dbs[s.path] = l
		rn.dbList = append(rn.dbList, l)
	}
	return nil
}

// closeDBs closes every database handle (before a step that moves files a
// database lives in, and at the end).
func (rn *runner) closeDBs() {
	for _, l := range rn.dbList {
		l.close()
	}
	rn.dbList = nil
	rn.dbs = map[string]*dbHandle{}
}

// checkNewUnused refuses when any store already holds rows for the new id.
func (rn *runner) checkNewUnused() error {
	var found []string
	for _, s := range dbSpecs(rn.p, ids{rn.o.New, rn.o.New}, rn.wsDBDir(), false) {
		l := rn.dbs[s.path]
		if l == nil {
			continue
		}
		for _, u := range s.rules {
			if !u.idRule {
				continue
			}
			ok, err := u.present(rn.ctx, l)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			n, err := u.count(rn.ctx, l.conn)
			if err != nil {
				return fmt.Errorf("%s %s: %w", s.path, u.label(), err)
			}
			if n > 0 {
				found = append(found, fmt.Sprintf("%s: %s has %d row(s) for %q", s.path, u.label(), n, rn.o.New))
			}
		}
	}
	if len(found) > 0 {
		return fmt.Errorf("the new id %q is already used; nothing was changed:\n  %s", rn.o.New, strings.Join(found, "\n  "))
	}
	return nil
}

func (rn *runner) saveJournal() error { return writeJSON(rn.jpath, rn.jr) }

// execute runs the steps in order. A failing step stops the run; a re-run
// with the same arguments resumes after the last step done.
func (rn *runner) execute(steps []step) error {
	rn.out.println()
	for i, s := range steps {
		if rn.jr.Done[s.id] {
			continue
		}
		if s.closeDBs {
			rn.closeDBs()
		}
		if s.n == 0 && !s.always {
			rn.jr.Done[s.id] = true
			if err := rn.saveJournal(); err != nil {
				return err
			}
			continue
		}
		if err := s.run(); err != nil {
			rn.out.printf("FAILED   %s: %v\n", s.title, err)
			rn.printFailure(steps, i)
			return fmt.Errorf("%s: %w", s.title, err)
		}
		rn.out.printf("done     %s\n", s.title)
		rn.jr.Done[s.id] = true
		if s.id == stepBackup {
			rn.out.printf("         backup: %s\n", rn.backup)
		}
		if err := rn.saveJournal(); err != nil {
			return err
		}
	}
	rn.closeDBs()
	rn.printChecklist()
	// Finished: the journal moves into the backup, beside the manifest.
	if err := os.Rename(rn.jpath, filepath.Join(rn.backup, "journal.json")); err != nil {
		return fmt.Errorf("finish: %w", err)
	}
	rn.out.printf("\nRenamed %q to %q on disk.\n", rn.o.Old, rn.o.New)
	return nil
}

// printFailure says what is done, what is not, and how to recover.
func (rn *runner) printFailure(steps []step, failed int) {
	rn.out.println("\nThe rename stopped. The failed step changed nothing in its own database; earlier steps stay done.")
	for i, s := range steps {
		state := "not done"
		switch {
		case rn.jr.Done[s.id]:
			state = "done"
		case i == failed:
			state = "FAILED"
		}
		rn.out.printf("  %-8s %s\n", state, s.title)
	}
	rn.out.printf("Fix the cause and re-run the same rename to resume it. The backup of everything it changes is %s.\n", rn.backup)
}

// printChecklist prints what the user must still do, and what this command
// cannot reach.
func (rn *runner) printChecklist() {
	rn.out.println("\nChecklist:")
	rn.out.printf("  - The Android and desktop apps cache the agent id per conversation: a conversation they hold may still name %q until the app reconnects and reloads its conversation list.\n", rn.o.Old)
	refs := rn.scriptRefs()
	if len(refs) == 0 {
		rn.out.printf("  - No helper script under ~/shared/scripts or ~/scripts names -a %s.\n", rn.o.Old)
	} else {
		rn.out.printf("  - Helper scripts that still name %q (not edited):\n", rn.o.Old)
		for _, l := range refs {
			rn.out.printf("      %s\n", l)
		}
	}
	rn.out.printf("  - Backup: %s. Undo: /agents rename %s %s\n", rn.backup, rn.o.New, rn.o.Old)
}

// scriptRefs greps the helper-script dirs for exact references to the old id.
func (rn *runner) scriptRefs() []string {
	root, err := fociroot.Dir()
	if err != nil {
		return nil
	}
	re := cronAgentFlag(rn.o.Old)
	var out []string
	for _, dir := range []string{filepath.Join(root, "shared", "scripts"), filepath.Join(root, "scripts")} {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if info, err := d.Info(); err != nil || info.Size() > 1<<20 {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			for i, line := range strings.Split(string(data), "\n") {
				if re.MatchString(line) {
					out = append(out, fmt.Sprintf("%s:%d: %s", p, i+1, strings.TrimSpace(line)))
				}
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

package agentrename

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"foci/internal/config"
)

// plan builds every step with its counts, in execution order.
func (rn *runner) plan(specs []dbSpec) ([]step, error) {
	var steps []step

	// 1. Backup.
	if !rn.jr.Done[stepBackup] {
		steps = append(steps, step{
			id: stepBackup, always: true,
			title:  "back up the databases, foci.toml and the sessions tree",
			detail: []string{"to " + rn.backup, fmt.Sprintf("%d database(s), foci.toml, %s", len(rn.lockList), filepath.Join(rn.p.SessionsDir, rn.o.Old))},
			run:    rn.runBackup,
		})
	}

	// 2. Sessions tree.
	oldDir, newDir := filepath.Join(rn.p.SessionsDir, rn.o.Old), filepath.Join(rn.p.SessionsDir, rn.o.New)
	ss := step{id: stepSessions, title: "sessions tree " + oldDir + " → " + newDir}
	scanDir := oldDir
	if _, err := os.Stat(oldDir); err == nil {
		ss.n++
		ss.detail = append(ss.detail, "move "+oldDir+" → "+newDir)
	} else {
		scanDir = newDir
	}
	files, err := branchFilesToRewrite(scanDir, rn.r)
	if err != nil {
		return nil, err
	}
	ss.n += len(files)
	ss.detail = append(ss.detail, fmt.Sprintf("rewrite branch_meta parent_key in %d branch file(s) (gzip kept)", len(files)))
	ss.run = func() error {
		return sessionsStep(oldDir, newDir, rn.r, func(from, to string) error { return recordMove(rn.backup, rn.man, from, to) }, rn.o.failSessionsAfter)
	}
	steps = append(steps, ss)

	// 3. Databases.
	for _, s := range specs {
		l := rn.locks[s.path]
		if l == nil {
			continue
		}
		st, err := rn.dbStep(s, l)
		if err != nil {
			return nil, err
		}
		steps = append(steps, st)
	}

	// 4. The per-agent search index (bleve): rebuilt from the stores at startup.
	bleve := filepath.Join(rn.wsDBDir(), ".data", "search.bleve")
	if _, err := os.Stat(bleve); err == nil || rn.jr.Done[stepBleve] {
		steps = append(steps, step{
			id: stepBleve, n: 1, closeDBs: true,
			title:  "search index " + bleve,
			detail: []string{"move it into the backup; the gateway rebuilds it at startup, with the new id on every todo"},
			run: func() error {
				to := filepath.Join(rn.backup, "search.bleve")
				if err := recordMove(rn.backup, rn.man, bleve, to); err != nil {
					return err
				}
				return os.Rename(bleve, to)
			},
		})
	}

	// 5. Workspace move.
	if rn.o.Move {
		steps = append(steps, rn.workspaceStep())
	}

	// 6. foci.toml.
	cs, err := rn.configStep()
	if err != nil {
		return nil, err
	}
	steps = append(steps, cs)

	// 7, 8. Privileged.
	if s, ok, err := rn.secretsStep(); err != nil {
		return nil, err
	} else if ok {
		steps = append(steps, s)
	}
	steps = append(steps, rn.crontabStep())
	return steps, nil
}

// dbStep counts a database's changes and builds its all-or-nothing step.
func (rn *runner) dbStep(s dbSpec, l *lockedDB) (step, error) {
	st := step{id: s.id, title: "database " + s.path}
	var live []rule
	for _, u := range s.rules {
		ok, err := u.present(rn.ctx, l)
		if err != nil {
			return st, err
		}
		if !ok {
			continue
		}
		n, err := u.count(rn.ctx, l.conn)
		if err != nil {
			return st, fmt.Errorf("%s %s: %w", s.path, u.label(), err)
		}
		live = append(live, u)
		if n > 0 {
			st.detail = append(st.detail, fmt.Sprintf("%-40s %d row(s)", u.label(), n))
			st.n += n
		}
	}
	st.run = func() error { return rn.applyDB(s, l, live) }
	return st, nil
}

// applyDB rewrites one database in one transaction: every rule or none.
// Foreign keys are deferred to the commit, so turns.turn_id and
// api_calls.turn_id change together and the commit refuses a broken pair.
func (rn *runner) applyDB(s dbSpec, l *lockedDB, rules []rule) error {
	ctx := rn.ctx
	if s.fk {
		if _, err := l.conn.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
			return err
		}
	}
	tx, err := l.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if s.fk {
		if _, err := tx.ExecContext(ctx, "PRAGMA defer_foreign_keys = ON"); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	for _, u := range rules {
		if _, err := u.apply(ctx, tx); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if rn.o.failDB == s.id {
		_ = tx.Rollback()
		return errors.New("simulated failure before commit")
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// runBackup takes the backup and starts the journal: from here on a re-run resumes.
func (rn *runner) runBackup() error {
	if err := takeBackup(rn.ctx, rn.backup, rn.man, rn.lockList, rn.cfg.SourcePath, filepath.Join(rn.p.SessionsDir, rn.o.Old)); err != nil {
		return err
	}
	return nil
}

// workspaceStep moves the workspace and the Claude Code project folder of its cwd.
func (rn *runner) workspaceStep() step {
	ccFrom := filepath.Join(rn.p.CCProjects, ccSlug(rn.p.OldWS))
	ccTo := filepath.Join(rn.p.CCProjects, ccSlug(rn.p.NewWS))
	st := step{id: stepWorkspace, closeDBs: true, title: "workspace " + rn.p.OldWS + " → " + rn.p.NewWS}
	if _, err := os.Stat(rn.p.OldWS); err == nil {
		st.n++
		st.detail = append(st.detail, "move "+rn.p.OldWS+" → "+rn.p.NewWS)
	}
	if _, err := os.Stat(ccFrom); err == nil {
		st.n++
		st.detail = append(st.detail, "move "+ccFrom+" → "+ccTo)
	}
	st.detail = append(st.detail,
		"WARNING: Claude Code, opencode and codex resume a session by its id AND its working directory.",
		"         The CC project folder, opencode session.directory and codex threads.cwd follow the move,",
		"         but a backend that cannot find a moved session starts a fresh one (its context is lost).")
	st.run = func() error {
		for _, mv := range [][2]string{{rn.p.OldWS, rn.p.NewWS}, {ccFrom, ccTo}} {
			if _, err := os.Stat(mv[0]); errors.Is(err, fs.ErrNotExist) {
				continue // done already (resume) or never existed
			}
			if _, err := os.Stat(mv[1]); err == nil {
				return fmt.Errorf("%s already exists", mv[1])
			}
			if err := recordMove(rn.backup, rn.man, mv[0], mv[1]); err != nil {
				return err
			}
			if err := os.Rename(mv[0], mv[1]); err != nil {
				return err
			}
		}
		return nil
	}
	return st
}

// configRename is the foci.toml edit for this run.
func (rn *runner) configRename() config.AgentRename {
	ar := config.AgentRename{Old: rn.o.Old, New: rn.o.New}
	if a := findAgent(rn.cfg, rn.o.Old); a != nil {
		for _, pl := range a.Platforms {
			if pl.Bot == rn.o.Old {
				ar.PinBots = append(ar.PinBots, pl.ID)
			}
		}
	}
	if rn.o.Move {
		ar.MoveFrom, ar.MoveTo = rn.p.OldWS, rn.p.NewWS
	} else {
		ar.KeepWorkspace = rn.p.OldWS
	}
	return ar
}

// configStep plans the foci.toml edit and checks the result loads. It is a
// [sudo] step like secrets.toml (#2784): foci.toml is root:foci-secrets 0664
// (#479) and the foci user may not write it. The write is `sudo tee`, which
// truncates and rewrites the existing file in place, so it keeps its inode,
// owner, group and mode (a temp-and-rename write would replace it with a file
// the gateway refuses). The planned content is also saved in the backup as
// foci.toml.after, the source of the manual command for a PENDING step.
func (rn *runner) configStep() (step, error) {
	path := rn.cfg.SourcePath
	st := step{id: stepConfig, sudo: true, title: "config " + path}
	data, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	cur := string(data)
	refs := config.FindPathRefs(cur, rn.p.OldWS)
	if findAgent(rn.cfg, rn.o.Old) == nil {
		// Resumed after the config step: nothing left to do here.
		return st, nil
	}
	next, err := config.PlanRenameAgent(cur, rn.configRename())
	if err != nil {
		return st, err
	}
	if err := config.CheckFileContent(next, path, rn.o.Backends); err != nil {
		return st, fmt.Errorf("the renamed foci.toml would not load: %w", err)
	}
	diff := unifiedDiff(filepath.Base(path), cur, next, 1)
	st.n = changedLines(cur, next)
	st.detail = append(st.detail, indent(diff)...)
	if len(refs) > 0 {
		what := "listed only; rewritten with --move-workspace"
		if rn.o.Move {
			what = "rewritten"
		}
		st.detail = append(st.detail, fmt.Sprintf("paths under %s in foci.toml (%s):", rn.p.OldWS, what))
		for _, r := range refs {
			st.detail = append(st.detail, fmt.Sprintf("  %s:%d: %s", filepath.Base(path), r.Line, r.Text))
		}
	}
	after := filepath.Join(rn.backup, filepath.Base(path)+".after")
	st.manual = func() []string {
		return []string{fmt.Sprintf("sudo cp %s %s   # cp onto the existing file keeps its owner, group and mode", after, path)}
	}
	st.run = func() error {
		if err := os.WriteFile(after, []byte(next), 0o600); err != nil {
			return err
		}
		now, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(now) != cur {
			return fmt.Errorf("%s changed since the plan was made; nothing written", path)
		}
		if _, err := rn.o.Priv.Run([]string{"tee", path}, []byte(next)); err != nil {
			return err
		}
		if got, err := os.ReadFile(path); err != nil {
			return err
		} else if string(got) != next {
			return fmt.Errorf("%s does not hold the planned content after the write", path)
		}
		return nil
	}
	return st, nil
}

func indent(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// printPlan prints the ordered steps with their counts.
func (rn *runner) printPlan(steps []step, resume bool) {
	mode := "keep the workspace " + rn.p.OldWS
	if rn.o.Move {
		mode = "move the workspace " + rn.p.OldWS + " → " + rn.p.NewWS
	}
	if resume {
		rn.out.printf("Resuming the rename of agent %q to %q (%s).\n", rn.o.Old, rn.o.New, mode)
	} else {
		rn.out.printf("Rename agent %q to %q (%s).\n", rn.o.Old, rn.o.New, mode)
	}
	rn.out.println("\nPlan (steps marked [sudo] go through sudo approval):")
	i := 0
	for _, s := range steps {
		if rn.jr.Done[s.id] {
			continue
		}
		i++
		tag := ""
		if s.sudo {
			tag = "[sudo] "
		}
		count := fmt.Sprintf("%d change(s)", s.n)
		if s.n == 0 && !s.always {
			count = "nothing to change"
		}
		if s.always {
			count = ""
		}
		rn.out.printf("%3d. %s%s", i, tag, s.title)
		if count != "" {
			rn.out.printf(" — %s", count)
		}
		rn.out.println()
		for _, d := range s.detail {
			rn.out.printf("       %s\n", d)
		}
	}
}

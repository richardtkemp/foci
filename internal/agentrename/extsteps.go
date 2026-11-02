package agentrename

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// changedLines counts the lines a → b adds (a changed line counts once).
func changedLines(a, b string) int {
	n := 0
	for _, o := range diffOps(strings.Split(a, "\n"), strings.Split(b, "\n")) {
		if o.kind == '+' {
			n++
		}
	}
	return n
}

// secretsPath is secrets.toml beside foci.toml (as the gateway loads it).
func (rn *runner) secretsPath() string {
	return filepath.Join(filepath.Dir(rn.cfg.SourcePath), "secrets.toml")
}

// readSecrets reads secrets.toml. exists is false when there is no file.
// The gateway may read it (it holds the foci-secrets group, #479).
func (rn *runner) readSecrets() (content string, exists bool, err error) {
	data, err := os.ReadFile(rn.secretsPath())
	switch {
	case err == nil:
		return string(data), true, nil
	case errors.Is(err, fs.ErrNotExist):
		return "", false, nil
	}
	return "", true, err
}

// writeInPlace rewrites an existing file's content without replacing the
// file, so its inode, owner, group and mode survive: foci.toml and
// secrets.toml are root:foci-secrets, and a temp-and-rename write by the
// gateway would leave a file it then refuses (#479). The secrets store
// re-reads the file on its next use (it stats it on every read).
func writeInPlace(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// secretsStep plans the secrets.toml edit (per-agent tables and the
// allowed_agents / denied_agents lists). The diff carries no context lines:
// a context line of secrets.toml may be a secret value.
func (rn *runner) secretsStep() (step, bool, error) {
	path := rn.secretsPath()
	cur, exists, err := rn.readSecrets()
	if err != nil {
		return step{}, false, fmt.Errorf("read %s: %w", path, err)
	}
	if !exists {
		return step{}, false, nil
	}
	next := rewriteSecrets(cur, rn.o.Old, rn.o.New)
	st := step{id: stepSecrets, title: "secrets " + path, n: changedLines(cur, next)}
	st.detail = indent(unifiedDiff("secrets.toml", cur, next, 0))
	st.run = func() error {
		// The file must still be what the diff was made from.
		now, _, err := rn.readSecrets()
		if err != nil {
			return err
		}
		if now != cur {
			return fmt.Errorf("%s changed since its diff was made; nothing written", path)
		}
		return writeInPlace(path, next)
	}
	return st, true, nil
}

// crontabStep plans the crontab edit: exact -a <old> / --agent <old> /
// FOCI_AGENT=<old> references on foci lines, and the workspace paths in a
// move. Installed as the gateway user (whose crontab it is), after checking
// the crontab still is what the diff was made from.
func (rn *runner) crontabStep() step {
	st := step{id: stepCrontab, title: "crontab"}
	cur, err := rn.o.Crontab.Read()
	if err != nil {
		st.always = true
		st.detail = []string{fmt.Sprintf("cannot read the crontab: %v", err)}
		st.run = func() error { return fmt.Errorf("cannot read the crontab: %w", err) }
		return st
	}
	moveFrom, moveTo := "", ""
	if rn.o.Move {
		moveFrom, moveTo = rn.p.OldWS, rn.p.NewWS
	}
	next := rewriteCrontab(cur, rn.r, moveFrom, moveTo)
	st.n = changedLines(cur, next)
	st.detail = indent(unifiedDiff("crontab", cur, next, 1))
	st.run = func() error {
		if err := writeBackupFile(filepath.Join(rn.backup, "crontab.before"), []byte(cur)); err != nil {
			return err
		}
		rn.man.Crontab = "crontab.before"
		if err := writeJSON(filepath.Join(rn.backup, manifestName), rn.man); err != nil {
			return err
		}
		now, err := rn.o.Crontab.Read()
		if err != nil {
			return err
		}
		if now != cur {
			return errors.New("the crontab changed since its diff was made; nothing installed")
		}
		if err := rn.o.Crontab.Install(next); err != nil {
			return err
		}
		if got, err := rn.o.Crontab.Read(); err == nil && strings.TrimRight(got, "\n") != strings.TrimRight(next, "\n") {
			return errors.New("the installed crontab differs from the planned one")
		}
		return nil
	}
	return st
}

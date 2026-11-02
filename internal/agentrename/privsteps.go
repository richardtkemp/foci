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

// readSecretsDirect reads secrets.toml as this user. readable is false when
// the file exists but this user may not read it (it is root:foci-secrets);
// exists is false when there is no file.
func (rn *runner) readSecretsDirect() (content string, readable, exists bool, err error) {
	data, err := os.ReadFile(rn.secretsPath())
	switch {
	case err == nil:
		return string(data), true, true, nil
	case errors.Is(err, fs.ErrNotExist):
		return "", false, false, nil
	case errors.Is(err, fs.ErrPermission):
		return "", false, true, nil
	}
	return "", false, true, err
}

// readSecrets reads secrets.toml, through sudo when this user cannot.
func (rn *runner) readSecrets() (string, error) {
	c, readable, _, err := rn.readSecretsDirect()
	if err != nil || readable {
		return c, err
	}
	out, err := rn.o.Priv.Run([]string{"cat", rn.secretsPath()}, nil)
	if err != nil {
		return "", fmt.Errorf("read %s with sudo: %w", rn.secretsPath(), err)
	}
	return string(out), nil
}

// secretsStep plans the secrets.toml edit (per-agent tables and the
// allowed_agents / denied_agents lists). The diff carries no context lines:
// a context line of secrets.toml may be a secret value. When this user
// cannot read the file, the diff is shown — and confirmed — when the step
// runs, after the sudo read.
func (rn *runner) secretsStep() (step, bool, error) {
	path := rn.secretsPath()
	cur, readable, exists, err := rn.readSecretsDirect()
	if err != nil {
		return step{}, false, err
	}
	if !exists {
		return step{}, false, nil
	}
	st := step{id: stepSecrets, sudo: true, title: "secrets " + path}
	st.manual = func() []string {
		return []string{
			fmt.Sprintf("sudo sed -i -E '%s' %s", secretsSedScript(rn.o.Old, rn.o.New), path),
			fmt.Sprintf("sudo grep -n '%s' %s   # check: a multi-line allowed_agents/denied_agents list must be edited by hand", rn.o.Old, path),
		}
	}
	write := func(old, next string) error {
		// The file must still be what the diff was made from.
		now, err := rn.readSecrets()
		if err != nil {
			return err
		}
		if now != old {
			return fmt.Errorf("%s changed since its diff was made; nothing written", path)
		}
		if _, err := rn.o.Priv.Run([]string{"tee", path}, []byte(next)); err != nil {
			return err
		}
		if c, ok, _, err := rn.readSecretsDirect(); err == nil && ok && c != next {
			return fmt.Errorf("%s does not hold the planned content after the write", path)
		}
		return nil
	}
	if !readable {
		st.always = true
		st.detail = []string{"this user cannot read it: it is read with sudo when this step runs, and its diff shown and confirmed before the write"}
		st.run = func() error {
			old, err := rn.readSecrets()
			if err != nil {
				return err
			}
			next := rewriteSecrets(old, rn.o.Old, rn.o.New)
			if next == old {
				rn.out.printf("         %s: nothing to change\n", path)
				return nil
			}
			for _, l := range indent(unifiedDiff("secrets.toml", old, next, 0)) {
				rn.out.printf("         %s\n", l)
			}
			if !rn.o.Yes {
				if err := rn.confirm("Write this change to " + path + "?"); err != nil {
					return err
				}
			}
			return write(old, next)
		}
		return st, true, nil
	}
	next := rewriteSecrets(cur, rn.o.Old, rn.o.New)
	st.n = changedLines(cur, next)
	st.detail = indent(unifiedDiff("secrets.toml", cur, next, 0))
	st.run = func() error { return write(cur, next) }
	return st, true, nil
}

// crontabStep plans the crontab edit: exact -a <old> / --agent <old> /
// FOCI_AGENT=<old> references on foci lines, and the workspace paths in a
// move. Installed with sudo crontab -u <user> -, after checking the crontab
// still is what the diff was made from.
func (rn *runner) crontabStep() step {
	user := rn.currentUser()
	st := step{id: stepCrontab, sudo: true, title: "crontab of " + user}
	cur, err := rn.o.Crontab.Read()
	if err != nil {
		st.always = true
		st.detail = []string{fmt.Sprintf("cannot read the crontab: %v", err)}
		st.run = func() error { return fmt.Errorf("cannot read the crontab: %w", err) }
		st.manual = func() []string {
			return []string{"crontab -e   # rename -a " + rn.o.Old + " to -a " + rn.o.New + " on the foci lines"}
		}
		return st
	}
	moveFrom, moveTo := "", ""
	if rn.o.Move {
		moveFrom, moveTo = rn.p.OldWS, rn.p.NewWS
	}
	next := rewriteCrontab(cur, rn.r, moveFrom, moveTo)
	st.n = changedLines(cur, next)
	st.detail = indent(unifiedDiff("crontab", cur, next, 1))
	after := filepath.Join(rn.backup, "crontab.after")
	st.manual = func() []string {
		return []string{fmt.Sprintf("sudo crontab -u %s %s", user, after)}
	}
	st.run = func() error {
		before := filepath.Join(rn.backup, "crontab.before")
		if err := rn.own.writeFile(before, []byte(cur)); err != nil {
			return err
		}
		if err := rn.own.writeFile(after, []byte(next)); err != nil {
			return err
		}
		rn.man.Crontab = "crontab.before"
		if err := rn.own.writeJSON(filepath.Join(rn.backup, manifestName), rn.man); err != nil {
			return err
		}
		now, err := rn.o.Crontab.Read()
		if err != nil {
			return err
		}
		if now != cur {
			return errors.New("the crontab changed since its diff was made; nothing installed")
		}
		if _, err := rn.o.Priv.Run([]string{"crontab", "-u", user, "-"}, []byte(next)); err != nil {
			return err
		}
		if got, err := rn.o.Crontab.Read(); err == nil && strings.TrimRight(got, "\n") != strings.TrimRight(next, "\n") {
			return errors.New("the installed crontab differs from the planned one")
		}
		return nil
	}
	return st
}

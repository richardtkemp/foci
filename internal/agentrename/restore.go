package agentrename

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"foci/internal/config"
)

// RestoreOptions configures a restore of a rename's backup.
type RestoreOptions struct {
	ConfigPath  string
	Backends    []string
	BackupDir   string
	Yes         bool
	In          io.Reader
	Out         io.Writer
	Interactive bool
}

// Restore puts back the state a rename's backup recorded (the gateway must be
// stopped): every move is reversed and every backed-up file and tree copied
// back. The privileged edits (secrets.toml, crontab) are not reversed here;
// the output gives the commands.
func Restore(ctx context.Context, o RestoreOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	cfg, err := config.Load(o.ConfigPath, o.Backends)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	m, err := readManifest(o.BackupDir)
	if err != nil {
		return err
	}
	if err := checkGatewayStopped(cfg.DataPath("foci-gw.sock")); err != nil {
		return err
	}
	// Nothing may hold a database we are about to replace.
	for _, f := range m.Files {
		if !strings.HasPrefix(f.Backup, "db"+string(filepath.Separator)) {
			continue
		}
		l, err := lockDB(ctx, f.Orig)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if errors.Is(err, errInUse) {
			return fmt.Errorf("%s is open in another process: stop the gateway first; nothing was changed", f.Orig)
		}
		if err != nil {
			return err
		}
		l.close()
	}

	pr := &printer{w: o.Out}
	pr.printf("Restore the rename of %q to %q from %s:\n", m.Old, m.New, o.BackupDir)
	for i := len(m.Moves) - 1; i >= 0; i-- {
		pr.printf("  move back %s → %s\n", m.Moves[i].To, m.Moves[i].From)
	}
	for _, t := range m.Trees {
		pr.printf("  copy back the tree %s\n", t.Orig)
	}
	for _, f := range m.Files {
		pr.printf("  copy back %s\n", f.Orig)
	}
	if !o.Yes {
		rn := &runner{o: Options{In: o.In, Interactive: o.Interactive}, out: pr}
		if err := rn.confirm("Restore?"); err != nil {
			return err
		}
	}
	if err := restoreBackup(o.BackupDir, m); err != nil {
		return err
	}
	// An unfinished rename's journal belongs to the state just undone.
	jpath := cfg.DataPath(journalName)
	if _, err := os.Stat(jpath); err == nil {
		if err := os.Rename(jpath, filepath.Join(o.BackupDir, "journal.restored.json")); err != nil {
			return err
		}
	}
	pr.println("\nRestored. Not reversed by this command:")
	if m.Crontab != "" {
		pr.printf("  - the crontab: sudo crontab -u <user> %s\n", filepath.Join(o.BackupDir, m.Crontab))
	}
	pr.printf("  - secrets.toml (if its step ran): sudo sed -i -E '%s' %s\n",
		secretsSedScript(m.New, m.Old), filepath.Join(filepath.Dir(cfg.SourcePath), "secrets.toml"))
	return pr.err
}

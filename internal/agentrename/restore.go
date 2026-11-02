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
	// Priv writes foci.toml back (default sudo; DirectRunner when Owner is
	// set): the foci user may not write the root:foci-secrets file (#479, #2784).
	Priv Privileged
	// Owner: the command runs as root (#2786); see Options.Owner.
	Owner *Owner

	fakeOwners ownerFS // tests: simulated chown and owners (a root run without root)
}

// Restore puts back the state a rename's backup recorded (the gateway must be
// stopped): every move is reversed and every backed-up file and tree copied
// back. foci.toml is written back in place through o.Priv (`sudo tee`, #2784);
// if that is declined or fails, everything else is still restored, the output
// gives the manual command and the result is ErrPending. The other privileged
// edits (secrets.toml, crontab) are not reversed here; the output gives the
// commands.
func Restore(ctx context.Context, o RestoreOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Priv == nil && o.Owner != nil {
		o.Priv = DirectRunner{}
	}
	if o.Priv == nil {
		o.Priv = SudoRunner{}
	}
	cfg, err := config.Load(o.ConfigPath, o.Backends)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	own := newOwnership(o.Owner, o.fakeOwners)
	if err := own.checkDataDir(config.ResolvePath(cfg.DataDir)); err != nil {
		return err
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
		if err := own.sidecars(f.Orig); err != nil {
			return err
		}
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
	var cfgPending []string
	writeConfig := func(backup, orig string) error {
		data, err := os.ReadFile(backup)
		if err != nil {
			return err
		}
		if _, err := os.Stat(orig); err != nil {
			// tee needs the live file to keep its owner and mode; a missing one
			// is put back by hand.
			cfgPending = append(cfgPending, fmt.Sprintf("sudo cp %s %s", backup, orig))
			pr.printf("PENDING  %s: %v\n", orig, err)
			return nil
		}
		if _, err := o.Priv.Run([]string{"tee", orig}, data); err != nil {
			cfgPending = append(cfgPending, fmt.Sprintf("sudo cp %s %s   # cp onto the existing file keeps its owner, group and mode", backup, orig))
			pr.printf("PENDING  %s: %v\n", orig, err)
			return nil
		}
		if got, err := os.ReadFile(orig); err != nil {
			return err
		} else if string(got) != string(data) {
			return fmt.Errorf("%s does not hold the backed-up content after the write", orig)
		}
		return nil
	}
	if err := own.restoreBackup(o.BackupDir, m, writeConfig); err != nil {
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
		cronUser := "<user>"
		if o.Owner != nil {
			cronUser = o.Owner.User
		}
		pr.printf("  - the crontab: sudo crontab -u %s %s\n", cronUser, filepath.Join(o.BackupDir, m.Crontab))
	}
	pr.printf("  - secrets.toml (if its step ran): sudo sed -i -E '%s' %s\n",
		secretsSedScript(m.New, m.Old), filepath.Join(filepath.Dir(cfg.SourcePath), "secrets.toml"))
	if len(cfgPending) > 0 {
		pr.println("\nPENDING: foci.toml was not written back (sudo was declined or failed). Run:")
		for _, c := range cfgPending {
			pr.printf("    %s\n", c)
		}
		if pr.err != nil {
			return pr.err
		}
		return ErrPending
	}
	return pr.err
}

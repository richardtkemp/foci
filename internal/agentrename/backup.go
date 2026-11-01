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
	"sort"
	"time"

	"foci/internal/config"
)

// manifest describes one rename's backup: what was copied from where, and
// every move the rename made (recorded BEFORE the move happens, so a crash
// mid-move still leaves the record). Restore replays it backwards.
type manifest struct {
	Old     string    `json:"old"`
	New     string    `json:"new"`
	Move    bool      `json:"move_workspace"`
	Created time.Time `json:"created"`
	// Files: single files copied byte for byte (databases after a WAL
	// checkpoint, foci.toml). Backup is relative to the backup dir.
	Files []backupEntry `json:"files"`
	// Trees: directories copied whole (the agent's sessions tree).
	Trees []backupEntry `json:"trees"`
	// Moves: renames the command made, in order.
	Moves []moveEntry `json:"moves"`
	// Crontab: the crontab before the rename (relative path), when one was edited.
	Crontab string `json:"crontab,omitempty"`
}

type backupEntry struct {
	Orig   string `json:"orig"`
	Backup string `json:"backup"`
	// Config marks the foci.toml entry: restore writes it back IN PLACE
	// (config.WriteConfigFile), so the file keeps the root:foci-secrets
	// owner and mode the gateway requires (#479). Every other file is
	// restored by temp-and-rename.
	Config bool `json:"config,omitempty"`
}

type moveEntry struct {
	From string `json:"from"`
	To   string `json:"to"`
}

const manifestName = "manifest.json"

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(path, 0o600, time.Time{}, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

func readManifest(dir string) (*manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", manifestName, err)
	}
	return &m, nil
}

// recordMove appends a move to the manifest on disk.
func recordMove(dir string, m *manifest, from, to string) error {
	m.Moves = append(m.Moves, moveEntry{From: from, To: to})
	return writeJSON(filepath.Join(dir, manifestName), m)
}

// copyFile copies src to dst byte for byte, keeping mode and mtime. dst's
// directory is created.
func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	return writeFileAtomic(dst, info.Mode().Perm(), info.ModTime(), func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}

// writeFileAtomic replaces dst in one step: fill writes a temp file beside
// it, which is synced, given perm (and mtime, when not zero), then renamed
// over dst. A reader never sees a half-written file. Not for foci.toml,
// which is written in place (config.WriteConfigFile, #479).
func writeFileAtomic(dst string, perm os.FileMode, mtime time.Time, fill func(io.Writer) error) error {
	tmp := dst + ".copy-tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := fill(out); err != nil {
		return fail(err)
	}
	if err := out.Sync(); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(tmp, mtime, mtime); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	return os.Rename(tmp, dst)
}

// copyTree copies every regular file under src to the same relative path under dst.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(filepath.Join(dst, rel), info.Mode().Perm()|0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(p, filepath.Join(dst, rel))
	})
}

// takeBackup copies every file the rename will modify into dir: each locked
// database (after folding its WAL into the main file, so the copy is the
// whole, consistent database), foci.toml and the agent's sessions tree. It
// writes the manifest last-but-not-least: a failure leaves an incomplete
// backup dir and nothing else changed.
func takeBackup(ctx context.Context, dir string, m *manifest, dbs []*lockedDB, configPath, sessionsOld string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}
	for i, l := range dbs {
		if err := l.checkpoint(ctx); err != nil {
			return err
		}
		rel := filepath.Join("db", fmt.Sprintf("%02d-%s", i, filepath.Base(l.path)))
		if err := copyFile(l.path, filepath.Join(dir, rel)); err != nil {
			return fmt.Errorf("back up %s: %w", l.path, err)
		}
		m.Files = append(m.Files, backupEntry{Orig: l.path, Backup: rel})
	}
	if err := copyFile(configPath, filepath.Join(dir, "config", filepath.Base(configPath))); err != nil {
		return fmt.Errorf("back up %s: %w", configPath, err)
	}
	m.Files = append(m.Files, backupEntry{Orig: configPath, Backup: filepath.Join("config", filepath.Base(configPath)), Config: true})
	if _, err := os.Stat(sessionsOld); err == nil {
		if err := copyTree(sessionsOld, filepath.Join(dir, "sessions")); err != nil {
			return fmt.Errorf("back up %s: %w", sessionsOld, err)
		}
		m.Trees = append(m.Trees, backupEntry{Orig: sessionsOld, Backup: "sessions"})
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return writeJSON(filepath.Join(dir, manifestName), m)
}

// restoreBackup puts back the state the manifest recorded: it reverses every
// recorded move (newest first, each only if it is still in place), then
// copies every backed-up file and tree over its original path. A database's
// stale -wal/-shm beside the restored file is removed (the copy is a whole
// database). The caller has checked that the gateway is stopped.
func restoreBackup(dir string, m *manifest) error {
	for i := len(m.Moves) - 1; i >= 0; i-- {
		mv := m.Moves[i]
		_, toErr := os.Stat(mv.To)
		_, fromErr := os.Stat(mv.From)
		switch {
		case toErr == nil && errors.Is(fromErr, fs.ErrNotExist):
			if err := os.MkdirAll(filepath.Dir(mv.From), 0o755); err != nil {
				return err
			}
			if err := os.Rename(mv.To, mv.From); err != nil {
				return fmt.Errorf("move %s back to %s: %w", mv.To, mv.From, err)
			}
		case fromErr == nil:
			// already back (or never moved)
		default:
			return fmt.Errorf("cannot reverse move %s → %s: neither path is in place", mv.From, mv.To)
		}
	}
	for _, t := range m.Trees {
		if err := copyTree(filepath.Join(dir, t.Backup), t.Orig); err != nil {
			return fmt.Errorf("restore %s: %w", t.Orig, err)
		}
	}
	files := append([]backupEntry(nil), m.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Orig < files[j].Orig })
	for _, f := range files {
		if f.Config {
			if err := restoreConfigInPlace(filepath.Join(dir, f.Backup), f.Orig); err != nil {
				return fmt.Errorf("restore %s: %w", f.Orig, err)
			}
			continue
		}
		if err := copyFile(filepath.Join(dir, f.Backup), f.Orig); err != nil {
			return fmt.Errorf("restore %s: %w", f.Orig, err)
		}
		for _, side := range []string{"-wal", "-shm"} {
			if err := os.Remove(f.Orig + side); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// restoreConfigInPlace writes the backed-up foci.toml over the live one in
// place (config.WriteConfigFile), keeping the live file's inode, owner, group
// and mode (#479). The backup's mode applies only if the live file is gone.
func restoreConfigInPlace(backup, orig string) error {
	info, err := os.Stat(backup)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(backup)
	if err != nil {
		return err
	}
	return config.WriteConfigFile(orig, string(data), info.Mode().Perm())
}

// journal records a rename in progress, so a re-run with the same arguments
// resumes: completed steps are skipped and the backup is not retaken.
type journal struct {
	Old       string          `json:"old"`
	New       string          `json:"new"`
	Move      bool            `json:"move_workspace"`
	BackupDir string          `json:"backup_dir"`
	Paths     paths           `json:"paths"`
	Done      map[string]bool `json:"done"`
}

func readJournal(path string) (*journal, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if j.Done == nil {
		j.Done = map[string]bool{}
	}
	return &j, nil
}

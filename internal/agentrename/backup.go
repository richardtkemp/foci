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
	"time"
)

// manifest describes one rename's backup: what was copied from where, and
// every move the rename made (recorded BEFORE the move happens, so a crash
// mid-move still leaves the record). The backup is for recovery by hand; the
// undo of a finished rename is the reverse rename (/agents rename NEW OLD),
// which, unlike copying whole shared databases back, does not throw away what
// the other agents wrote since.
type manifest struct {
	Old     string    `json:"old"`
	New     string    `json:"new"`
	Move    bool      `json:"move_workspace"`
	Created time.Time `json:"created"`
	// Files: single files (a VACUUM INTO snapshot of each database,
	// foci.toml). Backup is relative to the backup dir.
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
// over dst. A reader never sees a half-written file. Not for foci.toml or
// secrets.toml, which are written in place (their owner and mode must
// survive, #479).
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

// takeBackup copies every file the rename will modify into dir: a VACUUM
// INTO snapshot of each database (consistent while the gateway's other
// connections keep writing), foci.toml and the agent's sessions tree. It
// writes the manifest last: a failure leaves an incomplete backup dir and
// nothing else changed.
func takeBackup(ctx context.Context, dir string, m *manifest, dbs []*dbHandle, configPath, sessionsOld string) error {
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o700); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}
	for i, l := range dbs {
		rel := filepath.Join("db", fmt.Sprintf("%02d-%s", i, filepath.Base(l.path)))
		if err := l.backupTo(ctx, filepath.Join(dir, rel)); err != nil {
			return err
		}
		m.Files = append(m.Files, backupEntry{Orig: l.path, Backup: rel})
	}
	if err := copyFile(configPath, filepath.Join(dir, "config", filepath.Base(configPath))); err != nil {
		return fmt.Errorf("back up %s: %w", configPath, err)
	}
	m.Files = append(m.Files, backupEntry{Orig: configPath, Backup: filepath.Join("config", filepath.Base(configPath))})
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

// writeBackupFile writes a small backup file (0600) in one step.
func writeBackupFile(path string, data []byte) error {
	return writeFileAtomic(path, 0o600, time.Time{}, func(out io.Writer) error {
		_, err := out.Write(data)
		return err
	})
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

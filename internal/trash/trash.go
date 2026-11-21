// Package trash moves a file or directory into the user's trash instead of
// deleting it, so a mistaken delete can be undone by hand (#3210: a deleted
// agent's workspace and transcripts go to the trash).
//
// On Linux (and any other non-darwin system) it follows the freedesktop.org
// Trash specification for the home trash: the item moves to
// $XDG_DATA_HOME/Trash/files/<name> (default ~/.local/share/Trash) beside an
// info/<name>.trashinfo record of its original path and deletion time, so
// trash-cli (trash-list, trash-restore, trash-empty) and desktop file managers
// see it. On macOS it moves to ~/.Trash.
//
// The move is a rename: the item must be on the same filesystem as the trash.
// Across filesystems Move fails and leaves the item where it is.
package trash

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Dir returns the trash directory Move uses: ~/.Trash on macOS, else the
// freedesktop home trash ($XDG_DATA_HOME/Trash, default
// ~/.local/share/Trash).
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, ".Trash"), nil
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, "Trash"), nil
}

// Move moves path into the trash and returns where it now is. now stamps the
// freedesktop deletion date (nil: time.Now).
func Move(path string, now func() time.Time) (string, error) {
	if now == nil {
		now = time.Now
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(abs); err != nil {
		return "", err
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return moveDarwin(abs, dir)
	}
	return moveFreedesktop(abs, dir, now())
}

// moveDarwin renames abs into ~/.Trash under a free name.
func moveDarwin(abs, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	base := filepath.Base(abs)
	for i := 0; ; i++ {
		dst := filepath.Join(dir, candidate(base, i))
		if _, err := os.Lstat(dst); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if err := os.Rename(abs, dst); err != nil {
			return "", fmt.Errorf("move %s to the trash: %w", abs, err)
		}
		return dst, nil
	}
}

// moveFreedesktop claims a name by creating its .trashinfo exclusively (the
// specification's way to avoid two deleters racing for one name), then
// renames abs into files/. A failed rename removes the claimed record.
func moveFreedesktop(abs, dir string, at time.Time) (string, error) {
	files, info := filepath.Join(dir, "files"), filepath.Join(dir, "info")
	for _, d := range []string{files, info} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
	}
	base := filepath.Base(abs)
	record := "[Trash Info]\nPath=" + escapePath(abs) + "\nDeletionDate=" + at.Format("2006-01-02T15:04:05") + "\n"
	for i := 0; ; i++ {
		name := candidate(base, i)
		infoPath := filepath.Join(info, name+".trashinfo")
		f, err := os.OpenFile(infoPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		dst := filepath.Join(files, name)
		if _, err := os.Lstat(dst); err == nil {
			// A stray file with no record holds the name: take the next.
			_ = f.Close()
			_ = os.Remove(infoPath)
			continue
		}
		_, werr := f.WriteString(record)
		cerr := f.Close()
		if err := errors.Join(werr, cerr); err != nil {
			_ = os.Remove(infoPath)
			return "", err
		}
		if err := os.Rename(abs, dst); err != nil {
			_ = os.Remove(infoPath)
			return "", fmt.Errorf("move %s to the trash: %w", abs, err)
		}
		return dst, nil
	}
}

// candidate is the i-th name tried for base: base, base.2, base.3, ...
func candidate(base string, i int) string {
	if i == 0 {
		return base
	}
	return base + "." + strconv.Itoa(i+1)
}

// escapePath percent-encodes an absolute path for a .trashinfo Path= line
// (RFC 2396 escaping, keeping the slashes).
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

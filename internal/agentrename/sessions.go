package agentrename

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"foci/internal/session"
)

// isSessionFile reports whether name is a session transcript (live, archive
// or child; plain or gzipped).
func isSessionFile(name string) bool {
	return strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".jsonl.gz")
}

// branchFilesToRewrite lists the session files under dir whose first-line
// branch_meta parent_key is a session key of agent r.old.
func branchFilesToRewrite(dir string, r ids) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == dir {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() || !isSessionFile(d.Name()) {
			return nil
		}
		parent, ok, err := session.ReadBranchParent(p)
		if err != nil {
			return err
		}
		if _, isOld := r.rewriteKey(parent); ok && isOld {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

// sessionsStep moves <sessions>/<old> to <sessions>/<new> and rewrites the
// branch_meta parent_key of every moved file. Each part is idempotent, so a
// resumed run finishes a half-done step: the move is skipped once done, and
// a file already rewritten no longer matches. onMove records the move in the
// backup manifest before it happens. failAfter > 0 (tests) fails the step
// after that many file rewrites.
func sessionsStep(oldDir, newDir string, r ids, onMove func(from, to string) error, failAfter int) error {
	if _, err := os.Stat(oldDir); err == nil {
		if _, err := os.Stat(newDir); err == nil {
			return errors.New(newDir + " and " + oldDir + " both exist; refusing to merge session trees")
		}
		if err := onMove(oldDir, newDir); err != nil {
			return err
		}
		if err := os.Rename(oldDir, newDir); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	files, err := branchFilesToRewrite(newDir, r)
	if err != nil {
		return err
	}
	for i, f := range files {
		if failAfter > 0 && i == failAfter {
			return errors.New("simulated failure in the sessions-tree step")
		}
		if _, err := session.RewriteBranchParent(f, r.rewriteKey); err != nil {
			return err
		}
	}
	return nil
}

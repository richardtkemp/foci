package session

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/log"
)

// ArchiveSweep gzips idle session files older than maxAge.
// It queries the index for active sessions whose last activity is older than
// maxAge, skips sessions with active branches, compresses the JSONL files to
// .jsonl.gz, removes the originals, and updates the index status to archived.
// Any numbered archive files (.N.jsonl) in the same directory are also gzipped.
// Returns the number of archived sessions.
func ArchiveSweep(store *Store, index *SessionIndex, maxAge time.Duration) (int, error) {
	if index == nil {
		return 0, nil
	}

	// Find active sessions older than maxAge
	candidates, err := index.Query(QueryOptions{
		Status: string(SessionStatusActive),
	})
	if err != nil {
		return 0, fmt.Errorf("query active sessions: %w", err)
	}

	cutoff := time.Now().UTC().Add(-maxAge)
	archived := 0

	for _, entry := range candidates {
		// Only archive sessions whose last activity is older than the cutoff
		lastActivity := entry.LastActivityAt
		if lastActivity.IsZero() {
			lastActivity = entry.CreatedAt
		}
		if lastActivity.After(cutoff) {
			continue
		}

		// Skip sessions that have active branches referencing them
		branches, err := index.Query(QueryOptions{})
		if err != nil {
			log.Warnf("session", "archive sweep: query branches for %s: %v", entry.SessionKey, err)
			continue
		}
		hasActiveBranch := false
		for _, b := range branches {
			if b.ParentSessionKey == entry.SessionKey && b.Status == SessionStatusActive {
				hasActiveBranch = true
				break
			}
		}
		if hasActiveBranch {
			continue
		}

		// Gzip the session file
		if err := gzipFile(entry.FilePath); err != nil {
			log.Warnf("session", "archive sweep: gzip %s: %v", entry.FilePath, err)
			continue
		}

		// Also gzip any numbered archive files in the same directory
		gzipArchiveFiles(entry.FilePath)

		// Update index status
		index.UpdateStatus(entry.SessionKey, SessionStatusArchived)
		archived++
		log.Infof("session", "archived session %s (last active %s)", entry.SessionKey, lastActivity.Format(time.RFC3339))
	}

	return archived, nil
}

// gzipFile compresses src to src.gz and removes the original.
func gzipFile(src string) error {
	in, err := os.Open(src)
	if os.IsNotExist(err) {
		return nil // already gone
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	dst := src + ".gz"
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}

	gw := gzip.NewWriter(out)
	if _, err := io.Copy(gw, in); err != nil {
		_ = gw.Close()
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("compress %s: %w", src, err)
	}
	if err := gw.Close(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("finalize gzip %s: %w", src, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("close %s: %w", dst, err)
	}

	return os.Remove(src)
}

// gzipArchiveFiles finds and gzips archive files for the same session.
// For example, if basePath is "chat/123.jsonl", this gzips "chat/123.2026-03-04T02-30-00Z.jsonl",
// "chat/123.1.jsonl", etc.
func gzipArchiveFiles(basePath string) {
	ext := filepath.Ext(basePath)
	stem := strings.TrimSuffix(basePath, ext)
	dir := filepath.Dir(basePath)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		full := filepath.Join(dir, name)
		// Match archive files like "123.2026-03-04T02-30-00Z.jsonl", "123.1.jsonl" etc.
		if !strings.HasSuffix(name, ext) || full == basePath {
			continue
		}
		nameNoExt := strings.TrimSuffix(name, ext)
		stemBase := filepath.Base(stem)
		if strings.HasPrefix(nameNoExt, stemBase+".") && isArchiveFile(name) {
			if err := gzipFile(full); err != nil {
				log.Warnf("session", "archive sweep: gzip archive %s: %v", name, err)
			}
		}
	}
}

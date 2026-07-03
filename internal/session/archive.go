package session

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"foci/internal/log"
	"foci/internal/provider"
	"foci/internal/timeutil"
)

// decompressIfGzipped checks if a .jsonl.gz version of the file exists.
// If found, it decompresses the gzip to the original .jsonl path and
// removes the .gz file. This transparently restores archived sessions.
func (s *Store) decompressIfGzipped(jsonlPath string) error {
	gzPath := jsonlPath + ".gz"
	gf, err := os.Open(gzPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open gzipped session %s: %w", gzPath, err)
	}
	defer func() { _ = gf.Close() }()

	gr, err := gzip.NewReader(gf)
	if err != nil {
		return fmt.Errorf("gzip reader %s: %w", gzPath, err)
	}
	defer func() { _ = gr.Close() }()

	if err := os.MkdirAll(filepath.Dir(jsonlPath), 0755); err != nil {
		return fmt.Errorf("create dir for decompressed session: %w", err)
	}

	out, err := s.createFile(jsonlPath)
	if err != nil {
		return fmt.Errorf("create decompressed session %s: %w", jsonlPath, err)
	}
	// #nosec G110 - legitimate session file decompression, not untrusted input
	if _, err := io.Copy(out, gr); err != nil {
		_ = out.Close()
		_ = os.Remove(jsonlPath)
		return fmt.Errorf("decompress session %s: %w", gzPath, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close decompressed session: %w", err)
	}

	_ = os.Remove(gzPath)
	log.Infof("session", "decompressed archived session %s", filepath.Base(jsonlPath))
	return nil
}

// latestArchivePath returns the path of the newest timestamped/numbered archive
// sibling of rootPath (e.g. for ".../root.jsonl" it finds ".../root.<ts>.jsonl"
// or ".../root.<ts>.jsonl.gz"), or "" if none exist. Archive names sort
// chronologically, so the lexical max is the most recent rotation.
func latestArchivePath(rootPath string) string {
	dir := filepath.Dir(rootPath)
	ext := filepath.Ext(rootPath) // ".jsonl"
	stemBase := strings.TrimSuffix(filepath.Base(rootPath), ext)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best := ""
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		plain := strings.TrimSuffix(name, ".gz") // archives may be gzipped
		if !strings.HasPrefix(plain, stemBase+".") || !isArchiveFile(plain) {
			continue
		}
		if name > best {
			best = name
		}
	}
	if best == "" {
		return ""
	}
	return filepath.Join(dir, best)
}

// loadArchiveFile reads messages from an archive file, transparently
// decompressing a .gz archive.
func (s *Store) loadArchiveFile(path, key string) ([]provider.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gr, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gzip reader %s: %w", path, err)
		}
		defer func() { _ = gr.Close() }()
		r = gr
	}
	return parseMessages(r, key)
}

// loadParentArchiveUnlocked recovers a branch parent's pre-rotation messages
// from its newest archive when the live root.jsonl has been rotated away
// (compaction). Returns false if no usable archive is found. Caller must hold
// s.mu. (P2-5.)
func (s *Store) loadParentArchiveUnlocked(key string) ([]provider.Message, bool) {
	path, err := s.SessionPath(key)
	if err != nil {
		return nil, false
	}
	archivePath := latestArchivePath(path)
	if archivePath == "" {
		return nil, false
	}
	msgs, err := s.loadArchiveFile(archivePath, key)
	if err != nil {
		log.Warnf("session", "branch parent %s: archive %s load failed: %v", key, filepath.Base(archivePath), err)
		return nil, false
	}
	log.Infof("session", "branch parent %s rotated away — recovered %d-message prefix from archive %s", key, len(msgs), filepath.Base(archivePath))
	return msgs, true
}

// nextArchivePath returns the next available archive path for a session file.
// E.g. for "5970082313.jsonl" it returns "5970082313.2026-03-04T02-30-00Z.jsonl".
// If that timestamp already exists, it adds a counter: "5970082313.2026-03-04T02-30-00Z.2.jsonl".
func nextArchivePath(basePath string) string {
	ext := filepath.Ext(basePath)
	stem := strings.TrimSuffix(basePath, ext)
	timestamp := timeutil.FormatFilename(timeutil.Now())

	// First try the basic timestamp pattern
	candidate := fmt.Sprintf("%s.%s%s", stem, timestamp, ext)
	if _, err := os.Stat(candidate); os.IsNotExist(err) {
		return candidate
	}

	// If timestamp already exists, add a counter
	for n := 2; ; n++ {
		candidate = fmt.Sprintf("%s.%s.%d%s", stem, timestamp, n, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

// isArchiveFile returns true if a filename is an archive (e.g. "5970082313.2026-03-04T02-30-00Z.jsonl", "5970082313.2026-03-04T02-30-00Z.2.jsonl", or "5970082313.1.jsonl").
func isArchiveFile(name string) bool {
	if !strings.HasSuffix(name, ".jsonl") {
		return false
	}
	base := strings.TrimSuffix(name, ".jsonl")
	if !strings.Contains(base, ".") {
		return false
	}

	// Split on dots to examine the suffix parts
	parts := strings.Split(base, ".")
	if len(parts) < 2 {
		return false
	}

	// For old numbered pattern: just digits after last dot
	lastPart := parts[len(parts)-1]
	if matched, _ := regexp.MatchString(`^\d+$`, lastPart); matched {
		return true
	}

	// For timestamp pattern: look for YYYY-MM-DDTHH-MM-SSZ or YYYY-MM-DDTHH-MM-SS+HHMM
	timestampPattern := `^\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}(Z|[+-]\d{4})$`

	// Check if last part is a timestamp
	if matched, _ := regexp.MatchString(timestampPattern, lastPart); matched {
		return true
	}

	// Check if second-to-last part is a timestamp (for counter suffix cases like "file.2026-03-04T02-30-00Z.2.jsonl")
	if len(parts) >= 3 {
		secondToLastPart := parts[len(parts)-2]
		if matched, _ := regexp.MatchString(timestampPattern, secondToLastPart); matched {
			// And last part should be a number
			if matched, _ := regexp.MatchString(`^\d+$`, lastPart); matched {
				return true
			}
		}
	}

	return false
}

// Reset archives the session's live file in place (root.jsonl →
// root.<ts>.jsonl), leaving the session key unchanged. The next Append
// recreates the file lazily. Used by /reset: the conversation history is
// archived but the session identity — and everything holding it — is stable.
func (s *Store) Reset(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.SessionPath(key)
	if err != nil {
		return err
	}

	var archivePath string
	if _, err := os.Stat(path); err == nil {
		archivePath = nextArchivePath(path)
		if err := os.Rename(path, archivePath); err != nil {
			return fmt.Errorf("reset session file: %w", err)
		}
		log.Infof("session", "session reset key=%s archive=%s", key, filepath.Base(archivePath))
	}

	s.fireEvent(SessionEvent{
		Key:         key,
		Type:        ClassifySessionKey(key),
		Status:      SessionStatusReset,
		ArchivePath: archivePath,
	})
	return nil
}

// replaceInternal overwrites a session with the given messages, archiving the
// old file in place (e.g. root.2026-03-04T02-30-00Z.jsonl) for audit/history.
// This is internal and must be called through SessionWriter only.
func (s *Store) replaceInternal(key string, msgs []provider.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.SessionPath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}

	// Read metadata before rotating the file
	branchMeta, _ := s.readBranchMeta(key)
	createdAt := s.getStoredCreatedAt(key)

	// Rotate existing file to numbered archive
	var archivePath string
	if _, err := os.Stat(path); err == nil {
		archivePath = nextArchivePath(path)
		if err := os.Rename(path, archivePath); err != nil {
			return fmt.Errorf("rotate session file: %w", err)
		}
		log.Infof("session", "session rotated key=%s archive=%s", key, filepath.Base(archivePath))
	}

	f, err := s.createFile(path)
	if err != nil {
		return fmt.Errorf("create session file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if branchMeta != nil {
		// Branch session: preserve branch_meta with branch_point=0.
		// Compacted messages are self-contained (summary includes parent context).
		branchMeta.BranchPoint = 0
		metaData, err := json.Marshal(branchMeta)
		if err != nil {
			return fmt.Errorf("marshal branch meta: %w", err)
		}
		if _, err := f.Write(append(metaData, '\n')); err != nil {
			return fmt.Errorf("write branch meta: %w", err)
		}
	} else if createdAt != "" {
		// Regular session: write session_meta to preserve creation time
		meta := SessionMeta{
			Type:      "session_meta",
			CreatedAt: createdAt,
		}
		metaData, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("marshal session meta: %w", err)
		}
		if _, err := f.Write(append(metaData, '\n')); err != nil {
			return fmt.Errorf("write session meta: %w", err)
		}
	}

	for _, msg := range msgs {
		data, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshal message: %w", err)
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("write message: %w", err)
		}
	}
	log.Infof("session", "session replaced key=%s messages=%d", key, len(msgs))
	s.fireEvent(SessionEvent{
		Key:         key,
		Type:        ClassifySessionKey(key),
		Status:      SessionStatusCompacted,
		FilePath:    path,
		ArchivePath: archivePath,
	})
	return nil
}

// ArchiveSweep gzips idle session files older than maxAge.
// It queries the index for active sessions whose last activity is older than
// maxAge, skips sessions with active branches, compresses the JSONL files to
// .jsonl.gz, removes the originals, and updates the index status to archived.
// Each agent's current chat session (per chat_metadata) is never archived.
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

	// Get the set of current session keys (the active session per agent+chat).
	// These must never be archived.
	currentKeys, err := index.CurrentSessionKeys()
	if err != nil {
		log.Warnf("session", "archive sweep: query current session keys: %v (skipping protection)", err)
		currentKeys = nil
	}

	cutoff := time.Now().Add(-maxAge)
	archived := 0

	for _, entry := range candidates {
		// Never archive an agent's current chat session.
		if currentKeys[entry.SessionKey] {
			continue
		}

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
// The .gz file is created with the same permission bits as src so that
// archived sessions inherit whatever mode the live file was using.
func gzipFile(src string) error {
	in, err := os.Open(src)
	if os.IsNotExist(err) {
		return nil // already gone
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	dst := src + ".gz"
	out, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
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

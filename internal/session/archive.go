package session

import (
	"bytes"
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

var (
	sessLog = log.NewComponentLogger("session")
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
	sessLog.Infof("decompressed archived session %s", filepath.Base(jsonlPath))
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
		sessionLog(key).Warnf("branch parent %s: archive %s load failed: %v", key, filepath.Base(archivePath), err)
		return nil, false
	}
	sessionLog(key).Infof("branch parent %s rotated away — recovered %d-message prefix from archive %s", key, len(msgs), filepath.Base(archivePath))
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
		// Archive a readable file: a torn final record would make the
		// archive unreadable too (#3065 F13).
		if err := s.repairTornTailUnlocked(key, path); err != nil {
			return err
		}
		archivePath = nextArchivePath(path)
		if err := os.Rename(path, archivePath); err != nil {
			return fmt.Errorf("reset session file: %w", err)
		}
		sessionLog(key).Infof("session reset key=%s archive=%s", key, filepath.Base(archivePath))
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
//
// The replacement is published atomically (#3066 F12): every record is
// marshalled before anything on disk changes, the new file is written and
// synced under a temporary name, the old file is archived by hard link (so the
// live path is never missing), and only then does a rename swap the new file
// in. Any failure before that rename leaves the original history live and
// untouched; a crash at any point leaves either the old or the new file live,
// never a partial one.
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

	// Archive a readable file: a torn final record would make the archive
	// unreadable too (#3065 F13).
	if err := s.repairTornTailUnlocked(key, path); err != nil {
		return err
	}

	// Read metadata from the file being replaced
	branchMeta, _ := s.readBranchMeta(key)
	createdAt := s.getStoredCreatedAt(key)

	// Marshal everything first: a marshal failure must change nothing.
	var buf bytes.Buffer
	if branchMeta != nil {
		// Branch session: preserve branch_meta with branch_point=0.
		// Compacted messages are self-contained (summary includes parent context).
		branchMeta.BranchPoint = 0
		metaData, err := json.Marshal(branchMeta)
		if err != nil {
			return fmt.Errorf("marshal branch meta: %w", err)
		}
		buf.Write(metaData)
		buf.WriteByte('\n')
	} else if createdAt != "" {
		// Regular session: write session_meta to preserve creation time
		metaData, err := json.Marshal(SessionMeta{
			Type:      "session_meta",
			CreatedAt: createdAt,
		})
		if err != nil {
			return fmt.Errorf("marshal session meta: %w", err)
		}
		buf.Write(metaData)
		buf.WriteByte('\n')
	}
	for _, msg := range msgs {
		data, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshal message: %w", err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}

	// Write the replacement under a temporary name that no session walker
	// reads (it does not end in .jsonl).
	tmp := path + ".replace-tmp"
	if err := s.writeSynced(tmp, buf.Bytes()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write replacement session file: %w", err)
	}

	// Archive the old file without taking it off the live path.
	var archivePath string
	if _, err := os.Stat(path); err == nil {
		archivePath = nextArchivePath(path)
		if err := s.archiveCopy(path, archivePath); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("archive session file: %w", err)
		}
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		if archivePath != "" {
			_ = os.Remove(archivePath) // the original is still live
		}
		return fmt.Errorf("publish replacement session file: %w", err)
	}
	syncDir(filepath.Dir(path))
	if archivePath != "" {
		sessionLog(key).Infof("session rotated key=%s archive=%s", key, filepath.Base(archivePath))
	}

	sessionLog(key).Infof("session replaced key=%s messages=%d", key, len(msgs))
	s.fireEvent(SessionEvent{
		Key:         key,
		Type:        ClassifySessionKey(key),
		Status:      SessionStatusCompacted,
		FilePath:    path,
		ArchivePath: archivePath,
	})
	return nil
}

// writeSynced creates path (truncating any leftover) with the store's file
// mode, writes data and syncs it to disk.
func (s *Store) writeSynced(path string, data []byte) error {
	f, err := s.createFile(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// archiveCopy makes dst a second name for src (a hard link), or a synced
// byte copy where the filesystem has no hard links. src stays in place.
func (s *Store) archiveCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := s.writeSynced(dst, data); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

// syncDir flushes a directory entry change (rename, create) to disk, best
// effort: power cuts are frequent on the host, and a rename that is not yet
// on disk can come back as the old name.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
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
		sessLog.Warnf("archive sweep: query current session keys: %v (skipping protection)", err)
		currentKeys = nil
	}

	cutoff := time.Now().Add(-maxAge)
	archived := 0
	var unarchivable []string

	for _, entry := range candidates {
		// Never archive an agent's current chat session.
		if currentKeys[entry.SessionKey] {
			continue
		}

		// An index row whose file_path names something that isn't a regular
		// file (a directory, a socket) can never be gzipped. Handing it to
		// gzipFile fails, and the failure path `continue`s without calling
		// UpdateStatus — so the row stays `active`, stays a candidate, and
		// re-warns on every sweep in perpetuity. Skip it here and report it
		// once per sweep as what it actually is: an index-integrity problem,
		// not an archiving error. An empty path is deliberately NOT treated
		// this way — Stat fails, so it falls through to the existing
		// gzipFile-swallows-ENOENT behaviour, unchanged (#1555).
		if fi, err := os.Stat(entry.FilePath); err == nil && !fi.Mode().IsRegular() {
			unarchivable = append(unarchivable, entry.SessionKey)
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

		// Skip sessions that have active branches referencing them. One
		// indexed existence query per eligible candidate, asked at the same
		// point the old full-index scan was, so it sees children archived
		// earlier in this sweep and branches created since it began (#3079).
		hasActiveBranch, err := index.HasActiveChild(entry.SessionKey)
		if err != nil {
			sessLog.Warnf("archive sweep: query branches for %s: %v", entry.SessionKey, err)
			continue
		}
		if hasActiveBranch {
			continue
		}

		// Compress and remove under the store lock that Append, Load and
		// Replace hold, and only if the file is still idle now: the
		// candidate list is a snapshot, and a session that resumed since
		// would otherwise have its new messages written into a file that is
		// then unlinked (#3066 F11).
		archivedNow, err := archiveIfStillIdle(store, entry.FilePath, cutoff)
		if err != nil {
			sessLog.Warnf("archive sweep: gzip %s: %v", entry.FilePath, err)
			continue
		}
		if !archivedNow {
			sessLog.Infof("archive sweep: %s was written since the sweep began; left active", entry.SessionKey)
			continue
		}

		// Update index status
		index.UpdateStatus(entry.SessionKey, SessionStatusArchived)
		archived++
		sessLog.Infof("archived session %s (last active %s)", entry.SessionKey, lastActivity.Format(time.RFC3339))
	}

	// Reported at Info, not Warn: the sweep cannot act on these and repeating a
	// WARN every 6h for a condition nobody can fix from here is what eroded the
	// zero-warning baseline in the first place (#1555). One line per sweep,
	// naming a key, is enough to find the row.
	if len(unarchivable) > 0 {
		sessLog.Infof("archive sweep: %d index entries point at a non-regular file and can never be archived (e.g. %s) — needs index repair",
			len(unarchivable), unarchivable[0])
	}

	return archived, nil
}

// archiveIfStillIdle gzips a session file and its archive siblings while
// holding store.mu, after re-checking that the file has not been modified
// since cutoff. It reports false (and changes nothing) when the file was
// written after cutoff. A file that no longer exists counts as archived, as
// gzipFile has always treated it.
func archiveIfStillIdle(store *Store, path string, cutoff time.Time) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	if fi, err := os.Stat(path); err == nil && fi.ModTime().After(cutoff) {
		return false, nil
	}
	if err := gzipFile(path); err != nil {
		return false, err
	}
	// Also gzip any numbered archive files in the same directory
	gzipArchiveFiles(path)
	return true, nil
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
	// The original is removed next: the compressed copy must be on disk
	// first, or a power cut can leave neither.
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("sync %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("close %s: %w", dst, err)
	}

	if err := os.Remove(src); err != nil {
		return err
	}
	syncDir(filepath.Dir(src))
	return nil
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
				sessLog.Warnf("archive sweep: gzip archive %s: %v", name, err)
			}
		}
	}
}

package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ChatSessionInfo holds metadata about a per-chat session.
type ChatSessionInfo struct {
	ChatID       int64
	SessionKey   string
	MessageCount int
	LastActivity time.Time
}

// ListChatSessions returns all chat sessions for an agent.
// It scans for directories matching the pattern <agentID>/c<chatID>/root.jsonl.
func (s *Store) ListChatSessions(agentID string) ([]ChatSessionInfo, error) {
	agentDir := filepath.Join(s.dir, agentID)
	entries, err := os.ReadDir(agentDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read agent dir: %w", err)
	}

	var sessions []ChatSessionInfo
	for _, e := range entries {
		// Look for directories starting with 'c' (chat sessions)
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "c") {
			continue
		}

		// Parse chat ID from directory name (e.g., "c123" -> 123)
		chatID, err := strconv.ParseInt(strings.TrimPrefix(e.Name(), "c"), 10, 64)
		if err != nil {
			continue
		}

		key := NewChatSessionKey(agentID, chatID)
		rootPath := filepath.Join(agentDir, e.Name(), "root.jsonl")
		if _, err := os.Stat(rootPath); os.IsNotExist(err) {
			continue
		}

		mc, _ := s.MessageCount(key)

		info, err := os.Stat(rootPath)
		var lastActivity time.Time
		if err == nil {
			lastActivity = info.ModTime()
		}

		sessions = append(sessions, ChatSessionInfo{
			ChatID:       chatID,
			SessionKey:   key,
			MessageCount: mc,
			LastActivity: lastActivity,
		})
	}

	return sessions, nil
}

// ScanAllSessions walks the session directory and returns an index entry for
// every session file: a current file (.jsonl) is active, a compaction/reset
// archive (.<ts>.jsonl) is compacted, and any gzipped file (.jsonl.gz) is
// archived. Each entry carries the identity foci wrote when it first created
// the row (scanEntry), so a rebuild restores it rather than the file's mtime.
// The per-file first-line reads (the I/O-bound part) run in parallel.
func (s *Store) ScanAllSessions() ([]SessionIndexEntry, error) {
	// Phase 1: Walk the directory tree collecting file info.
	// No store lock needed — this is read-only and runs at startup before
	// any message processing starts.
	type fileInfo struct {
		path    string
		modTime time.Time
	}
	var files []fileInfo

	err := filepath.WalkDir(s.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".jsonl.gz") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, fileInfo{path: path, modTime: info.ModTime()})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	// Phase 2: derive each file's entry in parallel.
	results := make([]SessionIndexEntry, len(files))
	ok := make([]bool, len(files))
	ch := make(chan int, len(files))
	for i := range files {
		ch <- i
	}
	close(ch)

	const workers = 8
	var wg sync.WaitGroup
	for w := 0; w < workers && w < len(files); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				results[i], ok[i] = s.scanEntry(files[i].path, files[i].modTime)
			}
		}()
	}
	wg.Wait()

	entries := make([]SessionIndexEntry, 0, len(files))
	for i := range results {
		if ok[i] {
			entries = append(entries, results[i])
		}
	}
	return entries, nil
}

// scanEntry derives one file's index entry. created_at, session_type and
// parent_session_key are what foci writes when it first creates the row
// (#3184), read from the file's name and first line:
//
//   - a compaction/reset archive: created_at is the time in its suffix (the
//     gateway's archive row is stamped then), the parent is the live session
//     it was rotated from, the type is that session's key class;
//   - a session file: created_at is its session_meta line's, else the time
//     its key encodes (a branch's b<ts>; branch files have no session_meta);
//     a branch_meta line gives the parent and the branch type.
//
// The mtime is the fallback created_at only when neither holds a time. A
// .gz file is read the same way: gzipping changes the mtime, not the content.
// last_activity_at is the mtime (the last append, for a session file); an
// archive's is left to fall back to its created_at, as the gateway's is.
func (s *Store) scanEntry(path string, modTime time.Time) (SessionIndexEntry, bool) {
	rel, err := filepath.Rel(s.dir, path)
	if err != nil {
		return SessionIndexEntry{}, false
	}
	gz := strings.HasSuffix(rel, ".gz")
	plain := strings.TrimSuffix(rel, ".gz")
	key := pathToKey(strings.TrimSuffix(plain, ".jsonl"))

	if isArchiveFile(filepath.Base(plain)) {
		parentKey := archiveParentKey(key)
		createdAt := modTime
		if t, ok := archiveSuffixTime(key); ok {
			createdAt = t
		}
		status := SessionStatusCompacted
		if gz {
			status = SessionStatusArchived
		}
		return SessionIndexEntry{
			SessionKey:       key,
			FilePath:         path,
			CreatedAt:        createdAt,
			ParentSessionKey: parentKey,
			SessionType:      ClassifySessionKey(parentKey),
			Status:           status,
		}, true
	}

	id := readFileIdentity(path)
	createdAt := id.createdAt
	if createdAt.IsZero() {
		if t, ok := keyTimestamp(key); ok {
			createdAt = t
		} else {
			createdAt = modTime
		}
	}
	// Roots classify from the key; branches recover their fine-grained
	// type from the persisted branch_type (the key alone can't distinguish
	// facet/reflection/spawn/…).
	sessionType := ClassifySessionKey(key)
	if id.branch && id.typ != "" {
		sessionType = SessionTypeForBranch(id.typ)
	}
	status := SessionStatusActive
	if gz {
		status = SessionStatusArchived
	}
	return SessionIndexEntry{
		SessionKey: key,
		FilePath:   path,
		CreatedAt:  createdAt,
		// The file's mtime is the last append — the true "last activity"
		// for a file-backed session. Without it, a rebuild seeds activity
		// from created_at and the most-recently-active ordering
		// (default-chat routing!) degrades to creation order.
		LastActivityAt:   modTime,
		ParentSessionKey: id.parent,
		SessionType:      sessionType,
		Status:           status,
	}, true
}

// pathToKey converts a relative file path (extension already stripped) back to
// a session key.
// Example: main/c123/root → main/c123
// Example: main/c123/b1709596800 → main/c123/b1709596800
func pathToKey(relPath string) string {
	// If path ends with /root, strip it (it's a root session)
	if strings.HasSuffix(relPath, "/root") {
		return strings.TrimSuffix(relPath, "/root")
	}
	// Otherwise the path IS the key
	return relPath
}

// archiveParentKey derives the live session key an archive file belongs to.
// The archive suffix (timestamp and/or counter, dot-separated) is stripped from
// the last path segment; a remaining "root" segment maps to the root key.
// Examples:
//
//	"main/c123/root.2026-03-04T02-30-00Z"    → "main/c123"
//	"main/c123/root.2026-03-04T02-30-00Z.2"  → "main/c123"
//	"main/c123/b1709596800.1"                → "main/c123/b1709596800"
func archiveParentKey(archiveKey string) string {
	keyParts := strings.Split(archiveKey, "/")
	lastSegment := keyParts[len(keyParts)-1]

	// The live file's stem is everything before the first dot: archive
	// suffixes are only ever appended (nextArchivePath), and live stems
	// ("root", "b<ts>", "i<ts>") never contain dots.
	stem := lastSegment
	if idx := strings.Index(lastSegment, "."); idx > 0 {
		stem = lastSegment[:idx]
	}

	if stem == "root" {
		// Root archive: the parent key is the directory path.
		return strings.Join(keyParts[:len(keyParts)-1], "/")
	}
	keyParts[len(keyParts)-1] = stem
	return strings.Join(keyParts, "/")
}

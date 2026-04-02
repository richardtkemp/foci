package session

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
// It scans for directories matching the pattern <agentID>/c<chatID>/<versionTS>/root.jsonl.
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
		chatIDStr := strings.TrimPrefix(e.Name(), "c")
		var chatID int64
		if _, err := fmt.Sscanf(chatIDStr, "%d", &chatID); err != nil {
			continue
		}

		// Look for version timestamp directories inside the chat directory
		chatDir := filepath.Join(agentDir, e.Name())
		versionEntries, err := os.ReadDir(chatDir)
		if err != nil {
			continue
		}

		// Find version directories (numeric names)
		for _, ve := range versionEntries {
			if !ve.IsDir() {
				continue
			}

			// Version should be a number (the timestamp)
			versionTS, err := strconv.ParseInt(ve.Name(), 10, 64)
			if err != nil {
				continue
			}

			// Check for root.jsonl in the version directory
			versionDir := filepath.Join(chatDir, ve.Name())
			rootPath := filepath.Join(versionDir, "root.jsonl")
			if _, err := os.Stat(rootPath); os.IsNotExist(err) {
				continue
			}

			// Reconstruct the actual session key from the directory structure
			key := fmt.Sprintf("%s/c%d/%d", agentID, chatID, versionTS)
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
			break // Only take the first version found
		}
	}

	return sessions, nil
}

// ScanAllSessions walks all session files and returns index entries.
// Current (non-archive) files are always marked active.
// Numbered archive files (e.g. .1.jsonl) are returned as compacted entries.
// Gzipped files (.jsonl.gz) are returned as archived entries.
// ScanAllSessions walks the session directory and returns index entries for
// all session files. Uses WalkDir for efficiency and parallelizes per-file
// metadata reads for active sessions (the I/O-bound part).
func (s *Store) ScanAllSessions() ([]SessionIndexEntry, error) {
	// Phase 1: Walk the directory tree collecting file info.
	// No store lock needed — this is read-only and runs at startup before
	// any message processing starts.
	type fileInfo struct {
		path    string
		name    string
		modTime time.Time
	}
	var activeFiles []fileInfo
	var staticEntries []SessionIndexEntry

	err := filepath.WalkDir(s.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		name := d.Name()
		info, err := d.Info()
		if err != nil {
			return nil
		}

		if strings.HasSuffix(name, ".jsonl.gz") {
			rel, err := filepath.Rel(s.dir, path)
			if err != nil {
				return nil
			}
			key := pathToKey(strings.TrimSuffix(rel, ".jsonl.gz"))
			staticEntries = append(staticEntries, SessionIndexEntry{
				SessionKey:  key,
				FilePath:    path,
				CreatedAt:   info.ModTime(),
				SessionType: ClassifySessionKey(key),
				Status:      SessionStatusArchived,
			})
			return nil
		}

		if !strings.HasSuffix(name, ".jsonl") {
			return nil
		}

		if isArchiveFile(name) {
			rel, err := filepath.Rel(s.dir, path)
			if err != nil {
				return nil
			}
			key := pathToKey(strings.TrimSuffix(rel, ".jsonl"))
			parentKey := archiveParentKey(key)
			staticEntries = append(staticEntries, SessionIndexEntry{
				SessionKey:       key,
				FilePath:         path,
				CreatedAt:        info.ModTime(),
				ParentSessionKey: parentKey,
				SessionType:      ClassifySessionKey(parentKey),
				Status:           SessionStatusCompacted,
			})
			return nil
		}

		// Active session file — needs metadata read (phase 2).
		activeFiles = append(activeFiles, fileInfo{
			path:    path,
			name:    name,
			modTime: info.ModTime(),
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	// Phase 2: Read metadata for active files in parallel.
	// Each goroutine reads the first line of a file for created_at/branch_meta.
	results := make([]SessionIndexEntry, len(activeFiles))
	ch := make(chan int, len(activeFiles))
	for i := range activeFiles {
		ch <- i
	}
	close(ch)

	const workers = 8
	var wg sync.WaitGroup
	for w := 0; w < workers && w < len(activeFiles); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				af := activeFiles[i]
				rel, err := filepath.Rel(s.dir, af.path)
				if err != nil {
					continue
				}
				key := pathToKey(strings.TrimSuffix(rel, ".jsonl"))

				createdAt := af.modTime
				if meta := s.getStoredCreatedAt(key); meta != "" {
					if t, err := time.Parse(time.RFC3339, meta); err == nil {
						createdAt = t
					}
				}

				var parentKey string
				if bm, _ := s.readBranchMeta(key); bm != nil {
					parentKey = bm.ParentKey
				}

				results[i] = SessionIndexEntry{
					SessionKey:       key,
					FilePath:         af.path,
					CreatedAt:        createdAt,
					ParentSessionKey: parentKey,
					SessionType:      ClassifySessionKey(key),
					Status:           SessionStatusActive,
				}
			}
		}()
	}
	wg.Wait()

	entries := make([]SessionIndexEntry, 0, len(staticEntries)+len(results))
	entries = append(entries, staticEntries...)
	entries = append(entries, results...)
	return entries, nil
}

// pathToKey converts a relative file path back to a session key.
// New format: path is the key, except root sessions have /root suffix.
// Example: main/c123/1709590000/root → main/c123/1709590000
// Example: main/c123/1709590000/b1709596800 → main/c123/1709590000/b1709596800
func pathToKey(relPath string) string {
	// If path ends with /root, strip it (it's a root session)
	if strings.HasSuffix(relPath, "/root") {
		return strings.TrimSuffix(relPath, "/root")
	}
	// Otherwise the path IS the key
	return relPath
}

// archiveParentKey derives the parent session key from an archive key.
// New format examples:
// "main/c123/1709590000.2026-03-04T02-30-00Z" → "main/c123/1709590000"
// "main/c123/1709590000.2026-03-04T02-30-00Z.2" → "main/c123/1709590000"
// "main/c123/1709590000.1" → "main/c123/1709590000" (numbered suffix)
func archiveParentKey(archiveKey string) string {
	// Find the last segment
	keyParts := strings.Split(archiveKey, "/")
	if len(keyParts) == 0 {
		return archiveKey
	}

	lastSegment := keyParts[len(keyParts)-1]
	segmentParts := strings.Split(lastSegment, ".")

	// Need at least 2 parts to have an archive suffix
	if len(segmentParts) < 2 {
		return archiveKey
	}

	// Find the base name by removing archive suffixes
	var baseParts []string

	// Compile regexes once before loop
	timestampRe := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}Z$`)
	digitsRe := regexp.MustCompile(`^\d+$`)

	// Identify where the archive suffix starts
	for i := 0; i < len(segmentParts); i++ {
		part := segmentParts[i]

		// Check if this part is a timestamp pattern
		if timestampRe.MatchString(part) {
			// Found timestamp, everything before this is the base
			baseParts = segmentParts[:i]
			break
		}

		// Check if this part is just digits (numbered pattern)
		if digitsRe.MatchString(part) {
			// Found numbered suffix, everything before this is the base
			baseParts = segmentParts[:i]
			break
		}
	}

	// If no archive pattern found, return original
	if len(baseParts) == 0 {
		return archiveKey
	}

	// Rebuild the key with cleaned last segment
	cleanedLastSegment := strings.Join(baseParts, ".")
	keyParts[len(keyParts)-1] = cleanedLastSegment
	return strings.Join(keyParts, "/")
}

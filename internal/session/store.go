package session

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"foci/internal/log"
	"foci/prompts"
	"foci/internal/provider"
)

// SessionMeta is stored as the first line in a session file to preserve metadata
// like the original creation time (important for compaction continuity).
type SessionMeta struct {
	Type      string `json:"type"`       // "session_meta"
	CreatedAt string `json:"created_at"` // RFC3339 timestamp
}

// Store is a JSONL-backed session store.
type Store struct {
	dir     string
	mu      sync.Mutex
	onEvent func(SessionEvent)
}

// NewStore creates a session store rooted at dir.
func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// SessionWriter restricts all session write operations to a single session,
// enforcing the ownership model: a session file can only be modified by its own
// HandleMessage execution. Any attempt to write to a different session is rejected
// with a clear error. This is the only way to safely write to sessions.
//
// Example usage in tools:
//
//	func (t *Tool) Execute(ctx context.Context, params json.RawMessage) (ToolResult, error) {
//	    currentSession := SessionKeyFromContext(ctx)
//	    writer := t.sessions.For(currentSession)
//	    // This works - writing to own session
//	    writer.Append(currentSession, msg)
//	    writer.AppendAll(currentSession, msgs)
//	    writer.Replace(currentSession, msgs)
//	    // All of these fail - cross-session write blocked
//	    writer.Append(otherSession, msg)  // ERROR
//	}
type SessionWriter struct {
	store      *Store
	sessionKey string
}

// For creates a SessionWriter that can only modify the specified session.
// This enforces session ownership: all write operations are restricted to this session.
func (s *Store) For(sessionKey string) *SessionWriter {
	return &SessionWriter{
		store:      s,
		sessionKey: sessionKey,
	}
}

// Append adds a message to the owned session, rejecting cross-session writes.
func (w *SessionWriter) Append(key string, msg provider.Message) error {
	if key != w.sessionKey {
		return fmt.Errorf("cross-session write blocked: SessionWriter for session %q cannot write to session %q",
			w.sessionKey, key)
	}
	return w.store.Append(key, msg)
}

// AppendAll adds multiple messages to the owned session, rejecting cross-session writes.
func (w *SessionWriter) AppendAll(key string, msgs []provider.Message) error {
	if key != w.sessionKey {
		return fmt.Errorf("cross-session write blocked: SessionWriter for session %q cannot write to session %q",
			w.sessionKey, key)
	}
	return w.store.AppendAll(key, msgs)
}

// Replace overwrites the owned session with new messages, rejecting cross-session writes.
func (w *SessionWriter) Replace(key string, msgs []provider.Message) error {
	if key != w.sessionKey {
		return fmt.Errorf("cross-session write blocked: SessionWriter for session %q cannot write to session %q",
			w.sessionKey, key)
	}
	return w.store.Replace(key, msgs)
}

// Clear deletes the owned session, rejecting cross-session writes.
func (w *SessionWriter) Clear(key string) error {
	if key != w.sessionKey {
		return fmt.Errorf("cross-session write blocked: SessionWriter for session %q cannot write to session %q",
			w.sessionKey, key)
	}
	return w.store.Clear(key)
}

// SessionPath converts a session key to a file path.
// Key format: {agentID}/{type}{id}/{versionTS}[/{childType}{childTS}][.{n}]
// Root path: {dir}/{key}/root.jsonl
// Child path: {dir}/{key}.jsonl
func (s *Store) SessionPath(key string) (string, error) {
	// Split on '/' and check last segment
	// If last segment is a pure number (version timestamp), it's a root
	parts := strings.Split(key, "/")
	if len(parts) < 3 {
		return "", fmt.Errorf("invalid session key %q: need at least agentID/typeID/versionTS", key)
	}

	lastSegment := parts[len(parts)-1]

	// Check for collision suffix
	if idx := strings.Index(lastSegment, "."); idx > 0 {
		lastSegment = lastSegment[:idx]
	}

	// If last segment is pure number, it's a root session
	if _, err := strconv.ParseInt(lastSegment, 10, 64); err == nil {
		return filepath.Join(s.dir, key, "root.jsonl"), nil
	}

	// Otherwise it's a child session
	return filepath.Join(s.dir, key+".jsonl"), nil
}

// Load reads all messages from a session file.
// Returns nil (not error) if file doesn't exist.
func (s *Store) Load(key string) ([]provider.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.loadUnlocked(key)
}

func (s *Store) loadUnlocked(key string) ([]provider.Message, error) {
	path, err := s.SessionPath(key)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if os.IsNotExist(err) {
		// Check for gzipped archive and decompress if found
		if err := s.decompressIfGzipped(path); err != nil {
			return nil, err
		}
		f, err = os.Open(path)
		if os.IsNotExist(err) {
			return nil, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("open session %s: %w", key, err)
	}
	defer func() { _ = f.Close() }()

	var messages []provider.Message
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Skip branch metadata lines
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &probe) == nil && (probe.Type == "branch_meta" || probe.Type == "session_meta") {
			continue
		}

		var msg provider.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("decode message in %s: %w", key, err)
		}
		messages = append(messages, msg)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan session %s: %w", key, err)
	}

	log.Debugf("session", "session loaded key=%s messages=%d", key, len(messages))
	return messages, nil
}

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

	out, err := os.Create(jsonlPath)
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

// Append adds a message to the session file, creating it if needed.
func (s *Store) Append(key string, msg provider.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.appendUnlocked(key, msg)
}

func (s *Store) appendUnlocked(key string, msg provider.Message) error {
	path, err := s.SessionPath(key)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}

	// Check if file exists - if not, write session metadata first
	exists := false
	if _, err := os.Stat(path); err == nil {
		exists = true
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644) // #nosec G302 - session file, needs group write access
	if err != nil {
		return fmt.Errorf("open session file: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Write session metadata on new files
	if !exists {
		now := time.Now().UTC()
		log.Infof("session", "session created key=%s", key)
		meta := SessionMeta{
			Type:      "session_meta",
			CreatedAt: now.Format(time.RFC3339),
		}
		metaData, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("marshal session meta: %w", err)
		}
		if _, err := f.Write(append(metaData, '\n')); err != nil {
			return fmt.Errorf("write session meta: %w", err)
		}
		s.fireEvent(SessionEvent{
			Key:       key,
			Type:      ClassifySessionKey(key),
			Status:    SessionStatusActive,
			FilePath:  path,
			CreatedAt: now,
		})
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write message: %w", err)
	}

	return nil
}

// AppendAll adds multiple messages to the session file.
func (s *Store) AppendAll(key string, msgs []provider.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, msg := range msgs {
		if err := s.appendUnlocked(key, msg); err != nil {
			return err
		}
	}
	return nil
}

// Clear removes a session file.
func (s *Store) Clear(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.SessionPath(key)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err == nil {
		log.Infof("session", "session cleared key=%s", key)
		s.fireEvent(SessionEvent{
			Key:    key,
			Status: SessionStatusCleared,
		})
	}
	return err
}

// nextArchivePath returns the next available archive path for a session file.
// E.g. for "5970082313.jsonl" it returns "5970082313.2026-03-04T02-30-00Z.jsonl".
// If that timestamp already exists, it adds a counter: "5970082313.2026-03-04T02-30-00Z.2.jsonl".
func nextArchivePath(basePath string) string {
	ext := filepath.Ext(basePath)
	stem := strings.TrimSuffix(basePath, ext)
	timestamp := time.Now().UTC().Format("2006-01-02T15-04-05Z")

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

	// For timestamp pattern: look for YYYY-MM-DDTHH-MM-SSZ
	timestampPattern := `^\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}Z$`

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

// Replace overwrites a session with the given messages, rotating the old file
// to a numbered archive (e.g. 5970082313.1.jsonl) for audit/history.
func (s *Store) Replace(key string, msgs []provider.Message) error {
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

	f, err := os.Create(path)
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

// getStoredCreatedAt reads the stored creation time from an existing session file.
func (s *Store) getStoredCreatedAt(key string) string {
	path, err := s.SessionPath(key)
	if err != nil {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var meta SessionMeta
		if err := json.Unmarshal(line, &meta); err == nil && meta.Type == "session_meta" && meta.CreatedAt != "" {
			return meta.CreatedAt
		}
		break
	}
	return ""
}

// RepairOrphans scans all session files and repairs any that end with an
// assistant message containing tool_use blocks without a following tool_result.
// This happens when the process is killed mid-tool-call: the defer flush writes
// the assistant message but no tool_result is ever created, leaving the session
// structurally invalid for the Anthropic API.
// Returns the number of repaired sessions and any error.
func (s *Store) RepairOrphans() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	repaired := 0

	err := filepath.Walk(s.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if isArchiveFile(filepath.Base(path)) {
			return nil
		}

		// Convert file path back to session key
		rel, err := filepath.Rel(s.dir, path)
		if err != nil {
			return nil
		}
		rel = strings.TrimSuffix(rel, ".jsonl")
		key := pathToKey(rel)

		msgs, err := s.loadUnlocked(key)
		if err != nil || len(msgs) == 0 {
			return nil
		}

		last := msgs[len(msgs)-1]
		if last.Role != "assistant" {
			return nil
		}

		var toolUseIDs []string
		for _, block := range last.Content {
			if block.Type == "tool_use" {
				toolUseIDs = append(toolUseIDs, block.ID)
			}
		}
		if len(toolUseIDs) == 0 {
			return nil
		}

		// Build synthetic tool_result message
		var results []provider.ContentBlock
		for _, id := range toolUseIDs {
			results = append(results, provider.ToolResultBlock(id, "Tool call interrupted by service restart", true))
		}
		repairMsg := provider.Message{Role: "user", Content: results}

		if err := s.appendUnlocked(key, repairMsg); err != nil {
			return fmt.Errorf("repair %s: %w", key, err)
		}
		repaired++
		return nil
	})

	if err != nil && !os.IsNotExist(err) {
		return repaired, err
	}
	return repaired, nil
}

// RestartMarkerMaxAge is the maximum age of a session file to receive a restart marker.
// Only sessions modified within this window are considered "active" at restart time.
const RestartMarkerMaxAge = 1 * time.Hour

// InjectRestartMarkers appends a restart marker to all active session files
// (those modified within maxAge of now). This gives the agent visibility that
// a service restart occurred. Returns the number of marked sessions.
func (s *Store) InjectRestartMarkers(maxAge time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	marked := 0

	err := filepath.Walk(s.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if isArchiveFile(filepath.Base(path)) {
			return nil
		}

		// Only mark recently active sessions
		if now.Sub(info.ModTime()) > maxAge {
			return nil
		}

		// Convert file path back to session key
		rel, err := filepath.Rel(s.dir, path)
		if err != nil {
			return nil
		}
		rel = strings.TrimSuffix(rel, ".jsonl")
		key := pathToKey(rel)

		marker := provider.Message{
			Role:    "user",
			Content: provider.TextContent(prompts.FormatInjectedMessage("SYSTEM RESTART", now, "")),
		}
		if err := s.appendUnlocked(key, marker); err != nil {
			return fmt.Errorf("mark %s: %w", key, err)
		}
		marked++
		return nil
	})

	if err != nil && !os.IsNotExist(err) {
		return marked, err
	}
	return marked, nil
}

// MessageCount returns the number of messages in a session.
func (s *Store) MessageCount(key string) (int, error) {
	msgs, err := s.Load(key)
	if err != nil {
		return 0, err
	}
	return len(msgs), nil
}

// CreatedAt returns the creation time of a session file as an RFC3339 string.
// Returns "n/a" if the file doesn't exist.
func (s *Store) CreatedAt(key string) string {
	// First try to read stored creation time from file
	path, err := s.SessionPath(key)
	if err != nil {
		return "n/a"
	}
	f, err := os.Open(path)
	if err != nil {
		return "n/a"
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Check if first line is session metadata
		var meta SessionMeta
		if err := json.Unmarshal(line, &meta); err == nil && meta.Type == "session_meta" && meta.CreatedAt != "" {
			return meta.CreatedAt
		}
		break // Only check first line
	}
	// Fall back to file modification time
	return s.fileTime(key)
}

// LastActivity returns the last modification time of a session file as an RFC3339 string.
// Returns "n/a" if the file doesn't exist.
func (s *Store) LastActivity(key string) string {
	return s.fileTime(key)
}

func (s *Store) fileTime(key string) string {
	path, err := s.SessionPath(key)
	if err != nil {
		return "n/a"
	}
	info, err := os.Stat(path)
	if err != nil {
		return "n/a"
	}
	return info.ModTime().UTC().Format("2006-01-02T15:04:05Z")
}

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

// SessionEvent describes a lifecycle event on a session.
type SessionEvent struct {
	Key         string
	Type        SessionType
	Status      SessionStatus
	ParentKey   string // for branches
	FilePath    string
	CreatedAt   time.Time
	ArchivePath string // set on compaction: path to the rotated archive file
}

// OnSessionEvent is an optional callback fired on session lifecycle events.
// Set this before any concurrent use of the Store.
func (s *Store) OnSessionEvent(fn func(SessionEvent)) {
	s.onEvent = fn
}

func (s *Store) fireEvent(e SessionEvent) {
	if s.onEvent != nil {
		s.onEvent(e)
	}
}

// ClassifySessionKey determines the SessionType from a session key.
// With the new format, chat vs independent is structural, and branch is
// identifiable by child type. Semantic subtypes (spawn, multiball, cron)
// are metadata and cannot be distinguished from the key alone.
func ClassifySessionKey(key string) SessionType {
	k, err := ParseSessionKey(key)
	if err != nil {
		return SessionTypeUnknown
	}
	if k.ChildType == 'b' {
		return SessionTypeBranch
	}
	if k.ChildType != 0 {
		return SessionTypeUnknown // independent spawn — can't distinguish subtypes
	}
	if k.Type == 'c' {
		return SessionTypeChat
	}
	return SessionTypeUnknown
}

// ScanAllSessions walks all session files and returns index entries.
// Current (non-archive) files are always marked active.
// Numbered archive files (e.g. .1.jsonl) are returned as compacted entries.
// Gzipped files (.jsonl.gz) are returned as archived entries.
func (s *Store) ScanAllSessions() ([]SessionIndexEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var entries []SessionIndexEntry
	err := filepath.Walk(s.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}

		name := filepath.Base(path)

		// Handle gzipped (archived) session files
		if strings.HasSuffix(name, ".jsonl.gz") {
			rel, err := filepath.Rel(s.dir, path)
			if err != nil {
				return nil
			}
			rel = strings.TrimSuffix(rel, ".jsonl.gz")
			key := pathToKey(rel)
			stype := ClassifySessionKey(key)
			entries = append(entries, SessionIndexEntry{
				SessionKey:  key,
				FilePath:    path,
				CreatedAt:   info.ModTime(),
				SessionType: stype,
				Status:      SessionStatusArchived,
			})
			return nil
		}

		if !strings.HasSuffix(name, ".jsonl") {
			return nil
		}

		// Handle numbered archive files (e.g. 5970082313.1.jsonl)
		if isArchiveFile(name) {
			rel, err := filepath.Rel(s.dir, path)
			if err != nil {
				return nil
			}
			rel = strings.TrimSuffix(rel, ".jsonl")
			key := pathToKey(rel)
			// Derive parent key: remove the archive suffix
			parentKey := archiveParentKey(key)
			stype := ClassifySessionKey(parentKey)
			entries = append(entries, SessionIndexEntry{
				SessionKey:       key,
				FilePath:         path,
				CreatedAt:        info.ModTime(),
				ParentSessionKey: parentKey,
				SessionType:      stype,
				Status:           SessionStatusCompacted,
			})
			return nil
		}

		// Current (non-archive) session file — always active
		rel, err := filepath.Rel(s.dir, path)
		if err != nil {
			return nil
		}
		rel = strings.TrimSuffix(rel, ".jsonl")
		key := pathToKey(rel)

		stype := ClassifySessionKey(key)

		// Determine created_at from session metadata or file mtime
		var createdAt time.Time
		if meta := s.getStoredCreatedAt(key); meta != "" {
			if t, err := time.Parse(time.RFC3339, meta); err == nil {
				createdAt = t
			}
		}
		if createdAt.IsZero() {
			createdAt = info.ModTime()
		}

		// Determine parent from branch_meta
		var parentKey string
		bm, _ := s.readBranchMeta(key)
		if bm != nil {
			parentKey = bm.ParentKey
		}

		entries = append(entries, SessionIndexEntry{
			SessionKey:       key,
			FilePath:         path,
			CreatedAt:        createdAt,
			ParentSessionKey: parentKey,
			SessionType:      stype,
			Status:           SessionStatusActive,
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return entries, err
	}
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

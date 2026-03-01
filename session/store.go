package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"foci/anthropic"
	"foci/log"
	"foci/prompts"
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

// keyToPath converts a session key to a file path.
// Key format: agent:AGENTID:TYPE[:BRANCHID]
// Path format: {dir}/agent/AGENTID/TYPE[/BRANCHID].jsonl
func (s *Store) keyToPath(key string) (string, error) {
	parts := strings.Split(key, ":")
	// parts[0] = "agent", parts[1] = AGENTID, parts[2] = TYPE, parts[3] = BRANCHID (optional)
	if len(parts) < 3 {
		return "", fmt.Errorf("invalid session key %q: need at least 3 colon-separated parts", key)
	}
	if len(parts) == 4 {
		return filepath.Join(s.dir, parts[0], parts[1], parts[2], parts[3]+".jsonl"), nil
	}
	return filepath.Join(s.dir, parts[0], parts[1], parts[2]+".jsonl"), nil
}

// Load reads all messages from a session file.
// Returns nil (not error) if file doesn't exist.
func (s *Store) Load(key string) ([]anthropic.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.loadUnlocked(key)
}

func (s *Store) loadUnlocked(key string) ([]anthropic.Message, error) {
	path, err := s.keyToPath(key)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open session %s: %w", key, err)
	}
	defer f.Close()

	var messages []anthropic.Message
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

		var msg anthropic.Message
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

// Append adds a message to the session file, creating it if needed.
func (s *Store) Append(key string, msg anthropic.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.appendUnlocked(key, msg)
}

func (s *Store) appendUnlocked(key string, msg anthropic.Message) error {
	path, err := s.keyToPath(key)
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

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open session file: %w", err)
	}
	defer f.Close()

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
func (s *Store) AppendAll(key string, msgs []anthropic.Message) error {
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

	path, err := s.keyToPath(key)
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
// E.g. for "5970082313.jsonl" it returns "5970082313.1.jsonl", or ".2.jsonl" if .1 exists, etc.
func nextArchivePath(basePath string) string {
	ext := filepath.Ext(basePath)
	stem := strings.TrimSuffix(basePath, ext)
	for n := 1; ; n++ {
		candidate := fmt.Sprintf("%s.%d%s", stem, n, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

// isArchiveFile returns true if a filename is a numbered archive (e.g. "5970082313.1.jsonl").
func isArchiveFile(name string) bool {
	base := strings.TrimSuffix(name, ".jsonl")
	return strings.Contains(base, ".")
}

// Replace overwrites a session with the given messages, rotating the old file
// to a numbered archive (e.g. 5970082313.1.jsonl) for audit/history.
func (s *Store) Replace(key string, msgs []anthropic.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.keyToPath(key)
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
	if _, err := os.Stat(path); err == nil {
		archivePath := nextArchivePath(path)
		if err := os.Rename(path, archivePath); err != nil {
			return fmt.Errorf("rotate session file: %w", err)
		}
		log.Infof("session", "session rotated key=%s archive=%s", key, filepath.Base(archivePath))
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create session file: %w", err)
	}
	defer f.Close()

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
		Key:    key,
		Status: SessionStatusCompacted,
	})
	return nil
}

// getStoredCreatedAt reads the stored creation time from an existing session file.
func (s *Store) getStoredCreatedAt(key string) string {
	path, err := s.keyToPath(key)
	if err != nil {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

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
		key := strings.ReplaceAll(rel, string(filepath.Separator), ":")

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
		var results []anthropic.ContentBlock
		for _, id := range toolUseIDs {
			results = append(results, anthropic.ToolResultBlock(id, "Tool call interrupted by service restart", true))
		}
		repairMsg := anthropic.Message{Role: "user", Content: results}

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
		key := strings.ReplaceAll(rel, string(filepath.Separator), ":")

		marker := anthropic.Message{
			Role:    "user",
			Content: anthropic.TextContent(prompts.FormatInjectedMessage("SYSTEM RESTART", now, "")),
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
	path, err := s.keyToPath(key)
	if err != nil {
		return "n/a"
	}
	f, err := os.Open(path)
	if err != nil {
		return "n/a"
	}
	defer f.Close()

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
	path, err := s.keyToPath(key)
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
// It scans for files matching the pattern agent/<agentID>/chat/<chatID>.jsonl.
func (s *Store) ListChatSessions(agentID string) ([]ChatSessionInfo, error) {
	chatDir := filepath.Join(s.dir, "agent", agentID, "chat")
	entries, err := os.ReadDir(chatDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read chat dir: %w", err)
	}

	var sessions []ChatSessionInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if isArchiveFile(e.Name()) {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".jsonl")

		// Parse chat ID from filename
		var chatID int64
		if _, err := fmt.Sscanf(name, "%d", &chatID); err != nil {
			continue
		}

		key := fmt.Sprintf("agent:%s:chat:%d", agentID, chatID)
		mc, _ := s.MessageCount(key)

		info, err := e.Info()
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

// SessionEvent describes a lifecycle event on a session.
type SessionEvent struct {
	Key       string
	Type      SessionType
	Status    SessionStatus
	ParentKey string // for branches
	FilePath  string
	CreatedAt time.Time
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
func ClassifySessionKey(key string) SessionType {
	parts := splitKeyParts(key)
	if len(parts) < 3 {
		return SessionTypeUnknown
	}
	// Check for ":branch:" segment (session-end memory branches like agent:X:multiball:Y:branch:Z)
	for i := 2; i < len(parts)-1; i++ {
		if parts[i] == "branch" {
			return SessionTypeBranch
		}
	}
	switch parts[2] {
	case "chat":
		return SessionTypeChat
	case "multiball":
		return SessionTypeMultiball
	case "spawn":
		return SessionTypeSpawn
	case "cron":
		return SessionTypeCron
	default:
		return SessionTypeUnknown
	}
}

// splitKeyParts splits a session key on colons.
func splitKeyParts(key string) []string {
	return strings.Split(key, ":")
}

// ScanAllSessions walks all non-archive session files and returns index entries.
func (s *Store) ScanAllSessions() ([]SessionIndexEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var entries []SessionIndexEntry
	err := filepath.Walk(s.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if isArchiveFile(filepath.Base(path)) {
			return nil
		}

		rel, err := filepath.Rel(s.dir, path)
		if err != nil {
			return nil
		}
		rel = strings.TrimSuffix(rel, ".jsonl")
		key := strings.ReplaceAll(rel, string(filepath.Separator), ":")

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

		// Determine status: if archives exist, this file has been compacted
		status := SessionStatusActive
		basePath := path
		ext := filepath.Ext(basePath)
		stem := strings.TrimSuffix(basePath, ext)
		if _, err := os.Stat(stem + ".1" + ext); err == nil {
			status = SessionStatusCompacted
		}

		entries = append(entries, SessionIndexEntry{
			SessionKey:       key,
			FilePath:         path,
			CreatedAt:        createdAt,
			ParentSessionKey: parentKey,
			SessionType:      stype,
			Status:           status,
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return entries, err
	}
	return entries, nil
}

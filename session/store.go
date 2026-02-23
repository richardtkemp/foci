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

	"clod/anthropic"
)

// SessionMeta is stored as the first line in a session file to preserve metadata
// like the original creation time (important for compaction continuity).
type SessionMeta struct {
	Type      string `json:"type"`       // "session_meta"
	CreatedAt string `json:"created_at"` // RFC3339 timestamp
}

// Store is a JSONL-backed session store.
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore creates a session store rooted at dir.
func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// keyToPath converts a session key to a file path.
// Key format: agent:AGENTID:TYPE[:BRANCHID]
// Path format: {dir}/agent/AGENTID/TYPE[/BRANCHID].jsonl
func (s *Store) keyToPath(key string) string {
	parts := strings.Split(key, ":")
	// parts[0] = "agent", parts[1] = AGENTID, parts[2] = TYPE, parts[3] = BRANCHID (optional)
	if len(parts) == 4 {
		return filepath.Join(s.dir, parts[0], parts[1], parts[2], parts[3]+".jsonl")
	}
	return filepath.Join(s.dir, parts[0], parts[1], parts[2]+".jsonl")
}

// Load reads all messages from a session file.
// Returns nil (not error) if file doesn't exist.
func (s *Store) Load(key string) ([]anthropic.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.loadUnlocked(key)
}

func (s *Store) loadUnlocked(key string) ([]anthropic.Message, error) {
	path := s.keyToPath(key)

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

	return messages, nil
}

// Append adds a message to the session file, creating it if needed.
func (s *Store) Append(key string, msg anthropic.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.appendUnlocked(key, msg)
}

func (s *Store) appendUnlocked(key string, msg anthropic.Message) error {
	path := s.keyToPath(key)

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
		meta := SessionMeta{
			Type:      "session_meta",
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		metaData, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("marshal session meta: %w", err)
		}
		if _, err := f.Write(append(metaData, '\n')); err != nil {
			return fmt.Errorf("write session meta: %w", err)
		}
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

	path := s.keyToPath(key)
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Replace overwrites a session with the given messages.
func (s *Store) Replace(key string, msgs []anthropic.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.keyToPath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}

	// Get existing creation time to preserve through compaction
	createdAt := s.getStoredCreatedAt(key)

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create session file: %w", err)
	}
	defer f.Close()

	// Write session metadata to preserve creation time
	if createdAt != "" {
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
	return nil
}

// getStoredCreatedAt reads the stored creation time from an existing session file.
func (s *Store) getStoredCreatedAt(key string) string {
	path := s.keyToPath(key)
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
			Content: anthropic.TextContent("[System restarted at " + now.UTC().Format(time.RFC3339) + "]"),
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
	path := s.keyToPath(key)
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
	path := s.keyToPath(key)
	info, err := os.Stat(path)
	if err != nil {
		return "n/a"
	}
	return info.ModTime().UTC().Format("2006-01-02T15:04:05Z")
}

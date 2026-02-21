package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"clod/anthropic"
)

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
		var probe struct{ Type string `json:"type"` }
		if json.Unmarshal(line, &probe) == nil && probe.Type == "branch_meta" {
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

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open session file: %w", err)
	}
	defer f.Close()

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

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create session file: %w", err)
	}
	defer f.Close()

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

// MessageCount returns the number of messages in a session.
func (s *Store) MessageCount(key string) (int, error) {
	msgs, err := s.Load(key)
	if err != nil {
		return 0, err
	}
	return len(msgs), nil
}

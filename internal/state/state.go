package state

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"foci/internal/log"
)

// Store provides thread-safe JSON file-backed key-value persistence.
type Store struct {
	path string
	mu   sync.Mutex
	data map[string]json.RawMessage
}

// New creates a state store backed by the given file path.
// Call Load() to read existing data from disk.
func New(path string) *Store {
	return &Store{
		path: path,
		data: make(map[string]json.RawMessage),
	}
}

// Load reads existing state from disk. Missing file is not an error.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state file: %w", err)
	}

	m := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("parse state file: %w", err)
	}
	s.data = m
	return nil
}

// Get unmarshals the value for key into v. Returns false if key not found.
func (s *Store) Get(key string, v interface{}) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, ok := s.data[key]
	if !ok {
		return false
	}
	if err := json.Unmarshal(raw, v); err != nil {
		log.Warnf("state", "unmarshal key %q: %v", key, err)
		return false
	}
	return true
}

// Set stores a value for key and saves to disk immediately.
func (s *Store) Set(key string, v interface{}) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.data[key] = raw
	return s.saveLocked()
}

// Delete removes a key and saves to disk.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.data, key)
	return s.saveLocked()
}

// saveLocked writes the current data to disk. Must be called with mu held.
func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	return nil
}

// DeleteKeys removes multiple keys and saves to disk once.
func (s *Store) DeleteKeys(keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, k := range keys {
		delete(s.data, k)
	}
	return s.saveLocked()
}

// AllKeys returns all keys in the state store.
// Used for migration from state.json to database.
func (s *Store) AllKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		keys = append(keys, k)
	}
	return keys
}

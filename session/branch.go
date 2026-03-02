package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"foci/anthropic"
	"foci/log"
)

// BranchMeta is stored as the first line of a branch session file.
type BranchMeta struct {
	Type        string `json:"type"` // always "branch_meta"
	ParentKey   string `json:"parent_key"`
	BranchPoint int    `json:"branch_point"`
	NoResetHook bool   `json:"no_reset_hook,omitempty"` // skip pre-reset memory hook
}

// BranchOptions configures optional behavior for a new branch session.
type BranchOptions struct {
	NoResetHook        bool   // skip pre-reset memory hook when this branch is reclaimed
	OrientationMessage string // if non-empty, written as first user message in the branch
}

// CreateBranchWithOptions creates a branch session with additional options.
func (s *Store) CreateBranchWithOptions(parentKey, branchKey string, opts BranchOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	parentMsgs, err := s.loadUnlocked(parentKey)
	if err != nil {
		return fmt.Errorf("load parent: %w", err)
	}

	meta := BranchMeta{
		Type:        "branch_meta",
		ParentKey:   parentKey,
		BranchPoint: len(parentMsgs),
		NoResetHook: opts.NoResetHook,
	}

	path, err := s.keyToPath(branchKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create branch dir: %w", err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create branch file: %w", err)
	}
	defer func() { _ = f.Close() }()

	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal branch meta: %w", err)
	}

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write branch meta: %w", err)
	}

	// Write orientation message as first user message if provided.
	if opts.OrientationMessage != "" {
		orientMsg := anthropic.Message{
			Role:    "user",
			Content: anthropic.TextContent(opts.OrientationMessage),
		}
		orientData, err := json.Marshal(orientMsg)
		if err != nil {
			return fmt.Errorf("marshal orientation message: %w", err)
		}
		if _, err := f.Write(append(orientData, '\n')); err != nil {
			return fmt.Errorf("write orientation message: %w", err)
		}
	}

	log.Infof("session", "branch created key=%s parent=%s branch_point=%d no_reset_hook=%v orientation=%v",
		branchKey, parentKey, meta.BranchPoint, opts.NoResetHook, opts.OrientationMessage != "")
	s.fireEvent(SessionEvent{
		Key:       branchKey,
		Type:      ClassifySessionKey(branchKey),
		Status:    SessionStatusActive,
		ParentKey: parentKey,
		FilePath:  path,
		CreatedAt: time.Now().UTC(),
	})
	return nil
}

// GetBranchMeta returns the branch metadata for a session key, or nil if not a branch.
func (s *Store) GetBranchMeta(key string) (*BranchMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readBranchMeta(key)
}

// LoadFull loads the full message history for a session.
// For branch sessions, this is parent[:branch_point] + branch messages.
// For regular sessions, this is the same as Load.
func (s *Store) LoadFull(key string) ([]anthropic.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if this is a branch by reading the first line
	meta, err := s.readBranchMeta(key)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		// Regular session
		return s.loadUnlocked(key)
	}

	// Branch session: load parent prefix + own messages
	parentMsgs, err := s.loadUnlocked(meta.ParentKey)
	if err != nil {
		return nil, fmt.Errorf("load parent for branch: %w", err)
	}

	branchPoint := meta.BranchPoint
	if branchPoint > len(parentMsgs) {
		branchPoint = len(parentMsgs)
	}

	prefix := parentMsgs[:branchPoint]
	ownMsgs, err := s.loadUnlocked(key)
	if err != nil {
		return nil, fmt.Errorf("load branch messages: %w", err)
	}

	result := make([]anthropic.Message, 0, len(prefix)+len(ownMsgs))
	result = append(result, prefix...)
	result = append(result, ownMsgs...)
	log.Debugf("session", "branch loaded key=%s parent_msgs=%d own_msgs=%d total=%d", key, len(prefix), len(ownMsgs), len(result))
	return result, nil
}

// readBranchMeta reads branch metadata from the first line of a session file.
// Returns nil, nil if the session is not a branch.
func (s *Store) readBranchMeta(key string) (*BranchMeta, error) {
	path, err := s.keyToPath(key)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open for branch check: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Read first line
	buf := make([]byte, 4096)
	n, err := f.Read(buf)
	if err != nil || n == 0 {
		return nil, nil
	}

	// Find first newline
	for i := 0; i < n; i++ {
		if buf[i] == '\n' {
			var meta BranchMeta
			if json.Unmarshal(buf[:i], &meta) == nil && meta.Type == "branch_meta" {
				return &meta, nil
			}
			return nil, nil
		}
	}

	return nil, nil
}

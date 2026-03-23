package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"foci/internal/log"
	"foci/internal/provider"
)

// BranchMeta is stored as the first line of a branch session file.
type BranchMeta struct {
	Type        string `json:"type"` // always "branch_meta"
	ParentKey   string `json:"parent_key"`
	BranchPoint int    `json:"branch_point"`
	NoResetHook bool   `json:"no_reset_hook,omitempty"` // skip pre-reset memory hook
	Orientation string `json:"orientation,omitempty"`    // orientation text for first turn
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
		Orientation: opts.OrientationMessage,
	}

	path, err := s.SessionPath(branchKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create branch dir: %w", err)
	}

	f, err := s.createFile(path)
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

// PendingOrientation returns the orientation text for a branch session that
// hasn't had its first turn yet. Returns "" for non-branches or branches that
// already have messages (orientation was consumed on the first turn).
func (s *Store) PendingOrientation(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	meta, err := s.readBranchMeta(key)
	if err != nil || meta == nil || meta.Orientation == "" {
		return ""
	}

	// Only return orientation if the branch has no own messages yet (first turn).
	ownMsgs, err := s.loadUnlocked(key)
	if err != nil || len(ownMsgs) > 0 {
		return ""
	}

	return meta.Orientation
}

// LoadFull loads the full message history for a session.
// For branch sessions, this is parent[:branch_point] + branch messages.
// For regular sessions, this is the same as Load.
func (s *Store) LoadFull(key string) ([]provider.Message, error) {
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

	result := make([]provider.Message, 0, len(prefix)+len(ownMsgs))
	result = append(result, prefix...)
	result = append(result, ownMsgs...)
	log.Debugf("session", "branch loaded key=%s parent_msgs=%d own_msgs=%d total=%d", key, len(prefix), len(ownMsgs), len(result))
	return result, nil
}

// readBranchMeta reads branch metadata from the first line of a session file.
// Returns nil, nil if the session is not a branch.
func (s *Store) readBranchMeta(key string) (*BranchMeta, error) {
	path, err := s.SessionPath(key)
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

	// Read first line using a scanner to handle large meta lines
	// (orientation text is stored in the meta JSON).
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)
	if !scanner.Scan() {
		return nil, nil
	}
	line := scanner.Bytes()
	if len(line) == 0 {
		return nil, nil
	}

	var meta BranchMeta
	if json.Unmarshal(line, &meta) == nil && meta.Type == "branch_meta" {
		return &meta, nil
	}
	return nil, nil
}

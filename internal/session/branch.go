package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/log"
	"foci/internal/provider"
	"foci/internal/timeutil"
)

// errBranchFileExists is returned when a branch file already exists (key collision).
var errBranchFileExists = errors.New("branch file already exists")

// BranchMeta is stored as the first line of a branch session file.
type BranchMeta struct {
	Type        string `json:"type"` // always "branch_meta"
	ParentKey   string `json:"parent_key"`
	BranchPoint int    `json:"branch_point"`
	NoResetHook bool   `json:"no_reset_hook,omitempty"` // skip pre-reset memory hook
	Orientation string `json:"orientation,omitempty"`    // orientation text for first turn
}

// Template placeholders for orientation text. These are substituted by
// CreateBranchWithOptions when creating the branch file.
const (
	BranchKeyVar  = "{branch_key}"
	ParentKeyVar  = "{parent_key}"
	BranchTypeVar = "{branch_type}"
)

// BranchOptions configures a new branch session.
type BranchOptions struct {
	NoResetHook         bool   // skip pre-reset memory hook when this branch is reclaimed
	BranchType          string // e.g. "cron", "compaction-memory" — resolves {branch_type}
	OrientationTemplate string // template with {branch_key}, {parent_key}, {branch_type} placeholders
}

// resolveOrientation substitutes template placeholders in the orientation text.
func resolveOrientation(template, branchKey, parentKey, branchType string) string {
	if template == "" {
		return ""
	}
	r := strings.NewReplacer(
		BranchKeyVar, branchKey,
		ParentKeyVar, parentKey,
		BranchTypeVar, branchType,
	)
	return r.Replace(template)
}

// CreateBranchWithOptions creates a branch session from parentKey.
// Generates the branch key internally, resolves orientation template
// placeholders, and writes the branch file. On same-second key collision,
// sleeps past the second boundary and retries with a fresh key.
// Returns the branch key used.
func (s *Store) CreateBranchWithOptions(parentKey string, opts BranchOptions) (string, error) {
	const maxRetries = 3

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Second)
			log.Warnf("session", "branch key collision on %s, retrying (attempt %d/%d)", parentKey, attempt, maxRetries)
		}

		branchKey, err := branchFromSession(parentKey)
		if err != nil {
			return "", fmt.Errorf("generate branch key: %w", err)
		}

		orientation := resolveOrientation(opts.OrientationTemplate, branchKey, parentKey, opts.BranchType)
		err = s.createBranchFile(parentKey, branchKey, opts.NoResetHook, orientation)
		if err == nil {
			return branchKey, nil
		}
		if !errors.Is(err, errBranchFileExists) {
			return "", err
		}
	}
	return "", fmt.Errorf("branch creation failed: key collision after %d attempts (parent=%s)", maxRetries+1, parentKey)
}

// createBranchFile performs a single attempt to create a branch file with
// exclusive semantics. Returns errBranchFileExists if the file already exists.
func (s *Store) createBranchFile(parentKey, branchKey string, noResetHook bool, orientation string) error {
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
		NoResetHook: noResetHook,
		Orientation: orientation,
	}

	path, err := s.SessionPath(branchKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create branch dir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, s.fileMode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return errBranchFileExists
		}
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
		branchKey, parentKey, meta.BranchPoint, noResetHook, orientation != "")
	s.fireEvent(SessionEvent{
		Key:       branchKey,
		Type:      ClassifySessionKey(branchKey),
		Status:    SessionStatusActive,
		ParentKey: parentKey,
		FilePath:  path,
		CreatedAt: timeutil.Now(),
	})
	return nil
}

// GetBranchMeta returns the branch metadata for a session key, or nil if not a branch.
func (s *Store) GetBranchMeta(key string) (*BranchMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readBranchMeta(key)
}

// ConsumeOrientation atomically returns the orientation text for a branch
// session and clears it from the stored metadata. Returns "" for non-branches
// or branches whose orientation was already consumed. Safe to call multiple
// times — only the first call returns the orientation.
//
// The orientation is cleared on disk so it survives restarts correctly:
// API branches won't re-inject, delegated branches start fresh anyway.
func (s *Store) ConsumeOrientation(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	meta, err := s.readBranchMeta(key)
	if err != nil || meta == nil || meta.Orientation == "" {
		return ""
	}

	orientation := meta.Orientation

	// Clear orientation from stored meta so it's never re-consumed.
	meta.Orientation = ""
	if err := s.rewriteBranchMeta(key, meta); err != nil {
		log.Warnf("session", "failed to clear consumed orientation for %s: %v", key, err)
		// Return orientation anyway — double-injection is better than no injection.
	}

	return orientation
}

// rewriteBranchMeta replaces the first line (BranchMeta JSON) of a branch
// session file, preserving all subsequent lines (messages).
func (s *Store) rewriteBranchMeta(key string, meta *BranchMeta) error {
	path, err := s.SessionPath(key)
	if err != nil {
		return err
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	newMeta, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	// Replace first line (up to first \n) with new meta.
	idx := bytes.IndexByte(content, '\n')
	if idx < 0 {
		content = append(newMeta, '\n')
	} else {
		content = append(append(newMeta, '\n'), content[idx+1:]...)
	}

	return os.WriteFile(path, content, s.fileMode)
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

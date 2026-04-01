package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"foci/internal/log"
	"foci/internal/provider"
	"foci/internal/timeutil"
)

// SessionWriter is the interface for writing to a specific session.
type SessionWriter interface {
	Append(key string, msg provider.Message) error
	AppendAll(key string, msgs []provider.Message) error
	Replace(key string, msgs []provider.Message) error
	ReplaceAndRotate(key string, msgs []provider.Message) (string, error)
	Clear(key string) error
}

// SessionMeta is stored as the first line in a session file to preserve metadata
// like the original creation time (important for compaction continuity).
type SessionMeta struct {
	Type      string `json:"type"`       // "session_meta"
	CreatedAt string `json:"created_at"` // RFC3339 timestamp
}

// Store is a JSONL-backed session store.
type Store struct {
	dir      string
	fileMode os.FileMode
	mu       sync.Mutex
	onEvent  func(SessionEvent)
}

// NewStore creates a session store rooted at dir.
// Session files are created with mode 0600 by default; call SetFileMode to override.
func NewStore(dir string) *Store {
	return &Store{dir: dir, fileMode: 0600}
}

// SetFileMode sets the permission bits used when creating session files.
func (s *Store) SetFileMode(mode os.FileMode) {
	s.fileMode = mode
}

// createFile creates a new file with the configured file mode.
func (s *Store) createFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, s.fileMode)
}

// openFileAppend opens a file for appending, creating it with the configured
// file mode if it doesn't exist.
func (s *Store) openFileAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, s.fileMode)
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
type sessionWriter struct {
	store      *Store
	sessionKey string
}

// For creates a SessionWriter that can only modify the specified session.
// This enforces session ownership: all write operations are restricted to this session.
func (s *Store) For(sessionKey string) SessionWriter {
	return &sessionWriter{
		store:      s,
		sessionKey: sessionKey,
	}
}

// Append adds a message to the owned session, rejecting cross-session writes.
func (w *sessionWriter) Append(key string, msg provider.Message) error {
	if key != w.sessionKey {
		return fmt.Errorf("cross-session write blocked: SessionWriter for session %q cannot write to session %q",
			w.sessionKey, key)
	}
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	return w.store.appendUnlocked(key, msg)
}

// AppendAll adds multiple messages to the owned session, rejecting cross-session writes.
// All messages are marshaled and written in a single file operation to prevent
// partial writes that could cause duplicate tool_use IDs on retry.
func (w *sessionWriter) AppendAll(key string, msgs []provider.Message) error {
	if key != w.sessionKey {
		return fmt.Errorf("cross-session write blocked: SessionWriter for session %q cannot write to session %q",
			w.sessionKey, key)
	}
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	return w.store.appendAllUnlocked(key, msgs)
}

// Replace overwrites the owned session with new messages, rejecting cross-session writes.
func (w *sessionWriter) Replace(key string, msgs []provider.Message) error {
	if key != w.sessionKey {
		return fmt.Errorf("cross-session write blocked: SessionWriter for session %q cannot write to session %q",
			w.sessionKey, key)
	}
	return w.store.replaceInternal(key, msgs)
}

// ReplaceAndRotate overwrites the owned session with new messages under a rotated
// key (new VersionTS), archiving the old file. Returns the new session key.
func (w *sessionWriter) ReplaceAndRotate(key string, msgs []provider.Message) (string, error) {
	if key != w.sessionKey {
		return "", fmt.Errorf("cross-session write blocked: SessionWriter for session %q cannot write to session %q",
			w.sessionKey, key)
	}
	return w.store.ReplaceAndRotate(key, msgs)
}

// Clear deletes the owned session, rejecting cross-session writes.
func (w *sessionWriter) Clear(key string) error {
	if key != w.sessionKey {
		return fmt.Errorf("cross-session write blocked: SessionWriter for session %q cannot write to session %q",
			w.sessionKey, key)
	}
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	return w.store.clearUnlocked(key)
}

// SessionPath converts a session key to a file path.
// Key format: {agentID}/{type}{id}/{versionTS}[/{childType}{childTS}]
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

	// If last segment is pure number, it's a root session
	if _, err := strconv.ParseInt(lastSegment, 10, 64); err == nil {
		return filepath.Join(s.dir, key, "root.jsonl"), nil
	}

	// Otherwise it's a child session
	return filepath.Join(s.dir, key+".jsonl"), nil
}

// TestAppend is for testing only - appends without SessionWriter guard.
// Tests should use this when setting up state, not For().Append().
func (s *Store) TestAppend(key string, msg provider.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendUnlocked(key, msg)
}

// TestCreateBranch is for testing only - creates a branch file with a specific key.
// Production code should use CreateBranchWithOptions which generates the key internally.
func (s *Store) TestCreateBranch(parentKey, branchKey string) error {
	return s.createBranchFile(parentKey, branchKey, false, "")
}

// TestAppendAll is for testing only - appends multiple messages without SessionWriter guard.
// Tests should use this when setting up state, not For().AppendAll().
func (s *Store) TestAppendAll(key string, msgs []provider.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendAllUnlocked(key, msgs)
}

// TestClear is for testing only - clears a session without SessionWriter guard.
// Tests should use this when setting up state, not For().Clear().
func (s *Store) TestClear(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clearUnlocked(key)
}

// TestReplace is for testing only - replaces without SessionWriter guard.
// Tests should use this when setting up state, not For().Replace().
func (s *Store) TestReplace(key string, msgs []provider.Message) error {
	return s.replaceInternal(key, msgs)
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

	f, err := s.openFileAppend(path)
	if err != nil {
		return fmt.Errorf("open session file: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Write session metadata on new files
	if !exists {
		now := timeutil.Now()
		log.Infof("session", "session created key=%s", key)
		meta := SessionMeta{
			Type:      "session_meta",
			CreatedAt: timeutil.Format(now),
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

	// Stamp message with current time if not already set
	if msg.Timestamp == nil {
		now := timeutil.Now()
		msg.Timestamp = &now
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

// appendAllUnlocked writes multiple messages in a single file operation.
// All messages are marshaled to a buffer first — if any marshal fails, nothing
// is written to disk. This prevents partial writes that cause duplicate tool_use
// IDs when a defer safety-net re-writes the same messages.
func (s *Store) appendAllUnlocked(key string, msgs []provider.Message) error {
	if len(msgs) == 0 {
		return nil
	}

	path, err := s.SessionPath(key)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}

	// Check if file exists — if not, we need to write session metadata first
	exists := false
	if _, err := os.Stat(path); err == nil {
		exists = true
	}

	// Marshal everything into a buffer first so a marshal failure writes nothing
	var buf bytes.Buffer

	if !exists {
		now := timeutil.Now()
		log.Infof("session", "session created key=%s", key)
		meta := SessionMeta{
			Type:      "session_meta",
			CreatedAt: timeutil.Format(now),
		}
		metaData, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("marshal session meta: %w", err)
		}
		buf.Write(metaData)
		buf.WriteByte('\n')

		// Defer event firing until after successful write
		defer s.fireEvent(SessionEvent{
			Key:       key,
			Type:      ClassifySessionKey(key),
			Status:    SessionStatusActive,
			FilePath:  path,
			CreatedAt: now,
		})
	}

	for _, msg := range msgs {
		// Stamp message with current time if not already set
		if msg.Timestamp == nil {
			now := timeutil.Now()
			msg.Timestamp = &now
		}
		data, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshal message: %w", err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}

	f, err := s.openFileAppend(path)
	if err != nil {
		return fmt.Errorf("open session file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("write messages: %w", err)
	}

	return nil
}

// clearUnlocked removes a session file (internal use only - must hold mutex).
func (s *Store) clearUnlocked(key string) error {
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
	return timeutil.Format(info.ModTime())
}

// SessionEvent describes a lifecycle event on a session.
type SessionEvent struct {
	Key         string
	NewKey      string // set on rotation: the new session key
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
// identifiable by child type. Semantic subtypes (spawn, facet, cron)
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

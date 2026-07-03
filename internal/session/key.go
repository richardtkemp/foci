package session

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SessionKey is a structured, STABLE session identity.
// Format: {agentID}/{type}{id}[/{childType}{childTS}]
//
// The identity never changes for the life of a conversation: compaction and
// /reset archive the underlying file in place (see Store.Replace / Store.Reset)
// without minting a new key. Everything that holds a session key — reminders,
// chat metadata, tmux ownership, in-flight maps, external cron jobs — can hold
// it forever.
//
// Examples:
//
//	clutch/c123              chat session (Telegram/Discord/app chat 123)
//	clutch/iresearch         named independent session
//	clutch/i1709596800       anonymous independent session (creation ts as id)
//	clutch/c123/b1709596800  branch (facet, spawn, cron, memory pass)
//	clutch/c123/i1709596801  independent spawn
type SessionKey struct {
	AgentID   string
	Type      rune   // 'c' (chat) or 'i' (independent)
	ID        string // chat ID, name, or creation timestamp
	ChildType rune   // 'b' (branch) or 'i' (independent spawn), 0 for root
	ChildTS   int64  // child timestamp, 0 for root
}

// parseTypeID extracts a single-character type code and the remaining string ID.
// Returns an error if the input is too short.
func parseTypeID(s string) (rune, string, error) {
	if len(s) < 2 {
		return 0, "", fmt.Errorf("invalid type+id segment: %q", s)
	}
	return rune(s[0]), s[1:], nil
}

// parseTypeTS extracts a single-character type code and parses the remaining timestamp.
// Returns an error if the input is too short or if the timestamp cannot be parsed.
func parseTypeTS(s string) (rune, int64, error) {
	if len(s) < 2 {
		return 0, 0, fmt.Errorf("invalid child segment: %q", s)
	}
	ts, err := strconv.ParseInt(s[1:], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid child timestamp: %w", err)
	}
	return rune(s[0]), ts, nil
}

// String converts the key to its string representation.
func (k SessionKey) String() string {
	var sb strings.Builder

	sb.WriteString(k.AgentID)
	sb.WriteRune('/')
	sb.WriteRune(k.Type)
	sb.WriteString(k.ID)

	if k.ChildType != 0 {
		sb.WriteRune('/')
		sb.WriteRune(k.ChildType)
		sb.WriteString(strconv.FormatInt(k.ChildTS, 10))
	}

	return sb.String()
}

// ParseSessionKey parses a string into a SessionKey.
func ParseSessionKey(s string) (SessionKey, error) {
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return SessionKey{}, fmt.Errorf("invalid session key format: %q (need at least agentID/typeID)", s)
	}
	if len(parts) > 3 {
		return SessionKey{}, fmt.Errorf("invalid session key %q: too many segments", s)
	}

	agentID := parts[0]
	if agentID == "" {
		return SessionKey{}, fmt.Errorf("invalid session key %q: empty agent ID", s)
	}

	typ, id, err := parseTypeID(parts[1])
	if err != nil {
		return SessionKey{}, err
	}
	if typ != 'c' && typ != 'i' {
		return SessionKey{}, fmt.Errorf("invalid session key %q: unknown type %q", s, string(typ))
	}

	key := SessionKey{
		AgentID: agentID,
		Type:    typ,
		ID:      id,
	}

	if len(parts) == 3 {
		childType, childTS, err := parseTypeTS(parts[2])
		if err != nil {
			return SessionKey{}, err
		}
		if childType != 'b' && childType != 'i' {
			return SessionKey{}, fmt.Errorf("invalid session key %q: unknown child type %q", s, string(childType))
		}
		key.ChildType = childType
		key.ChildTS = childTS
	}

	return key, nil
}

// NewChatSession creates the chat session key for a chat ID. Deterministic:
// the same (agentID, chatID) always yields the same key.
func NewChatSession(agentID string, chatID int64) SessionKey {
	return SessionKey{
		AgentID: agentID,
		Type:    'c',
		ID:      strconv.FormatInt(chatID, 10),
	}
}

// withChild creates a child session key with the given child type. Deriving a
// child from a child yields a sibling under the same root — the file layout is
// flat; the logical parent is recorded in the branch_meta line, not the key.
func (k SessionKey) withChild(childType rune) SessionKey {
	return SessionKey{
		AgentID:   k.AgentID,
		Type:      k.Type,
		ID:        k.ID,
		ChildType: childType,
		ChildTS:   time.Now().Unix(),
	}
}

// Branch creates a branch from this session.
func (k SessionKey) Branch() SessionKey {
	return k.withChild('b')
}

// Root returns the root key for this session (itself if already a root).
func (k SessionKey) Root() SessionKey {
	k.ChildType = 0
	k.ChildTS = 0
	return k
}

// IsRoot returns true if this is a root session (not a child).
func (k SessionKey) IsRoot() bool {
	return k.ChildType == 0
}

// ChatID returns the chat ID if this is a chat session, or 0 otherwise.
func (k SessionKey) ChatID() int64 {
	if k.Type != 'c' {
		return 0
	}
	id, _ := strconv.ParseInt(k.ID, 10, 64)
	return id
}

// NewChatSessionKey returns the chat session key string for a chat ID.
// Deterministic: the same (agentID, chatID) always yields the same key.
func NewChatSessionKey(agentID string, chatID int64) string {
	return NewChatSession(agentID, chatID).String()
}

// ValidateSessionName checks that a request-controlled session name is a single
// safe path segment. The name is placed verbatim into the session key (and hence
// into a filesystem path), so it must not contain path separators, "."/".."
// traversal, or control characters — otherwise a caller could escape the session
// directory (P1-5).
func ValidateSessionName(name string) error {
	if name == "" {
		return fmt.Errorf("session name must not be empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("session name %q is reserved", name)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("session name %q must not contain path separators", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("session name must not contain control characters")
		}
	}
	return nil
}

// NamedIndependentSessionKey constructs the deterministic independent session
// key for a given name. The name is used as the ID field so that repeated calls
// with the same agentID and name return the same key. The name is validated
// (see ValidateSessionName) so that request-controlled names cannot escape the
// session directory.
func NamedIndependentSessionKey(agentID, name string) (string, error) {
	if err := ValidateSessionName(name); err != nil {
		return "", err
	}
	return SessionKey{
		AgentID: agentID,
		Type:    'i',
		ID:      name,
	}.String(), nil
}

// ChatIDFromKey extracts the chat ID from a session key string.
// Returns 0 if the key doesn't contain a chat ID.
func ChatIDFromKey(key string) int64 {
	if sk, err := ParseSessionKey(key); err == nil {
		return sk.ChatID()
	}
	return 0
}

// AgentIDFromKey extracts the agent ID (first segment) from a session key
// string. Returns "" if the key is malformed. Equivalent to
// ParseSessionKey(key).AgentID with the error path discarded — use it when
// the agent ID is the only field needed and a parse failure should fall back
// to "no agent" rather than surfacing as an error. Mirrors ChatIDFromKey.
func AgentIDFromKey(key string) string {
	idx := strings.Index(key, "/")
	if idx <= 0 {
		// No separator (or starts with /) — not a valid session key.
		// Don't return the whole string as an "agent ID" — that hides
		// the fact that it's malformed.
		return ""
	}
	return key[:idx]
}

// branchFromSession creates a branch child key from a parent session key string.
func branchFromSession(parentKey string) (string, error) {
	parent, err := ParseSessionKey(parentKey)
	if err != nil {
		return "", err
	}
	return parent.Branch().String(), nil
}

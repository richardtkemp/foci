package session

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SessionKey represents a structured session identifier.
// Format: {agentID}/{type}{id}/{versionTS}[/{childType}{childTS}][.{n}]
type SessionKey struct {
	AgentID   string
	Type      rune   // 'c' (chat) or 'i' (independent)
	ID        string // chat ID or creation timestamp
	VersionTS int64  // version timestamp (creation or compaction time)
	ChildType rune   // 'b' (branch) or 'i' (independent spawn), 0 for root
	ChildTS   int64  // child timestamp, 0 for root
	Collision int    // collision counter, 0 for first
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

	// Base: agentID/typeID/versionTS
	sb.WriteString(k.AgentID)
	sb.WriteRune('/')
	sb.WriteRune(k.Type)
	sb.WriteString(k.ID)
	sb.WriteRune('/')
	sb.WriteString(strconv.FormatInt(k.VersionTS, 10))

	// Child suffix: /childTypeTS
	if k.ChildType != 0 {
		sb.WriteRune('/')
		sb.WriteRune(k.ChildType)
		sb.WriteString(strconv.FormatInt(k.ChildTS, 10))
	}

	// Collision suffix: .N
	if k.Collision > 0 {
		sb.WriteRune('.')
		sb.WriteString(strconv.Itoa(k.Collision))
	}

	return sb.String()
}

// ParseSessionKey parses a string into a SessionKey.
func ParseSessionKey(s string) (SessionKey, error) {
	// Handle collision suffix
	var collision int
	if idx := strings.LastIndex(s, "."); idx > 0 {
		if n, err := strconv.Atoi(s[idx+1:]); err == nil && n > 0 {
			collision = n
			s = s[:idx]
		}
	}

	parts := strings.Split(s, "/")
	if len(parts) < 3 {
		return SessionKey{}, fmt.Errorf("invalid session key format: %q (need at least agentID/typeID/versionTS)", s)
	}

	agentID := parts[0]

	// Parse type and ID from second part (e.g., "c123" or "i1709596800")
	typ, id, err := parseTypeID(parts[1])
	if err != nil {
		return SessionKey{}, err
	}

	// Parse version timestamp
	versionTS, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return SessionKey{}, fmt.Errorf("invalid version timestamp: %w", err)
	}

	key := SessionKey{
		AgentID:   agentID,
		Type:      typ,
		ID:        id,
		VersionTS: versionTS,
		Collision: collision,
	}

	// Check for child suffix (4th part)
	if len(parts) >= 4 {
		childType, childTS, err := parseTypeTS(parts[3])
		if err != nil {
			return SessionKey{}, err
		}
		key.ChildType = childType
		key.ChildTS = childTS
	}

	if len(parts) > 4 {
		return SessionKey{}, fmt.Errorf("invalid session key: too many segments")
	}

	return key, nil
}

// NewChatSession creates a new chat session key.
func NewChatSession(agentID string, chatID int64) SessionKey {
	return SessionKey{
		AgentID:   agentID,
		Type:      'c',
		ID:        strconv.FormatInt(chatID, 10),
		VersionTS: time.Now().Unix(),
	}
}

// NewIndependentSession creates a new independent session key.
func NewIndependentSession(agentID string) SessionKey {
	ts := time.Now().Unix()
	return SessionKey{
		AgentID:   agentID,
		Type:      'i',
		ID:        strconv.FormatInt(ts, 10), // independent sessions use timestamp as ID
		VersionTS: ts,
	}
}

// withChild creates a child session key with the given child type.
func (k SessionKey) withChild(childType rune) SessionKey {
	return SessionKey{
		AgentID:   k.AgentID,
		Type:      k.Type,
		ID:        k.ID,
		VersionTS: k.VersionTS,
		ChildType: childType,
		ChildTS:   time.Now().Unix(),
	}
}

// Branch creates a branch from this session.
func (k SessionKey) Branch() SessionKey {
	return k.withChild('b')
}

// IndependentSpawn creates an independent spawn from this session.
func (k SessionKey) IndependentSpawn() SessionKey {
	return k.withChild('i')
}

// WithCollision returns a copy with the collision counter set.
func (k SessionKey) WithCollision(n int) SessionKey {
	k.Collision = n
	return k
}

// WithVersion returns a copy with a new version timestamp.
func (k SessionKey) WithVersion(versionTS int64) SessionKey {
	k.VersionTS = versionTS
	k.ChildType = 0
	k.ChildTS = 0
	k.Collision = 0
	return k
}

// IsRoot returns true if this is a root session (not a child).
func (k SessionKey) IsRoot() bool {
	return k.ChildType == 0
}

// RootKey returns the root key (strips child suffix if present).
func (k SessionKey) RootKey() SessionKey {
	if k.IsRoot() {
		return k
	}
	return SessionKey{
		AgentID:   k.AgentID,
		Type:      k.Type,
		ID:        k.ID,
		VersionTS: k.VersionTS,
	}
}

// ChatID returns the chat ID if this is a chat session, or 0 otherwise.
func (k SessionKey) ChatID() int64 {
	if k.Type != 'c' {
		return 0
	}
	id, _ := strconv.ParseInt(k.ID, 10, 64)
	return id
}

// NewChatSessionKey creates a NEW chat session key with the current timestamp.
// Each call generates a unique key — cache the result if you need stable keys across calls.
func NewChatSessionKey(agentID string, chatID int64) string {
	return NewChatSession(agentID, chatID).String()
}


// IndependentSessionKey constructs an independent session key string.
// Uses current timestamp as both ID and version. Call this for HTTP or other independent sessions.
func IndependentSessionKey(agentID string) string {
	return NewIndependentSession(agentID).String()
}

// NamedIndependentSessionKey constructs a deterministic independent session key
// for a given name. The name is used as the ID field so that repeated calls with
// the same agentID and name return the same key. The version timestamp is fixed
// at 0 to ensure stability.
func NamedIndependentSessionKey(agentID, name string) string {
	return SessionKey{
		AgentID:   agentID,
		Type:      'i',
		ID:        name,
		VersionTS: 0,
	}.String()
}

// ChatIDFromKey extracts the chat ID from a session key string.
// Session keys use the format "{agentID}/c{chatID}/{versionTS}".
// Returns 0 if the key doesn't contain a chat ID.
func ChatIDFromKey(key string) int64 {
	if sk, err := ParseSessionKey(key); err == nil {
		return sk.ChatID()
	}
	return 0
}

// BranchFromSession creates a branch child key from a parent session key string.
func BranchFromSession(parentKey string) (string, error) {
	parent, err := ParseSessionKey(parentKey)
	if err != nil {
		return "", err
	}
	return parent.Branch().String(), nil
}

// IndependentSpawnFromSession creates an independent spawn child key from a parent session key string.
func IndependentSpawnFromSession(parentKey string) (string, error) {
	parent, err := ParseSessionKey(parentKey)
	if err != nil {
		return "", err
	}
	return parent.IndependentSpawn().String(), nil
}

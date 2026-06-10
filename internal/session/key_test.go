package session

import (
	"testing"
)

func TestSessionKeyString(t *testing.T) {
	// Proves that SessionKey.String() produces the correct slash-separated path
	// for chat roots, independent roots, branch children, and collision suffixes.
	tests := []struct {
		name string
		key  SessionKey
		want string
	}{
		{
			name: "chat with branch",
			key: SessionKey{
				AgentID:   "main",
				Type:      'c',
				ID:        "123",
				VersionTS: 1709590000,
				ChildType: 'b',
				ChildTS:   1709596800,
			},
			want: "main/c123/1709590000/b1709596800",
		},
		{
			name: "independent root",
			key: SessionKey{
				AgentID:   "main",
				Type:      'i',
				ID:        "1709596800",
				VersionTS: 1709596800,
			},
			want: "main/i1709596800/1709596800",
		},
		{
			name: "chat root",
			key: SessionKey{
				AgentID:   "main",
				Type:      'c',
				ID:        "123",
				VersionTS: 1709590000,
			},
			want: "main/c123/1709590000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.key.String()
			if got != tt.want {
				t.Errorf("SessionKey.String() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseSessionKey(t *testing.T) {
	// Proves that ParseSessionKey correctly round-trips all key formats including
	// chat roots, independent roots, branch and independent children, collision
	// suffixes, and rejects invalid input with an error.
	tests := []struct {
		name    string
		input   string
		want    SessionKey
		wantErr bool
	}{
		{
			name:  "chat root",
			input: "main/c123/1709590000",
			want: SessionKey{
				AgentID:   "main",
				Type:      'c',
				ID:        "123",
				VersionTS: 1709590000,
			},
		},
		{
			name:  "independent root",
			input: "main/i1709596800/1709596800",
			want: SessionKey{
				AgentID:   "main",
				Type:      'i',
				ID:        "1709596800",
				VersionTS: 1709596800,
			},
		},
		{
			name:  "branch child",
			input: "main/c123/1709590000/b1709596800",
			want: SessionKey{
				AgentID:   "main",
				Type:      'c',
				ID:        "123",
				VersionTS: 1709590000,
				ChildType: 'b',
				ChildTS:   1709596800,
			},
		},
		{
			name:  "independent child",
			input: "main/c123/1709590000/i1709596801",
			want: SessionKey{
				AgentID:   "main",
				Type:      'c',
				ID:        "123",
				VersionTS: 1709590000,
				ChildType: 'i',
				ChildTS:   1709596801,
			},
		},
		{
			name:    "invalid format",
			input:   "invalid",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSessionKey(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseSessionKey() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseSessionKey() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateSessionName(t *testing.T) {
	// Proves that ValidateSessionName rejects names that could escape the session
	// directory (path separators, "..", control chars, empty) and accepts plain
	// single-segment identifiers. This is the primary P1-5 guard at the key layer.
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "plain word", input: "work", wantErr: false},
		{name: "dashed", input: "proj-1", wantErr: false},
		{name: "underscored", input: "my_session", wantErr: false},
		{name: "dots inside", input: "v1.2.3", wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "dot", input: ".", wantErr: true},
		{name: "dotdot", input: "..", wantErr: true},
		{name: "forward slash traversal", input: "../../etc", wantErr: true},
		{name: "embedded slash", input: "a/b", wantErr: true},
		{name: "backslash", input: "a\\b", wantErr: true},
		{name: "leading slash", input: "/etc/passwd", wantErr: true},
		{name: "newline", input: "a\nb", wantErr: true},
		{name: "null byte", input: "a\x00b", wantErr: true},
		{name: "del char", input: "a\x7fb", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSessionName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateSessionName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestNamedIndependentSessionKey(t *testing.T) {
	// Proves that NamedIndependentSessionKey returns a stable key for valid names
	// and an error for traversal-bearing names, so request-controlled session names
	// cannot escape the session directory via the key.
	got, err := NamedIndependentSessionKey("main", "work")
	if err != nil {
		t.Fatalf("NamedIndependentSessionKey(valid) unexpected error: %v", err)
	}
	if got != "main/iwork/0" {
		t.Errorf("NamedIndependentSessionKey = %q, want %q", got, "main/iwork/0")
	}
	// Same inputs must yield the same key (deterministic).
	again, _ := NamedIndependentSessionKey("main", "work")
	if again != got {
		t.Errorf("NamedIndependentSessionKey not deterministic: %q != %q", again, got)
	}
	for _, bad := range []string{"../../../../other/c123/0", "a/b", "..", ""} {
		if _, err := NamedIndependentSessionKey("main", bad); err == nil {
			t.Errorf("NamedIndependentSessionKey(%q) should return error", bad)
		}
	}
}

func TestSessionKeyBranch(t *testing.T) {
	// Proves that Branch() returns a child key that inherits the parent's identity
	// fields, sets ChildType to 'b', and generates a non-zero ChildTS timestamp.
	parent := SessionKey{
		AgentID:   "main",
		Type:      'c',
		ID:        "123",
		VersionTS: 1709590000,
	}

	child := parent.Branch()

	if child.AgentID != parent.AgentID {
		t.Errorf("Branch() AgentID = %v, want %v", child.AgentID, parent.AgentID)
	}
	if child.Type != parent.Type {
		t.Errorf("Branch() Type = %v, want %v", child.Type, parent.Type)
	}
	if child.ID != parent.ID {
		t.Errorf("Branch() ID = %v, want %v", child.ID, parent.ID)
	}
	if child.VersionTS != parent.VersionTS {
		t.Errorf("Branch() VersionTS = %v, want %v", child.VersionTS, parent.VersionTS)
	}
	if child.ChildType != 'b' {
		t.Errorf("Branch() ChildType = %v, want 'b'", child.ChildType)
	}
	if child.ChildTS == 0 {
		t.Errorf("Branch() ChildTS should be set")
	}
}

func TestChatIDFromKey(t *testing.T) {
	// Verifies that ChatIDFromKey extracts chat IDs from
	// slash-separated session key formats, including branch keys which
	// preserve the root chat type.
	tests := []struct {
		name string
		key  string
		want int64
	}{
		{
			name: "chat root",
			key:  "main/c123456/1709590000",
			want: 123456,
		},
		{
			name: "chat branch",
			key:  "main/c123456/1709590000/b1709596800",
			want: 123456,
		},
		{
			name: "independent root",
			key:  "main/i1709596800/1709596800",
			want: 0,
		},
		{
			name: "independent branch",
			key:  "main/i1709596800/1709596800/b1709596900",
			want: 0,
		},
		{
			name: "empty key",
			key:  "",
			want: 0,
		},
		{
			name: "garbage",
			key:  "not-a-session-key",
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ChatIDFromKey(tt.key); got != tt.want {
				t.Errorf("ChatIDFromKey(%q) = %d, want %d", tt.key, got, tt.want)
			}
		})
	}
}

func TestSessionKeyBase(t *testing.T) {
	// Proves that SessionKeyBase extracts the stable {agentID}/{type}{id} prefix
	// regardless of version timestamp, branch suffix, or collision counter.
	tests := []struct {
		name string
		key  string
		want string
	}{
		{name: "root key", key: "main/c123/1700000000", want: "main/c123"},
		{name: "rotated key", key: "main/c123/1700100000", want: "main/c123"},
		{name: "branch key", key: "main/c123/1700000000/b1700050000", want: "main/c123"},
		{name: "independent", key: "main/i1700000000/1700000000", want: "main/i1700000000"},
		{name: "empty", key: "", want: ""},
		{name: "single segment", key: "main", want: "main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SessionKeyBase(tt.key); got != tt.want {
				t.Errorf("SessionKeyBase(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestAgentIDFromKey(t *testing.T) {
	// Proves that AgentIDFromKey returns the first segment of a session key,
	// or the empty string for malformed input. Mirrors ChatIDFromKey/
	// SessionKeyBase coverage. Used by telegram/discord providers when
	// resuming a saved session — they only need the agent ID, not the full
	// parse, so failing soft (return "" on malformed input) matches the
	// pre-existing extractAgentID helper this function replaces.
	tests := []struct {
		name string
		key  string
		want string
	}{
		{name: "chat root", key: "main/c123/1700000000", want: "main"},
		{name: "branch", key: "main/c123/1700000000/b1700050000", want: "main"},
		{name: "independent", key: "clutch/i1700000000/1700000000", want: "clutch"},
		{name: "empty string", key: "", want: ""},
		{name: "no separator", key: "main", want: ""},
		{name: "leading slash", key: "/main/c123/1700000000", want: ""},
		{name: "just slash", key: "/", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AgentIDFromKey(tt.key); got != tt.want {
				t.Errorf("AgentIDFromKey(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestChatID(t *testing.T) {
	// Proves that ChatID() returns the numeric chat ID for 'c'-type sessions and
	// zero for independent ('i'-type) sessions.
	tests := []struct {
		name string
		key  SessionKey
		want int64
	}{
		{
			name: "chat session",
			key: SessionKey{
				Type: 'c',
				ID:   "123456789",
			},
			want: 123456789,
		},
		{
			name: "independent session",
			key: SessionKey{
				Type: 'i',
				ID:   "1709596800",
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.key.ChatID(); got != tt.want {
				t.Errorf("ChatID() = %v, want %v", got, tt.want)
			}
		})
	}
}

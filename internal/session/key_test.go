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
			name: "collision",
			key: SessionKey{
				AgentID:   "main",
				Type:      'c',
				ID:        "123",
				VersionTS: 1709590000,
				ChildType: 'b',
				ChildTS:   1709596800,
				Collision: 1,
			},
			want: "main/c123/1709590000/b1709596800.1",
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
			name:  "collision",
			input: "main/c123/1709590000/b1709596800.1",
			want: SessionKey{
				AgentID:   "main",
				Type:      'c',
				ID:        "123",
				VersionTS: 1709590000,
				ChildType: 'b',
				ChildTS:   1709596800,
				Collision: 1,
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

// TestChatIDFromKey verifies that ChatIDFromKey extracts chat IDs from
// slash-separated session key formats, including branch keys which
// preserve the root chat type.
func TestChatIDFromKey(t *testing.T) {
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
		{
			name: "collision suffix",
			key:  "main/c999/1709590000/b1709596800.2",
			want: 999,
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

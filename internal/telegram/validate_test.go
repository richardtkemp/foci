package telegram

import "testing"

func TestIsValidBotToken(t *testing.T) {
	// Proves the bot token regexp accepts BotFather-shaped tokens (digits:secret)
	// and rejects malformed ones (missing colon, short parts, bad chars).
	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{"valid token", "123456789:AAF-abcdefghijklmnopqrstuvwxyz", true},
		{"valid with underscores", "98765432:abc_DEF-123456789012345678", true},
		{"empty", "", false},
		{"missing colon", "123456789AAFabcdefghijklmnopqrstuvwxyz", false},
		{"short bot id", "1234:AAF-abcdefghijklmnopqrstuvwxyz", false},
		{"short secret", "123456789:short", false},
		{"invalid chars in secret", "123456789:AAF abcdefghijklmnopqrstuvwxyz", false},
		{"non-numeric id", "abcdefghi:AAF-abcdefghijklmnopqrstuvwxyz", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsValidBotToken(tt.token); got != tt.want {
				t.Errorf("IsValidBotToken(%q) = %v, want %v", tt.token, got, tt.want)
			}
		})
	}
}

func TestIsValidUserID(t *testing.T) {
	// Proves the user ID regexp accepts numeric IDs of 3+ digits and rejects
	// short, empty, or non-numeric input.
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"valid id", "12345678", true},
		{"minimum length", "123", true},
		{"too short", "12", false},
		{"empty", "", false},
		{"non-numeric", "12a45678", false},
		{"negative", "-12345678", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsValidUserID(tt.id); got != tt.want {
				t.Errorf("IsValidUserID(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

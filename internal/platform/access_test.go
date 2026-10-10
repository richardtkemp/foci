package platform

import "testing"

// TestUserAllowed proves the single allowlist rule shared by message senders
// and button pressers on every chat transport: a listed user always passes;
// with a non-empty list an unlisted user never passes; an empty (or nil) list
// passes anyone only when allowedOnly is false — lockdown otherwise.
func TestUserAllowed(t *testing.T) {
	listed := map[string]bool{"111": true}

	tests := []struct {
		name        string
		allowed     map[string]bool
		allowedOnly bool
		userID      string
		want        bool
	}{
		{"listed user passes with allowedOnly true", listed, true, "111", true},
		{"listed user passes with allowedOnly false", listed, false, "111", true},
		{"unlisted user blocked when list non-empty and allowedOnly true", listed, true, "999", false},
		{"unlisted user blocked when list non-empty and allowedOnly false", listed, false, "999", false},
		{"empty list with allowedOnly false passes anyone", map[string]bool{}, false, "999", true},
		{"empty list with allowedOnly true passes no one", map[string]bool{}, true, "999", false},
		{"nil map with allowedOnly false behaves like empty", nil, false, "999", true},
		{"nil map with allowedOnly true behaves like empty", nil, true, "999", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UserAllowed(tt.allowed, tt.allowedOnly, tt.userID); got != tt.want {
				t.Errorf("UserAllowed(%v, %v, %q) = %v, want %v", tt.allowed, tt.allowedOnly, tt.userID, got, tt.want)
			}
		})
	}
}

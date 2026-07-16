package ccstream

import "testing"

// TestResolveBinary pins the single shared binary-resolution rule Start and
// CheckReady/queryAuthStatus both depend on. A prior split — readiness.go
// left reading a deprecated cfg key after lifecycle.go migrated off it —
// silently made the L2 harness's stub override invisible to the auth-status
// probe while Start kept working, so both call sites now go through this one
// function instead of independently re-reading cfg["binary"].
func TestResolveBinary(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"unset falls back to claude", map[string]any{}, "claude"},
		{"nil cfg falls back to claude", nil, "claude"},
		{"empty string falls back to claude", map[string]any{"binary": ""}, "claude"},
		{"non-string value falls back to claude", map[string]any{"binary": 42}, "claude"},
		{"set value wins", map[string]any{"binary": "/tmp/fgw/foci-l2-bin/cc-stub"}, "/tmp/fgw/foci-l2-bin/cc-stub"},
		{"deprecated claude_binary key is NOT read", map[string]any{"claude_binary": "/tmp/should-be-ignored"}, "claude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Backend{cfg: tc.cfg}
			if got := b.resolveBinary(); got != tc.want {
				t.Errorf("resolveBinary() = %q, want %q", got, tc.want)
			}
		})
	}
}

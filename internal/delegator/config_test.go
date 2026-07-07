package delegator

import "testing"

// TestSkipPermissions proves the shared accessor treats only a true bool as
// "skip": false, absent, and wrongly-typed values (e.g. the string "true"
// from a mis-quoted TOML entry) all report false, so a config typo fails
// closed — CC still prompts — rather than silently disabling permissions.
func TestSkipPermissions(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		want bool
	}{
		{"true bool", map[string]any{"skip_permissions": true}, true},
		{"false bool", map[string]any{"skip_permissions": false}, false},
		{"absent", map[string]any{}, false},
		{"nil map", nil, false},
		{"string true fails closed", map[string]any{"skip_permissions": "true"}, false},
	}
	for _, tc := range cases {
		if got := SkipPermissions(tc.cfg); got != tc.want {
			t.Errorf("%s: SkipPermissions = %v, want %v", tc.name, got, tc.want)
		}
	}
}

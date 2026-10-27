package ccstream

import (
	"slices"
	"testing"
)

// TestUnattendedPermissionArgs pins the launch flags per session kind: a
// batch session runs in CC's auto mode, an operator's skip_permissions keeps
// --dangerously-skip-permissions, an ordinary session gets neither.
func TestUnattendedPermissionArgs(t *testing.T) {
	cases := []struct {
		name           string
		cfgSkip, batch bool
		want           []string
	}{
		{"ordinary session", false, false, nil},
		{"batch session", false, true, []string{"--permission-mode", "auto"}},
		{"config skip_permissions", true, false, []string{"--dangerously-skip-permissions"}},
		{"config skip_permissions wins over batch", true, true, []string{"--dangerously-skip-permissions"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := unattendedPermissionArgs(c.cfgSkip, c.batch)
			if !slices.Equal(got, c.want) {
				t.Fatalf("unattendedPermissionArgs(%v, %v) = %q, want %q", c.cfgSkip, c.batch, got, c.want)
			}
		})
	}
}

package ccstream

import (
	"slices"
	"testing"

	"foci/internal/delegator"
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
			got := unattendedPermissionArgs(c.cfgSkip, c.batch, delegator.BatchAccessDefault)
			if !slices.Equal(got, c.want) {
				t.Fatalf("unattendedPermissionArgs(%v, %v) = %q, want %q", c.cfgSkip, c.batch, got, c.want)
			}
		})
	}
}

// TestRestrictedSpawnArgs_IndependentOfSkip pins #2684: a restricted spawn
// (raw / explore) whose asks go up to its spawner launches without
// SkipPermissions, and still gets its restricted flags — its access, not its
// attendance, decides them. Parent access and ordinary sessions stay flagless.
func TestRestrictedSpawnArgs_IndependentOfSkip(t *testing.T) {
	for _, access := range []delegator.BatchAccess{delegator.BatchAccessIsolated, delegator.BatchAccessReadOnly} {
		attended := unattendedPermissionArgs(false, false, access)
		unattended := unattendedPermissionArgs(false, true, access)
		if !slices.Equal(attended, restrictedBatchArgs(access)) || !slices.Equal(attended, unattended) {
			t.Errorf("%s: attended args %q, unattended %q; want both the restricted args %q", access, attended, unattended, restrictedBatchArgs(access))
		}
		if !restrictedBatch(access) {
			t.Errorf("%s: not restricted", access)
		}
	}
	if got := unattendedPermissionArgs(false, false, delegator.BatchAccessParent); got != nil {
		t.Errorf("attended parent-access spawn args = %q, want none (the agent's own launch)", got)
	}
	if restrictedBatch(delegator.BatchAccessParent) || restrictedBatch(delegator.BatchAccessDefault) {
		t.Error("parent or default access counted as restricted")
	}
}

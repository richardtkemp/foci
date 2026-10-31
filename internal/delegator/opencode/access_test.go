package opencode

import (
	"testing"

	"foci/internal/delegator"
)

// TestBatchAccessRules_FollowAccessOnly pins #2684: a read-only spawn whose
// asks go up to its spawner (no SkipPermissions) keeps its read-only ruleset —
// the access alone decides it; every other access keeps the agent's rules.
func TestBatchAccessRules_FollowAccessOnly(t *testing.T) {
	if batchAccessRules(delegator.BatchAccessReadOnly) == nil {
		t.Error("read-only access has no ruleset: an attended explore spawn could write")
	}
	for _, a := range []delegator.BatchAccess{delegator.BatchAccessDefault, delegator.BatchAccessParent, delegator.BatchAccessIsolated} {
		if batchAccessRules(a) != nil {
			t.Errorf("%s access got a batch ruleset", a)
		}
	}
}

package session

import "testing"

// Registry completeness (every MetaKey* constant registered, every entry
// backed by a constant, no duplicates) is a static source check, run by
// `make lint` via scripts/find-metadata-key-registry-drift rather than here:
// a test that parsed this package's source keyed its cached result on every
// file's mode and mtime, so each fresh checkout re-ran the package (#2353).

// TestSessionMetadataKeysDisplayOrder pins the /sessions info display contract
// at the registry: the Shown entries, in slice order, must equal the exact
// legacy knownSessionMetadataKeys list (same keys, same order, same unset
// renderings, same branch-only flag) so the command's output is unchanged.
func TestSessionMetadataKeysDisplayOrder(t *testing.T) {
	legacy := []struct {
		key        string
		unset      string
		branchOnly bool
	}{
		{"model", "null", false},
		{"model_endpoint", "null", false},
		{"model_format", "null", false},
		{"effort", "null", false},
		{"permission_mode", "null", false},
		{"cc_resume_id", "null", false},
		{"last_activity", "null", false},
		{"no_compact", "false", false},
		{"display_show_thinking", "false", false},
		{"orientation_consumed", "false", true},
	}
	var shown []SessionMetadataKey
	for _, mk := range SessionMetadataKeys {
		if mk.Shown {
			shown = append(shown, mk)
		}
	}
	if len(shown) != len(legacy) {
		t.Fatalf("registry lists %d Shown keys, legacy display contract has %d", len(shown), len(legacy))
	}
	for i, want := range legacy {
		got := shown[i]
		if got.Key != want.key || got.Unset != want.unset || got.BranchOnly != want.branchOnly {
			t.Errorf("Shown key %d = {%q %q branchOnly=%v}, want {%q %q branchOnly=%v}",
				i, got.Key, got.Unset, got.BranchOnly, want.key, want.unset, want.branchOnly)
		}
	}
}

// TestSessionMetadataKeyValuesStable proves the constants are byte-for-byte
// the key strings persisted before the registry existed, so every converted
// call site keeps writing identical data (no renames, no migration).
func TestSessionMetadataKeyValuesStable(t *testing.T) {
	stable := []struct {
		name string
		got  string
		want string
	}{
		{"MetaKeyModel", MetaKeyModel, "model"},
		{"MetaKeyModelEndpoint", MetaKeyModelEndpoint, "model_endpoint"},
		{"MetaKeyModelFormat", MetaKeyModelFormat, "model_format"},
		{"MetaKeyEffort", MetaKeyEffort, "effort"},
		{"MetaKeyThinking", MetaKeyThinking, "thinking"},
		{"MetaKeySpeed", MetaKeySpeed, "speed"},
		{"MetaKeyShowToolCalls", MetaKeyShowToolCalls, "show_tool_calls"},
		{"MetaKeyDisplayShowThinking", MetaKeyDisplayShowThinking, "display_show_thinking"},
		{"MetaKeyStreamOutput", MetaKeyStreamOutput, "stream_output"},
		{"MetaKeyDisplayWidth", MetaKeyDisplayWidth, "display_width"},
		{"MetaKeyPermissionMode", MetaKeyPermissionMode, "permission_mode"},
		{"MetaKeyNoCompact", MetaKeyNoCompact, "no_compact"},
		{"MetaKeyCCResumeID", MetaKeyCCResumeID, "cc_resume_id"},
		{"MetaKeyCCUndelivered", MetaKeyCCUndelivered, "cc_undelivered"},
		{"MetaKeyOrientationConsumed", MetaKeyOrientationConsumed, "orientation_consumed"},
		{"MetaKeyLastActivity", MetaKeyLastActivity, "last_activity"},
	}
	for _, s := range stable {
		if s.got != s.want {
			t.Errorf("%s = %q, must stay %q (storage format — renaming requires a migration)", s.name, s.got, s.want)
		}
	}
}

package delegator

import "testing"

// TestResolvePermissionMode uses a backend-specific list, never a fixed CC
// numbering scheme, and rejects unknown values without fallback permission.
func TestResolvePermissionMode(t *testing.T) {
	modes := []PermissionMode{{Name: "normal", Value: "user", Aliases: []string{"default"}}, {Name: "auto", Value: "auto_review"}}
	for input, want := range map[string]string{"normal": "user", "DEFAULT": "user", "1": "user", "2": "auto_review", " auto ": "auto_review", "auto_review": "auto_review"} {
		got, ok := ResolvePermissionMode(modes, input)
		if !ok || got.Value != want {
			t.Errorf("%q = %+v/%v, want %q", input, got, ok, want)
		}
	}
	for _, input := range []string{"", "plan", "accept", "4", "bypassPermissions"} {
		if _, ok := ResolvePermissionMode(modes, input); ok {
			t.Errorf("unexpected acceptance: %q", input)
		}
	}
}

// TestSpecValidate_PermissionModes makes mode availability and its backend-owned
// choices one contract, rejecting incomplete or duplicate mode declarations.
func TestSpecValidate_PermissionModes(t *testing.T) {
	s := validSpec()
	s.Prototype = (*specFakeControl)(nil)
	s.Caps[CapControl] = Yes()
	s.Caps[CapControlPermissionMode] = Yes()
	if err := s.Validate(); err == nil {
		t.Fatal("mode control requires declared choices")
	}
	s.PermissionModes = []PermissionMode{{Name: "auto", Value: "auto_review", Description: "automatic review"}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	s.PermissionModes = append(s.PermissionModes, s.PermissionModes[0])
	if err := s.Validate(); err == nil {
		t.Fatal("duplicate mode values accepted")
	}
	s = validSpec()
	s.PermissionModes = []PermissionMode{{Name: "auto", Value: "auto_review", Description: "automatic review"}}
	if err := s.Validate(); err == nil {
		t.Fatal("mode choices without control capability accepted")
	}
}

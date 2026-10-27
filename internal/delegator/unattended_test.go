package delegator

import (
	"strings"
	"testing"
)

// TestUnattended_SessionOrTurn: an ask has nobody to answer it when the whole
// session skips prompts, or when the in-flight turn is unattended; the label
// names which. Between turns (nil) and on an attended turn it is answerable.
func TestUnattended_SessionOrTurn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		skip      bool
		turn      *TurnEvents
		wantLabel string
		wantOK    bool
	}{
		{"skip session", true, nil, UnattendedSkipLabel, true},
		{"skip session wins over turn", true, &TurnEvents{Trigger: "reflection", Unattended: true}, UnattendedSkipLabel, true},
		{"unattended turn", false, &TurnEvents{Trigger: "reflection", Unattended: true}, "reflection", true},
		{"attended turn", false, &TurnEvents{Trigger: "telegram"}, "", false},
		{"between turns", false, nil, "", false},
	} {
		if label, ok := Unattended(tc.skip, tc.turn); label != tc.wantLabel || ok != tc.wantOK {
			t.Errorf("%s: Unattended = (%q, %v), want (%q, %v)", tc.name, label, ok, tc.wantLabel, tc.wantOK)
		}
	}
}

// TestUnattendedDeniedLog_NamesVeto: a veto (a rule matched, the
// substitutability guard overrode it) is reported as such, distinct from no
// rule matching at all.
func TestUnattendedDeniedLog_NamesVeto(t *testing.T) {
	t.Parallel()
	none := UnattendedDeniedLog("a/c1", "reflection", "run: rm x", "")
	if !strings.Contains(none, "no rule permits it") || strings.Contains(none, "vetoed") {
		t.Errorf("no-match line = %q", none)
	}
	veto := UnattendedDeniedLog("a/c1", "reflection", "run: git status", "git is writable")
	if !strings.Contains(veto, "vetoed (git is writable)") {
		t.Errorf("veto line = %q, want it to name the veto", veto)
	}
}

package main

import (
	"testing"

	"foci/internal/delegator"
)

// TestOwnToolIOGap: every registered backend that does not declare tool_io
// Yes gets a startup INFO line from the own-material index, with its level
// and reason (#3187 Q127); Yes gets none; an undeclared one counts as no.
func TestOwnToolIOGap(t *testing.T) {
	for _, s := range delegator.Specs() {
		level, reason, gap := ownToolIOGap(s)
		want := !s.Supports(delegator.CapToolIO)
		if gap != want || (gap && (level == "" || reason == "")) {
			t.Errorf("%s: gap=%v level=%q reason=%q, want gap=%v with a level and reason", s.Name, gap, level, reason, want)
		}
		if s.Support(delegator.CapToolIO).State() == delegator.PartialState && level != "partial" {
			t.Errorf("%s: level %q for a Partial declaration", s.Name, level)
		}
	}
	yes := delegator.Spec{Caps: map[delegator.Capability]delegator.Support{delegator.CapToolIO: delegator.Yes()}}
	if _, _, gap := ownToolIOGap(yes); gap {
		t.Error("a Yes declaration logs a gap")
	}
	no := delegator.Spec{Caps: map[delegator.Capability]delegator.Support{delegator.CapToolIO: delegator.No("none")}}
	if level, reason, gap := ownToolIOGap(no); !gap || level != "no" || reason != "none" {
		t.Errorf("No: %q %q %v", level, reason, gap)
	}
	if level, _, gap := ownToolIOGap(delegator.Spec{}); !gap || level != "no" {
		t.Errorf("undeclared: %q %v", level, gap)
	}
}

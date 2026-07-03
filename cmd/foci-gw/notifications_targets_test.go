package main

import "testing"

// TestSystemInjectionTargets proves the master-agent routing rule for
// agent-less system injections: with a master set, it is the sole target for
// both the changelog and the restart injection; without one, the first agent
// gets the changelog and the restart injection is unrestricted (every agent).
func TestSystemInjectionTargets(t *testing.T) {
	order := []string{"alpha", "beta", "clutch"}

	welcome, restartOnly := systemInjectionTargets("clutch", order)
	if welcome != "clutch" || restartOnly != "clutch" {
		t.Errorf("with master: got (%q, %q), want (clutch, clutch)", welcome, restartOnly)
	}

	welcome, restartOnly = systemInjectionTargets("", order)
	if welcome != "alpha" || restartOnly != "" {
		t.Errorf("without master: got (%q, %q), want (alpha, unrestricted)", welcome, restartOnly)
	}

	welcome, restartOnly = systemInjectionTargets("", nil)
	if welcome != "" || restartOnly != "" {
		t.Errorf("no agents: got (%q, %q), want empty", welcome, restartOnly)
	}
}

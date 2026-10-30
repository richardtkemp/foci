package session

import "testing"

func TestCrossAgentSpawnKey(t *testing.T) {
	t.Parallel()
	root := CrossAgentSpawnRoot("helen")
	if root != "helen/ixspawn" {
		t.Fatalf("root = %q", root)
	}
	for key, want := range map[string]bool{
		root:                     true,
		root + "/b1793349772615": true,
		"helen/ibatch":           false,
		"helen/c123":             false,
		"helen/c123/b456":        false,
		"":                       false,
		"garbage":                false,
	} {
		if got := IsCrossAgentSpawnKey(key); got != want {
			t.Errorf("IsCrossAgentSpawnKey(%q) = %v, want %v", key, got, want)
		}
	}
}

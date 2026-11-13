package secrets

import (
	"reflect"
	"testing"
)

// SetAllowedAgents replaces a denied_agents list (a section with both would
// make the file unloadable), and the rule survives Save and a fresh Load:
// the per-agent view then hides the section from every other agent.
func TestSetAllowedAgents_ReplacesDeniedAndRoundTrips(t *testing.T) {
	path := writeSecrets(t, `
[svc]
key = "v1"
denied_agents = ["bob"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	mustMutate(t, s.SetAllowedAgents("svc", []string{"alice"}))
	mustMutate(t, s.Save())

	re, err := Load(path)
	if err != nil {
		t.Fatalf("reload after Save: %v (both lists written?)", err)
	}
	if got := re.SectionAllowedAgents("svc"); !reflect.DeepEqual(got, []string{"alice"}) {
		t.Errorf("allowed = %v, want [alice]", got)
	}
	if got := re.SectionDeniedAgents("svc"); got != nil {
		t.Errorf("denied = %v, want none", got)
	}
	if _, ok := re.ForAgent("alice").Get("svc.key"); !ok {
		t.Error("alice cannot see svc.key")
	}
	if _, ok := re.ForAgent("carol").Get("svc.key"); ok {
		t.Error("carol sees svc.key despite allowed_agents = [alice]")
	}
}

// SetDeniedAgents replaces allowed_agents; an empty list removes both rules.
func TestSetDeniedAgents_AndClear(t *testing.T) {
	path := writeSecrets(t, `
[svc]
key = "v1"
allowed_agents = ["alice"]
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	mustMutate(t, s.SetDeniedAgents("svc", []string{"bob"}))
	if s.SectionAllowedAgents("svc") != nil || !reflect.DeepEqual(s.SectionDeniedAgents("svc"), []string{"bob"}) {
		t.Fatalf("after deny: allowed=%v denied=%v", s.SectionAllowedAgents("svc"), s.SectionDeniedAgents("svc"))
	}
	mustMutate(t, s.SetAllowedAgents("svc", nil))
	mustMutate(t, s.Save())
	re, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if re.SectionAllowedAgents("svc") != nil || re.SectionDeniedAgents("svc") != nil || re.HasAgentRestrictions() {
		t.Errorf("rules left after clear: allowed=%v denied=%v", re.SectionAllowedAgents("svc"), re.SectionDeniedAgents("svc"))
	}
}

// A per-agent view may not change agent rules.
func TestSetAgentRules_ViewRejected(t *testing.T) {
	s, err := Load(writeSecrets(t, "[svc]\nkey = \"v\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.ForAgent("alice").SetAllowedAgents("svc", []string{"alice"}); err == nil {
		t.Error("view.SetAllowedAgents should be rejected")
	}
	if err := s.ForAgent("alice").SetDeniedAgents("svc", []string{"bob"}); err == nil {
		t.Error("view.SetDeniedAgents should be rejected")
	}
}

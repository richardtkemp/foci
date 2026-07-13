package main

import (
	"os"
	"path/filepath"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
)

// TestLiveApplyCoversHotFields keeps the applier address lists and the `hot`
// struct tags (internal/config/types.go) in lockstep: a hot-tagged registry
// row without an applier would claim live-appliability it doesn't have, and
// an applier for a restart-tagged row would apply silently while the UI says
// "restart required".
func TestLiveApplyCoversHotFields(t *testing.T) {
	covered := map[string]bool{}
	for _, addrs := range [][]string{liveApplyLoggingAddrs, liveApplyDebugAddrs, liveApplyPeriodicAddrs, liveApplyResolvedAddrs} {
		for _, a := range addrs {
			if covered[a] {
				t.Errorf("duplicate applier address %s", a)
			}
			covered[a] = true
		}
	}

	hot := map[string]bool{}
	for _, f := range config.AllFields() {
		if !f.NeedsRestart {
			hot[f.Section+"."+f.Key] = true
		}
	}

	for a := range covered {
		if !hot[a] {
			t.Errorf("applier covers %s but its registry row is not hot-tagged", a)
		}
	}
	for h := range hot {
		if !covered[h] {
			t.Errorf("registry row %s is hot-tagged but has no applier", h)
		}
	}
}

// TestLiveApplyResolvedActuallySwaps proves a hot field read via LiveConfig
// really goes live: register the appliers, run the resolved applier with a
// fresh config, and assert the agent's LiveConfig reflects the new value.
func TestLiveApplyResolvedActuallySwaps(t *testing.T) {
	oldTTS, newTTS := "old", "new"
	base := &config.Config{Agents: []config.AgentConfig{{ID: "a", Voice: config.VoiceConfig{TTS: &oldTTS}}}}
	inst := &agentInstance{id: "a", resolved: config.NewLiveValue(config.Resolve(base, base.Agents[0]))}
	if got := inst.LiveConfig().Voice.TTS; got != oldTTS {
		t.Fatalf("initial LiveConfig().Voice.TTS = %q, want %q", got, oldTTS)
	}

	la := newLiveApply("")
	registerLiveAppliers(la, map[string]*agentInstance{"a": inst})

	fresh := &config.Config{Agents: []config.AgentConfig{{ID: "a", Voice: config.VoiceConfig{TTS: &newTTS}}}}
	applier := la.appliers["voice.tts"]
	if applier == nil {
		t.Fatal("no applier registered for voice.tts")
	}
	if err := applier(fresh); err != nil {
		t.Fatalf("applier: %v", err)
	}

	if got := inst.LiveConfig().Voice.TTS; got != newTTS {
		t.Errorf("after apply, LiveConfig().Voice.TTS = %q, want %q — hot apply did not take effect", got, newTTS)
	}
}

// TestLiveApply_MapSection proves the dynamic-key fallback end to end: an
// Apply() call for a key that was never pre-registered (a user-defined group
// name, here "myteam") still finds and runs the map-section applier via
// liveApply.mapSectionAppliers, which in turn updates BOTH ResolvedAgentConfig
// (Groups/Webhooks, read live by any consumer) and the agent's existing
// *config.GroupResolver in place (a derived handle other components already
// hold a pointer to — see config.GroupResolver.Update's doc).
func TestLiveApply_MapSection(t *testing.T) {
	base := &config.Config{
		Groups: config.GroupsConfig{Groups: map[string]string{"powerful": "anthropic/claude-opus-4-6"}},
		System: config.SystemConfig{Webhooks: map[string]string{"deploy": "old.md"}},
		Agents: []config.AgentConfig{{ID: "a"}},
	}
	resolved := config.Resolve(base, base.Agents[0])
	gr := config.NewGroupResolver(resolved.Groups, base.Models, true)

	inst := &agentInstance{
		id:       "a",
		ag:       &agent.Agent{GroupResolver: gr},
		resolved: config.NewLiveValue(resolved),
	}

	la := newLiveApply("")
	registerLiveAppliers(la, map[string]*agentInstance{"a": inst})

	if r := inst.ag.GroupResolver.ResolveGroup(config.GroupPowerful); r == nil || r.ModelID != "claude-opus-4-6" {
		t.Fatalf("initial ResolveGroup(powerful) = %+v, want claude-opus-4-6", r)
	}

	// Apply() reloads from disk (real edits land there via SetInFile before
	// Apply runs) — write the "fresh" state as a real config file rather than
	// passing a *config.Config in memory.
	configPath := filepath.Join(t.TempDir(), "foci.toml")
	freshTOML := `
[groups]
powerful = "anthropic/claude-sonnet-4-10-20250514"
myteam = "anthropic/claude-haiku-4-5"

[system]
webhooks = { deploy = "new.md", myteam = "unused" }

[[agents]]
id = "a"
`
	if err := os.WriteFile(configPath, []byte(freshTOML), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	la.configPath = configPath

	// "groups.myteam" was never pre-registered — this is the exact key an
	// admin adding a brand-new group name would hit.
	applied, err := la.Apply("groups", "myteam")
	if err != nil {
		t.Fatalf("Apply(groups, myteam): %v", err)
	}
	if !applied {
		t.Fatal("Apply(groups, myteam) returned applied=false — dynamic-key fallback did not fire")
	}

	if r := inst.ag.GroupResolver.ResolveGroup(config.GroupPowerful); r == nil || r.ModelID != "claude-sonnet-4-10-20250514" {
		t.Errorf("after Apply, GroupResolver.ResolveGroup(powerful) = %+v, want claude-sonnet-4-10-20250514 — GroupResolver.Update did not fire", r)
	}
	if r := inst.ag.GroupResolver.ResolveGroup("myteam"); r == nil || r.ModelID != "claude-haiku-4-5" {
		t.Errorf("after Apply, GroupResolver.ResolveGroup(myteam) = %+v, want claude-haiku-4-5 — new group not picked up", r)
	}
	if got := inst.LiveConfig().Webhooks["deploy"]; got != "new.md" {
		t.Errorf("after Apply, LiveConfig().Webhooks[deploy] = %q, want %q", got, "new.md")
	}

	// A completely unrelated section must still miss.
	if applied, _ := la.Apply("nonexistent", "foo"); applied {
		t.Error(`Apply("nonexistent", "foo") = applied, want false`)
	}
}

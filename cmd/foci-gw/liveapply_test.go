package main

import (
	"testing"

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

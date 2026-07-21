package main

import (
	"os"
	"path/filepath"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/nudge"
)

// TestLiveApplyCoversHotFields keeps the applier address lists and the `hot`
// struct tags (internal/config/types.go) in lockstep: a hot-tagged registry
// row without an applier would claim live-appliability it doesn't have, and
// an applier for a restart-tagged row would apply silently while the UI says
// "restart required".
func TestLiveApplyCoversHotFields(t *testing.T) {
	covered := map[string]bool{}
	for _, addrs := range [][]string{liveApplyLoggingAddrs, liveApplyDebugAddrs, liveApplyPeriodicAddrs, liveApplyResolvedAddrs, liveApplyWarningAddrs, liveApplyNudgeAddrs} {
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

// TestLiveApplyPeriodicRefreshesSnapshot proves the periodic applier also
// refreshes the resolved snapshot. reflection.notify_on_skill_creation has two
// consumers — the scheduler handle and the memory-formation sites, which read it
// live via a.reflection() off the snapshot — but a field maps to ONE applier, so
// the periodic applier must swap the snapshot too or a notify-only edit would go
// live for the scheduler but stay stale for the memory sites (#1241).
func TestLiveApplyPeriodicRefreshesSnapshot(t *testing.T) {
	on, off := true, false
	base := &config.Config{Agents: []config.AgentConfig{{ID: "a", Reflection: config.ReflectionConfig{NotifyOnSkillCreation: &on}}}}
	inst := &agentInstance{id: "a", resolved: config.NewLiveValue(config.Resolve(base, base.Agents[0]))}
	if !inst.LiveConfig().Reflection.NotifyOnSkillCreation {
		t.Fatal("initial LiveConfig().Reflection.NotifyOnSkillCreation = false, want true")
	}

	la := newLiveApply("")
	registerLiveAppliers(la, map[string]*agentInstance{"a": inst})

	fresh := &config.Config{Agents: []config.AgentConfig{{ID: "a", Reflection: config.ReflectionConfig{NotifyOnSkillCreation: &off}}}}
	applier := la.appliers["reflection.notify_on_skill_creation"]
	if applier == nil {
		t.Fatal("no applier registered for reflection.notify_on_skill_creation")
	}
	if err := applier(fresh); err != nil {
		t.Fatalf("applier: %v", err)
	}
	if inst.LiveConfig().Reflection.NotifyOnSkillCreation {
		t.Error("after periodic apply, LiveConfig().Reflection.NotifyOnSkillCreation still true — snapshot not refreshed")
	}
}

// TestLiveApplyForceInSessionOverridesGoLive proves the #1450 per-operation
// force_in_session overrides are wired through the SAME resolved-snapshot
// applier as other reflection/keepalive/background/maintenance fields:
// agent.BranchStrategyFor reads them via keepalive()/reflection()/
// backgroundConfig()/maintenance() off LiveConfig(), so an edit applies
// without a restart.
func TestLiveApplyForceInSessionOverridesGoLive(t *testing.T) {
	off := false
	base := &config.Config{Agents: []config.AgentConfig{{ID: "a", Keepalive: config.KeepaliveConfig{ForceInSession: &off}}}}
	inst := &agentInstance{id: "a", resolved: config.NewLiveValue(config.Resolve(base, base.Agents[0]))}
	if inst.LiveConfig().Keepalive.ForceInSession {
		t.Fatal("initial LiveConfig().Keepalive.ForceInSession = true, want false")
	}

	la := newLiveApply("")
	registerLiveAppliers(la, map[string]*agentInstance{"a": inst})

	on := true
	fresh := &config.Config{Agents: []config.AgentConfig{{ID: "a", Keepalive: config.KeepaliveConfig{ForceInSession: &on}}}}
	applier := la.appliers["keepalive.force_in_session"]
	if applier == nil {
		t.Fatal("no applier registered for keepalive.force_in_session")
	}
	if err := applier(fresh); err != nil {
		t.Fatalf("applier: %v", err)
	}

	if !inst.LiveConfig().Keepalive.ForceInSession {
		t.Error("after apply, LiveConfig().Keepalive.ForceInSession = false, want true — hot apply did not take effect")
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

// TestLiveApply_AgentMapSection is TestLiveApply_MapSection's per-agent
// counterpart (#1231): a live "agent.groups.myteam=..." edit dispatches
// through Apply("agent", "groups.myteam") — the "agent" section is a
// separate registration from the global map sections above (see
// registerLiveAppliers' comment) but reuses the same rebuild-and-swap
// applier, since config.Resolve already merges per-agent overrides.
func TestLiveApply_AgentMapSection(t *testing.T) {
	base := &config.Config{
		Groups: config.GroupsConfig{Groups: map[string]string{"powerful": "anthropic/claude-opus-4-6"}},
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

	if r := inst.ag.GroupResolver.ResolveGroup("myteam"); r != nil {
		t.Fatalf("initial ResolveGroup(myteam) = %+v, want nil (not yet defined)", r)
	}

	configPath := filepath.Join(t.TempDir(), "foci.toml")
	freshTOML := `
[groups]
powerful = "anthropic/claude-opus-4-6"

[[agents]]
id = "a"

[agents.groups]
myteam = "anthropic/claude-haiku-4-5"
`
	if err := os.WriteFile(configPath, []byte(freshTOML), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	la.configPath = configPath

	applied, err := la.Apply("agent", "groups.myteam")
	if err != nil {
		t.Fatalf("Apply(agent, groups.myteam): %v", err)
	}
	if !applied {
		t.Fatal("Apply(agent, groups.myteam) returned applied=false — per-agent map fallback did not fire")
	}

	if r := inst.ag.GroupResolver.ResolveGroup("myteam"); r == nil || r.ModelID != "claude-haiku-4-5" {
		t.Errorf("after Apply, GroupResolver.ResolveGroup(myteam) = %+v, want claude-haiku-4-5", r)
	}
	if got := inst.LiveConfig().Groups.Groups["myteam"]; got != "anthropic/claude-haiku-4-5" {
		t.Errorf("after Apply, LiveConfig().Groups.Groups[myteam] = %q", got)
	}
}

// TestLiveApply_WarningQueuesFlipOnLive proves the #1225 core case: an agent
// that started with warning injection off has its always-constructed queues
// enabled live when inject_agent_warnings/inject_chat_warnings are turned on,
// with the chat queue's errors-only filter applied from the fresh level.
func TestLiveApply_WarningQueuesFlipOnLive(t *testing.T) {
	base := &config.Config{
		Platforms: []config.PlatformConfig{{ID: "telegram"}}, // injection off (default)
		Agents:    []config.AgentConfig{{ID: "a"}},
	}
	rc := config.Resolve(base, base.Agents[0])
	ag := &agent.Agent{}
	setupWarningQueue(ag, rc, base)

	if ag.WarningQueue.Enabled() || ag.ChatWarningQueue.Enabled() {
		t.Fatal("queues should start disabled when injection is off")
	}

	inst := &agentInstance{id: "a", ag: ag, resolved: config.NewLiveValue(rc)}
	la := newLiveApply("")
	registerLiveAppliers(la, map[string]*agentInstance{"a": inst})

	applier := la.appliers["notify.inject_agent_warnings"]
	if applier == nil {
		t.Fatal("no applier registered for notify.inject_agent_warnings")
	}

	allLvl, errLvl := config.InjectionAll, config.InjectionErrors
	fresh := &config.Config{
		Platforms: []config.PlatformConfig{{ID: "telegram", Notify: config.NotifyConfig{
			InjectAgentWarnings: &allLvl,
			InjectChatWarnings:  &errLvl,
		}}},
		Agents: []config.AgentConfig{{ID: "a"}},
	}
	if err := applier(fresh); err != nil {
		t.Fatalf("applier: %v", err)
	}

	if !ag.WarningQueue.Enabled() || !ag.ChatWarningQueue.Enabled() {
		t.Fatal("queues should be enabled after live turn-on")
	}

	// Agent queue is level=all: WARN passes.
	ag.WarningQueue.Push("WARN", "config", "noise")
	if ag.WarningQueue.Len() != 1 {
		t.Errorf("agent queue Len() = %d, want 1 (all)", ag.WarningQueue.Len())
	}
	// Chat queue is level=errors: WARN dropped, ERROR kept.
	ag.ChatWarningQueue.Push("WARN", "config", "noise")
	ag.ChatWarningQueue.Push("ERROR", "config", "fatal")
	if ag.ChatWarningQueue.Len() != 1 {
		t.Errorf("chat queue Len() = %d, want 1 (errors-only)", ag.ChatWarningQueue.Len())
	}
}

// TestLiveApplyNudgeReconfigures proves a [defaults.nudge] edit reconfigures the
// live scheduler in place (no rebuild) — here observed via PreAnswerGate (#1228).
func TestLiveApplyNudgeReconfigures(t *testing.T) {
	sched := nudge.NewScheduler(&nudge.RuleSet{Rules: nudge.BraindeadRule()}, 5, 1)
	off, on := false, true
	base := &config.Config{Agents: []config.AgentConfig{{ID: "a", Nudge: config.NudgeConfig{NudgePreAnswerGate: &off}}}}
	sched.Configure(nudgeSettings(config.Resolve(base, base.Agents[0]).Nudge))
	if sched.PreAnswerGate() {
		t.Fatal("initial PreAnswerGate should be false")
	}
	inst := &agentInstance{id: "a", ag: &agent.Agent{Nudger: sched}}

	la := newLiveApply("")
	registerLiveAppliers(la, map[string]*agentInstance{"a": inst})

	fresh := &config.Config{Agents: []config.AgentConfig{{ID: "a", Nudge: config.NudgeConfig{NudgePreAnswerGate: &on}}}}
	applier := la.appliers["nudge.nudge_pre_answer_gate"]
	if applier == nil {
		t.Fatal("no applier registered for nudge.nudge_pre_answer_gate")
	}
	if err := applier(fresh); err != nil {
		t.Fatalf("applier: %v", err)
	}
	if !sched.PreAnswerGate() {
		t.Error("after nudge apply, PreAnswerGate should be true — scheduler not reconfigured")
	}
}

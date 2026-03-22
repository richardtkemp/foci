package config

import (
	"reflect"
	"testing"
)

func TestResolve_AgentOverridesGlobal(t *testing.T) {
	// Proves that per-agent values take priority over global defaults
	// for every standard 2-layer config section.
	cfg := &Config{
		Defaults: DefaultsConfig{
			Loop:     AgentLoopConfig{MaxToolLoops: Ptr(10)},
			Behavior: BehaviorConfig{SteerMode: Ptr(false)},
		},
		Tools: ToolsConfig{
			ToolConfig:    ToolConfig{ExecAutoBackground: Ptr(30)},
			SummaryConfig: SummaryConfig{MaxResultChars: Ptr(5000)},
		},
		Sessions: SessionsConfig{
			CompactionConfig: CompactionConfig{CompactionThreshold: ptrFloat(0.8)},
		},
		Keepalive: KeepaliveConfig{Enabled: Ptr(false)},
		Browser:   BrowserConfig{Headless: Ptr(true)},
	}
	acfg := AgentConfig{
		Loop:      AgentLoopConfig{MaxToolLoops: Ptr(50)},
		Behavior:  BehaviorConfig{SteerMode: Ptr(true)},
		Tools:     AgentToolsOverride{ToolConfig: ToolConfig{ExecAutoBackground: Ptr(60)}},
		Sessions:  AgentSessionsOverride{CompactionConfig: CompactionConfig{CompactionThreshold: ptrFloat(0.5)}},
		Keepalive: KeepaliveConfig{Enabled: Ptr(true)},
		Browser:   BrowserConfig{Headless: Ptr(false)},
	}

	rc := Resolve(cfg, acfg)

	if got := DerefInt(rc.Loop.MaxToolLoops); got != 50 {
		t.Errorf("Loop.MaxToolLoops = %d, want 50 (agent override)", got)
	}
	if got := DerefBool(rc.Behavior.SteerMode); got != true {
		t.Errorf("Behavior.SteerMode = %v, want true (agent override)", got)
	}
	if got := DerefInt(rc.Tools.ExecAutoBackground); got != 60 {
		t.Errorf("Tools.ExecAutoBackground = %d, want 60 (agent override)", got)
	}
	if got := DerefFloat(rc.Compaction.CompactionThreshold); got != 0.5 {
		t.Errorf("Compaction.CompactionThreshold = %v, want 0.5 (agent override)", got)
	}
	if got := DerefBool(rc.Keepalive.Enabled); got != true {
		t.Errorf("Keepalive.Enabled = %v, want true (agent override)", got)
	}
	if got := DerefBool(rc.Browser.Headless); got != false {
		t.Errorf("Browser.Headless = %v, want false (agent override)", got)
	}
}

func TestResolve_FallsBackToGlobal(t *testing.T) {
	// Proves that global defaults are used when per-agent values are nil.
	cfg := &Config{
		Defaults: DefaultsConfig{
			Loop:  AgentLoopConfig{MaxToolLoops: Ptr(10), CacheTTL: Ptr("5m")},
			Voice: VoiceConfig{TTS: Ptr("groq-playai")},
			Nudge: NudgeConfig{NudgeEnable: Ptr(true)},
		},
		Debug: DebugConfig{MessagesInLog: Ptr(true)},
		Mana:  ManaConfig{Name: Ptr("mana")},
	}
	acfg := AgentConfig{
		Loop: AgentLoopConfig{MaxToolLoops: Ptr(50)}, // override only MaxToolLoops
	}

	rc := Resolve(cfg, acfg)

	if got := DerefInt(rc.Loop.MaxToolLoops); got != 50 {
		t.Errorf("Loop.MaxToolLoops = %d, want 50 (agent)", got)
	}
	if got := DerefStr(rc.Loop.CacheTTL); got != "5m" {
		t.Errorf("Loop.CacheTTL = %q, want \"5m\" (global fallback)", got)
	}
	if got := DerefStr(rc.Voice.TTS); got != "groq-playai" {
		t.Errorf("Voice.TTS = %q, want \"groq-playai\" (global fallback)", got)
	}
	if got := DerefBool(rc.Nudge.NudgeEnable); got != true {
		t.Errorf("Nudge.NudgeEnable = %v, want true (global fallback)", got)
	}
	if got := DerefBool(rc.Debug.MessagesInLog); got != true {
		t.Errorf("Debug.MessagesInLog = %v, want true (global fallback)", got)
	}
	if got := DerefStr(rc.Mana.Name); got != "mana" {
		t.Errorf("Mana.Name = %q, want \"mana\" (global fallback)", got)
	}
}

func TestResolve_GroupsMergeMaps(t *testing.T) {
	// Proves Groups.Calls and Groups.Fallbacks use per-key merge (agent
	// overlay on global base) rather than whole-map replacement.
	cfg := &Config{
		Groups: GroupsConfig{
			Powerful: Ptr("claude"),
			Calls:    map[string]string{"search": "fast", "code": "powerful"},
			Fallbacks: map[string]string{"claude": "gpt4"},
		},
	}
	acfg := AgentConfig{
		Groups: GroupsConfig{
			Calls:    map[string]string{"code": "cheap"}, // override "code", keep "search"
			Fallbacks: map[string]string{"gpt4": "llama"}, // add new key
		},
	}

	rc := Resolve(cfg, acfg)

	if got := DerefStr(rc.Groups.Powerful); got != "claude" {
		t.Errorf("Groups.Powerful = %q, want \"claude\" (global)", got)
	}
	// Calls: agent overrides "code", global's "search" remains
	if got := rc.Groups.Calls["code"]; got != "cheap" {
		t.Errorf("Groups.Calls[code] = %q, want \"cheap\" (agent overlay)", got)
	}
	if got := rc.Groups.Calls["search"]; got != "fast" {
		t.Errorf("Groups.Calls[search] = %q, want \"fast\" (global base)", got)
	}
	// Fallbacks: both global and agent keys present
	if got := rc.Groups.Fallbacks["claude"]; got != "gpt4" {
		t.Errorf("Groups.Fallbacks[claude] = %q, want \"gpt4\" (global)", got)
	}
	if got := rc.Groups.Fallbacks["gpt4"]; got != "llama" {
		t.Errorf("Groups.Fallbacks[gpt4] = %q, want \"llama\" (agent)", got)
	}
}

func TestResolve_WebhooksMerge(t *testing.T) {
	// Proves Webhooks uses MergeMaps with global defaults as base and agent
	// as overlay — agent keys override global, global-only keys are kept.
	cfg := &Config{
		Defaults: DefaultsConfig{
			System: SystemConfig{
				Webhooks: map[string]string{"on_start": "/shared/start.md", "on_error": "/shared/error.md"},
			},
		},
	}
	acfg := AgentConfig{
		System: SystemConfig{
			Webhooks: map[string]string{"on_start": "/agent/start.md"}, // override on_start
		},
	}

	rc := Resolve(cfg, acfg)

	if got := rc.Webhooks["on_start"]; got != "/agent/start.md" {
		t.Errorf("Webhooks[on_start] = %q, want \"/agent/start.md\" (agent overlay)", got)
	}
	if got := rc.Webhooks["on_error"]; got != "/shared/error.md" {
		t.Errorf("Webhooks[on_error] = %q, want \"/shared/error.md\" (global base)", got)
	}
}

func TestResolve_AllFieldsPopulated(t *testing.T) {
	// Proves every field of ResolvedAgentConfig is populated when Resolve()
	// is called with non-zero agent and global configs. This catches new
	// fields being added to the struct without updating Resolve().
	cfg := &Config{
		Defaults: DefaultsConfig{
			Loop:     AgentLoopConfig{MaxToolLoops: Ptr(1)},
			Behavior: BehaviorConfig{SteerMode: Ptr(false)},
			Voice:    VoiceConfig{TTS: Ptr("test")},
			Nudge:    NudgeConfig{NudgeEnable: Ptr(false)},
			System:   SystemConfig{SystemFiles: []string{"a.md"}, Webhooks: map[string]string{"hook": "path"}},
		},
		Tools: ToolsConfig{
			ToolConfig:    ToolConfig{ExecAutoBackground: Ptr(1)},
			SummaryConfig: SummaryConfig{MaxResultChars: Ptr(1)},
		},
		Sessions: SessionsConfig{
			CompactionConfig: CompactionConfig{CompactionThreshold: ptrFloat(0.5)},
		},
		Debug:           DebugConfig{MessagesInLog: Ptr(false)},
		Groups:          GroupsConfig{Powerful: Ptr("x")},
		Keepalive:       KeepaliveConfig{Enabled: Ptr(false)},
		Background:      BackgroundConfig{Enabled: Ptr(false)},
		MemoryFormation: MemoryFormationConfig{IntervalEnabled: Ptr(false)},
		Browser:         BrowserConfig{Enabled: Ptr(false)},
		Mana:            ManaConfig{Name: Ptr("m")},
	}
	acfg := AgentConfig{} // all nil — global values should fill in

	rc := Resolve(cfg, acfg)

	rv := reflect.ValueOf(*rc)
	rt := rv.Type()
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
		if f.IsZero() {
			t.Errorf("ResolvedAgentConfig.%s is zero — Resolve() may not be setting it", rt.Field(i).Name)
		}
	}
}

func ptrFloat(v float64) *float64 { return &v }

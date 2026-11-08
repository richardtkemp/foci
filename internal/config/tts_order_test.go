package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestTTSOrder(t *testing.T) {
	entries := []TTSConfig{
		{ID: "a", Priority: 1},
		{ID: "b", Priority: 5},
		{ID: "c", Priority: 1},
		{ID: "d", Priority: -1},
		{ID: "b", Priority: 99}, // a duplicate id keeps its first place
	}
	cases := []struct {
		name   string
		prefer []string
		want   []string
	}{
		{"priority, ties in config order", nil, []string{"b", "a", "c", "d"}},
		{"preferred first, in their order", []string{"d", "c"}, []string{"d", "c", "b", "a"}},
		{"unknown and repeated preferences dropped", []string{"zz", "c", "c"}, []string{"c", "b", "a", "d"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TTSOrder(entries, tc.prefer); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("TTSOrder = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolvedVoice_TTSPreference(t *testing.T) {
	if got := (ResolvedVoice{}).TTSPreference(); len(got) != 0 {
		t.Errorf("empty = %v", got)
	}
	got := ResolvedVoice{TTS: "x", TTSOrder: []string{"y"}}.TTSPreference()
	if !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Errorf("got %v", got)
	}
}

// A per-agent tts_order replaces the global one (Merge: most specific wins).
func TestResolve_TTSOrderPerAgent(t *testing.T) {
	cfg := &Config{Voice: VoiceConfig{TTSOrder: []string{"g"}}}
	if got := Resolve(cfg, AgentConfig{ID: "a"}).Voice.TTSOrder; !reflect.DeepEqual(got, []string{"g"}) {
		t.Errorf("global = %v", got)
	}
	acfg := AgentConfig{ID: "a", Voice: VoiceConfig{TTSOrder: []string{"x", "y"}}}
	if got := Resolve(cfg, acfg).Voice.TTSOrder; !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Errorf("agent = %v", got)
	}
}

// #2974 req 3: a config written for fallback chains gets a load-time message
// naming the order now used; an unknown id in an order is reported.
func TestTTSOrderWarnings(t *testing.T) {
	fb := "neural2"
	cfg := &Config{
		TTS: []TTSConfig{
			{ID: "groq", Fallback: &fb},
			{ID: "neural2"},
		},
		Agents: []AgentConfig{{ID: "a", Voice: VoiceConfig{TTSOrder: []string{"neural2", "gone"}}}},
	}
	w := ttsOrderWarnings(cfg)
	if len(w) != 2 {
		t.Fatalf("warnings = %q, want 2", w)
	}
	if !strings.Contains(w[0], `"groq" fallback`) || !strings.Contains(w[0], "groq, neural2, then the built-in edge-tts") {
		t.Errorf("fallback warning = %q", w[0])
	}
	if !strings.Contains(w[1], `agent "a"`) || !strings.Contains(w[1], `"gone"`) {
		t.Errorf("unknown id warning = %q", w[1])
	}

	clean := &Config{TTS: []TTSConfig{{ID: "groq", Priority: 2}}, Voice: VoiceConfig{TTSOrder: []string{"edge-tts", "groq"}}}
	if w := ttsOrderWarnings(clean); len(w) != 0 {
		t.Errorf("clean config warned: %q", w)
	}
}

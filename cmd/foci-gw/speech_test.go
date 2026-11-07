package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/voice"
)

type stubTTS struct{ name string }

func (s *stubTTS) Synthesize(context.Context, string) ([]byte, error) { return []byte(s.name), nil }

func testSpeechCatalog(tts []config.TTSConfig, keyed map[string]bool, cmds map[string]bool) *speechCatalog {
	ttsMap := map[string]voice.TTS{}
	for _, e := range tts {
		ttsMap[e.ID] = &stubTTS{e.ID}
	}
	return &speechCatalog{
		cfg:     &config.Config{TTS: tts},
		ttsMap:  ttsMap,
		hasKey:  func(e config.TTSConfig) bool { return keyed[e.ID] },
		hasCmd:  func(name string) bool { return cmds[name] },
		builtin: &stubTTS{"builtin"},
	}
}

func TestSpeechCatalog_Sources(t *testing.T) {
	api := func(id string) config.TTSConfig {
		return config.TTSConfig{ID: id, Format: "openai", Endpoint: "https://x.example"}
	}
	cases := []struct {
		name  string
		tts   []config.TTSConfig
		keyed map[string]bool
		cmds  map[string]bool
		want  []voice.SpeechSource
	}{
		{"nothing on the host", nil, nil, nil, nil},
		{"edge-tts alone needs no [[tts]]", nil, nil, map[string]bool{"edge-tts": true},
			[]voice.SpeechSource{{ID: "edge-tts", Kind: "edge"}}},
		{"api then built-in", []config.TTSConfig{api("groq")}, map[string]bool{"groq": true}, map[string]bool{"edge-tts": true},
			[]voice.SpeechSource{{ID: "groq", Kind: "api"}, {ID: "edge-tts", Kind: "edge"}}},
		{"api with no key is left out", []config.TTSConfig{api("groq"), api("oai")}, map[string]bool{"oai": true}, nil,
			[]voice.SpeechSource{{ID: "oai", Kind: "api"}}},
		{"declared edge entry replaces the built-in", []config.TTSConfig{{ID: "edge-tts", Format: "edge-tts", Voice: "v"}}, nil, map[string]bool{"edge-tts": true},
			[]voice.SpeechSource{{ID: "edge-tts", Kind: "edge"}}},
		{"edge entry whose binary is missing", []config.TTSConfig{{ID: "mine", Format: "edge-tts", Command: "my-edge"}}, nil, map[string]bool{"edge-tts": true},
			[]voice.SpeechSource{{ID: "edge-tts", Kind: "edge"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := testSpeechCatalog(c.tts, c.keyed, c.cmds).Sources()
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// Req 6: one voice from start to end. The provider must not be a fallback chain,
// even though every [[tts]] entry falls back to edge-tts in resolveTTS.
func TestSpeechCatalog_ProviderIsOneVoice(t *testing.T) {
	c := testSpeechCatalog([]config.TTSConfig{{ID: "groq", Format: "openai"}}, map[string]bool{"groq": true}, map[string]bool{"edge-tts": true})
	p := c.Provider("a", "groq")
	if p == nil {
		t.Fatal("no provider for an available source")
	}
	if _, chained := p.(*voice.FallbackTTS); chained {
		t.Fatal("source is a fallback chain: a failure would change the voice silently")
	}
	if got, _ := p.Synthesize(context.Background(), "x"); string(got) != "groq" {
		t.Errorf("provider = %q", got)
	}
	if got, _ := c.Provider("a", "edge-tts").Synthesize(context.Background(), "x"); string(got) != "builtin" {
		t.Errorf("built-in = %q", got)
	}
	if c.Provider("a", "gone") != nil {
		t.Error("an unknown source must yield nil")
	}
	c.hasKey = func(config.TTSConfig) bool { return false }
	if c.Provider("a", "groq") != nil {
		t.Error("a source Sources does not list must yield nil")
	}
}

// Req 11: no secret reaches the client, whatever the config holds.
func TestSpeechCatalog_SourcesCarryNoConfigValues(t *testing.T) {
	c := testSpeechCatalog([]config.TTSConfig{{ID: "groq", Format: "openai", Endpoint: "https://secret.example", Secret: "groq.api_key", Model: "m", Voice: "v"}},
		map[string]bool{"groq": true}, nil)
	raw, err := json.Marshal(c.Sources())
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"secret.example", "api_key", `"m"`} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("sources leak %q: %s", leak, raw)
		}
	}
}

func TestSpeechCatalog_AgentVoiceOverGlobal(t *testing.T) {
	global, agent := 1.1, 1.3
	c := &speechCatalog{cfg: &config.Config{
		Voice:  config.VoiceConfig{TTSRate: &global, TTSReplacements: map[string]string{"foci": "foki"}},
		Agents: []config.AgentConfig{{ID: "a", Voice: config.VoiceConfig{TTSRate: &agent, TTSReplacements: map[string]string{"x": "y"}}}},
	}}
	if rate, repls := c.agentVoice("a"); rate != 1.3 || repls["x"] != "y" || repls["foci"] != "foki" {
		t.Errorf("agent: rate %v repls %v", rate, repls)
	}
	if rate, repls := c.agentVoice("other"); rate != 1.1 || repls["foci"] != "foki" {
		t.Errorf("unknown agent: rate %v repls %v", rate, repls)
	}
}

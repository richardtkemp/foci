package main

import (
	"os"
	"path/filepath"
	"testing"

	"foci/internal/config"
	"foci/internal/voice"
)

// ttsTOML is a loadable foci.toml whose [[tts]] block is body.
func ttsTOML(body string) string {
	return `
[groups]
powerful = "anthropic/claude-haiku-4-5"

` + body + `

[[agents]]
id = "a"
`
}

// openaiVoice is the *voice.OpenAITTS behind a build's provider for id.
func openaiVoice(t *testing.T, v *ttsVoices, id string) *voice.OpenAITTS {
	t.Helper()
	p, ok := v.providers[id].(*voice.OpenAITTS)
	if !ok {
		t.Fatalf("provider %q = %T, want *voice.OpenAITTS", id, v.providers[id])
	}
	return p
}

// #2942: a [[tts]] edit reaches the running voices through the live applier at
// the address `foci config set` sends for it, with no restart. A build taken
// before the edit (an in-flight request) keeps its voices.
func TestLiveApply_TTSEditRebuildsVoices(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "foci.toml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(configPath, []byte(ttsTOML(body)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`[[tts]]
id = "groq"
format = "openai"
endpoint = "http://127.0.0.1:1/speech"
voice = "old-voice"
`)
	startup, err := loadGatewayConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	reg := newTTSRegistry(startup, newTestStore(t, nil))
	la := newLiveApply(configPath, nil)
	registerLiveAppliers(la, registryFromMap(map[string]*agentInstance{}))
	registerTTSLiveApplier(la, reg)

	inFlight := reg.Voices()
	if openaiVoice(t, inFlight, "groq").LocalSpeed {
		t.Fatal("startup voice already has local_speed_control")
	}

	// The first live use: local_speed_control on an existing entry, plus a new
	// entry put first (a reorder changes the default voice).
	write(`[[tts]]
id = "oai"
format = "openai"
endpoint = "http://127.0.0.1:1/speech"
voice = "alloy"

[[tts]]
id = "groq"
format = "openai"
endpoint = "http://127.0.0.1:1/speech"
voice = "new-voice"
local_speed_control = true
`)
	p, err := config.ParseKeyPath("tts[groq]")
	if err != nil {
		t.Fatal(err)
	}
	section, key := p.LiveApplyAddress()
	applied, err := la.Apply(section, key)
	if err != nil || !applied {
		t.Fatalf("Apply(%q, %q) = %v, %v; want applied", section, key, applied, err)
	}

	now := reg.Voices()
	groq := openaiVoice(t, now, "groq")
	if !groq.LocalSpeed || groq.Voice != "new-voice" {
		t.Errorf("after apply, groq = LocalSpeed %v voice %q; want true, new-voice", groq.LocalSpeed, groq.Voice)
	}
	if got := chainIDs(t, now.resolve(nil, 0, nil)); len(got) == 0 || got[0] != "oai" {
		t.Errorf("chain = %v, want the new first entry oai first (equal priorities keep config order)", got)
	}
	if old := openaiVoice(t, inFlight, "groq"); old.LocalSpeed || old.Voice != "old-voice" {
		t.Errorf("the build held before the edit changed: LocalSpeed %v voice %q", old.LocalSpeed, old.Voice)
	}

	// A remove: the entry and its provider are gone.
	write(`[[tts]]
id = "oai"
format = "openai"
endpoint = "http://127.0.0.1:1/speech"
`)
	if applied, err := la.Apply("tts", ""); err != nil || !applied {
		t.Fatalf(`Apply("tts", "") = %v, %v`, applied, err)
	}
	if _, ok := reg.Voices().providers["groq"]; ok {
		t.Error("a removed entry still has a provider")
	}
}

// The [voice] HTTP settings are restart-only: a [[tts]] edit must not carry a
// pending hand edit to them into the running voices.
func TestTTSRegistry_KeepsStartupHTTPOpts(t *testing.T) {
	startup := &config.Config{TTS: []config.TTSConfig{{ID: "g", Format: "openai", Endpoint: "http://127.0.0.1:1/x"}}}
	timeout := "7s"
	startup.Voice.HTTPTimeout = &timeout
	reg := newTTSRegistry(startup, newTestStore(t, nil))
	want := openaiVoice(t, reg.Voices(), "g").HTTP

	reg.Configure([]config.TTSConfig{{ID: "g", Format: "openai", Endpoint: "http://127.0.0.1:1/y"}})
	if got := openaiVoice(t, reg.Voices(), "g").HTTP; got != want {
		t.Errorf("HTTP opts after Configure = %+v, want the startup %+v", got, want)
	}
}

// #2942: every per-request path reads the current build. The bot-connection
// adapter is installed even with no voice, reports that through
// voice.Configured, and speaks once a live edit adds a voice.
func TestTTSRegistry_PerRequestPathsFollowEdits(t *testing.T) {
	reg := newTTSRegistry(&config.Config{}, newTestStore(t, nil))
	lazy := &lazyTTS{resolve: func() voice.TTS { return reg.Resolve(nil, 0, nil) }}
	if voice.Configured(lazy) {
		t.Fatal("lazy adapter with no [[tts]] reports a voice")
	}
	c := &speechCatalog{cfg: &config.Config{}, voices: reg.Voices, hasKey: func(config.TTSConfig) bool { return true }, hasCmd: func(string) bool { return false }}
	if got := c.Sources(); len(got) != 0 {
		t.Fatalf("sources with no [[tts]] = %+v", got)
	}

	reg.Configure([]config.TTSConfig{{ID: "groq", Format: "openai", Endpoint: "http://127.0.0.1:1/x"}})
	if !voice.Configured(lazy) {
		t.Error("lazy adapter did not pick up a voice added live")
	}
	if got := c.Sources(); len(got) != 1 || got[0].ID != "groq" {
		t.Errorf("speech sources after the edit = %+v, want groq", got)
	}
	if c.Provider("a", "groq") == nil {
		t.Error("speech provider for a voice added live is nil")
	}

	reg.Configure(nil)
	if voice.Configured(lazy) {
		t.Error("lazy adapter still reports a voice after the last one was removed")
	}
}

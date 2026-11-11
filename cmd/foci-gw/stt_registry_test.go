package main

import (
	"os"
	"path/filepath"
	"testing"

	"foci/internal/config"
	"foci/internal/voice"
)

// sttTOML is a loadable foci.toml whose [[stt]] block is body.
func sttTOML(body string) string {
	return `
[groups]
powerful = "anthropic/claude-haiku-4-5"

` + body + `

[[agents]]
id = "a"
`
}

// credEndpoint is the endpoint a transcriber would call now, "" for none.
func credEndpoint(s voice.STT) string {
	c, _ := voice.CredentialOf(s)
	return c.Endpoint
}

// #3042: a [[stt]] edit reaches the running providers through the live applier
// at the address `foci config set` sends for it, with no restart: the first
// entry turns batch transcription and hands-free voice on, an edit moves the
// next transcription to the new endpoint, and a remove turns both off. A
// provider taken before an edit (an in-flight transcription) keeps its endpoint.
func TestLiveApply_STTEditRebuildsProviders(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "foci.toml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(configPath, []byte(sttTOML(body)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(``)
	startup, err := loadGatewayConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	reg := newSTTRegistry(startup, newTestStore(t, map[string]string{"deepgram.api_key": "d"}))
	la := newLiveApply(configPath, nil)
	registerLiveAppliers(la, registryFromMap(map[string]*agentInstance{}))
	notified := 0
	registerSTTLiveApplier(la, reg, func() { notified++ })

	// What a connection holds from startup: installed with no [[stt]] at all.
	lazy := reg.Live(nil, nil)
	if voice.STTConfigured(lazy) {
		t.Fatal("lazy transcriber with no [[stt]] reports a provider")
	}
	if reg.Stream() != nil {
		t.Fatal("a stream provider with no [[stt]]")
	}

	apply := func(path string) {
		t.Helper()
		p, err := config.ParseKeyPath(path)
		if err != nil {
			t.Fatal(err)
		}
		section, key := p.LiveApplyAddress()
		applied, err := la.Apply(section, key)
		if err != nil || !applied {
			t.Fatalf("Apply(%q, %q) = %v, %v; want applied", section, key, applied, err)
		}
	}

	// Add: a batch entry and a Flux entry, as on the work Mac.
	write(`[[stt]]
id = "groq"
format = "openai"
endpoint = "http://127.0.0.1:1/old"
secret = "x.key"

[[stt]]
id = "flux"
format = "deepgram-flux"
`)
	apply("stt[flux]")
	if notified != 1 {
		t.Errorf("hello notifier ran %d times after an [[stt]] apply, want 1", notified)
	}
	if !voice.STTConfigured(lazy) {
		t.Error("lazy transcriber did not pick up a provider added live")
	}
	if got := credEndpoint(lazy); got != "http://127.0.0.1:1/old" {
		t.Errorf("transcription endpoint = %q, want the added entry's", got)
	}
	if sp := reg.Stream(); sp == nil {
		t.Error("hands-free voice still off after a deepgram-flux entry was added live")
	}
	inFlight := reg.Resolve("", nil)

	// Change: the next transcription uses the new endpoint; the held one does not.
	write(`[[stt]]
id = "groq"
format = "openai"
endpoint = "http://127.0.0.1:1/new"
secret = "x.key"

[[stt]]
id = "flux"
format = "deepgram-flux"
`)
	apply("stt[groq].endpoint")
	if got := credEndpoint(lazy); got != "http://127.0.0.1:1/new" {
		t.Errorf("after the edit, transcription endpoint = %q, want the new one", got)
	}
	if got := inFlight.(*voice.OpenAISTT).Endpoint; got != "http://127.0.0.1:1/old" {
		t.Errorf("the provider held before the edit changed endpoint to %q", got)
	}

	// Remove: both off.
	write(``)
	apply("stt")
	if voice.STTConfigured(lazy) {
		t.Error("lazy transcriber still reports a provider after every [[stt]] entry was removed")
	}
	if reg.Stream() != nil {
		t.Error("hands-free voice still on after the deepgram-flux entry was removed")
	}
}

// #3042: a key added to secrets.toml for an existing [[stt]] entry applies at
// the next use, with no [[stt]] edit: the store re-reads the file, and the
// registry rebuilds when an entry's key differs from the one it built with.
func TestSTTRegistry_SecretAddedLater(t *testing.T) {
	store := newTestStore(t, nil)
	reg := newSTTRegistry(&config.Config{STT: []config.STTConfig{
		{ID: "flux", Format: config.STTFormatDeepgramFlux},
		{ID: "groq", Format: "openai", Endpoint: "http://127.0.0.1:1/x", Secret: "groq.api_key"},
	}}, store)
	if reg.Stream() != nil {
		t.Fatal("stream provider built with no deepgram.api_key")
	}
	held := reg.Resolve("", nil).(*voice.OpenAISTT)

	if err := store.Set("deepgram.api_key", "d"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("groq.api_key", "g"); err != nil {
		t.Fatal(err)
	}
	sp := reg.Stream()
	if sp == nil {
		t.Fatal("hands-free voice still off after deepgram.api_key was added")
	}
	if f, ok := sp.stt.(*voice.FluxSTT); !ok || f.APIKey != "d" {
		t.Errorf("stream provider = %#v, want Flux with the added key", sp.stt)
	}
	if got := reg.Resolve("", nil).(*voice.OpenAISTT).APIKey; got != "g" {
		t.Errorf("batch key after the secret was added = %q, want g", got)
	}
	if held.APIKey != "" {
		t.Errorf("the provider held before the key was added changed key to %q", held.APIKey)
	}
	// An unchanged key does not rebuild.
	if before, after := reg.cur.Load(), reg.current(); before != after {
		t.Error("current rebuilt with no key change")
	}
}

// The [voice] HTTP settings are restart-only: a [[stt]] edit must not carry a
// pending hand edit to them into the running providers.
func TestSTTRegistry_KeepsStartupHTTPOpts(t *testing.T) {
	startup := &config.Config{STT: []config.STTConfig{{ID: "g", Format: "openai", Endpoint: "http://127.0.0.1:1/x"}}}
	timeout := "7s"
	startup.Voice.HTTPTimeout = &timeout
	reg := newSTTRegistry(startup, newTestStore(t, nil))
	want := reg.Resolve("", nil).(*voice.OpenAISTT).HTTP

	reg.Configure([]config.STTConfig{{ID: "g", Format: "openai", Endpoint: "http://127.0.0.1:1/y"}})
	if got := reg.Resolve("", nil).(*voice.OpenAISTT).HTTP; got != want {
		t.Errorf("HTTP opts after Configure = %+v, want the startup %+v", got, want)
	}
}

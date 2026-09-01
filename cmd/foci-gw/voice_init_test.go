package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/secrets"
	"foci/internal/testtemp"
	"foci/internal/voice"
)

// newTestStore creates a secrets store from a key-value map.
// Keys use "section.key" format (e.g. "groq.api_key" → [groq] api_key = "...").
func newTestStore(t *testing.T, vals map[string]string) *secrets.Store {
	t.Helper()
	dir, err := os.MkdirTemp(testtemp.Dir(), "foci-test-secrets-*")
	if err != nil {
		panic(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// Group by section
	sections := map[string][]string{}
	for k, v := range vals {
		parts := strings.SplitN(k, ".", 2)
		if len(parts) != 2 {
			panic("key must be section.key format: " + k)
		}
		sections[parts[0]] = append(sections[parts[0]], fmt.Sprintf("%s = %q", parts[1], v))
	}
	var toml strings.Builder
	for sec, lines := range sections {
		fmt.Fprintf(&toml, "[%s]\n", sec)
		for _, l := range lines {
			toml.WriteString(l + "\n")
		}
		toml.WriteString("\n")
	}
	path := filepath.Join(dir, "secrets.toml")
	if err := os.WriteFile(path, []byte(toml.String()), 0600); err != nil {
		panic(err)
	}
	store, err := secrets.Load(path)
	if err != nil {
		panic(err)
	}
	return store
}

// TestResolveVoiceAPIKey_Explicit verifies that an explicit secret name
// is looked up directly in the secrets store.
func TestResolveVoiceAPIKey_Explicit(t *testing.T) {
	store := newTestStore(t, map[string]string{
		"groq.api_key": "groq-key-123",
	})
	got := resolveVoiceAPIKey(store, "groq.api_key", "https://api.groq.com/v1/audio")
	if got != "groq-key-123" {
		t.Errorf("got %q, want %q", got, "groq-key-123")
	}
}

// TestResolveVoiceAPIKey_HostnameFallback verifies that when no explicit secret
// is given, the hostname prefix is extracted from the endpoint URL and used
// to look up "{prefix}.api_key" in the secrets store.
func TestResolveVoiceAPIKey_HostnameFallback(t *testing.T) {
	store := newTestStore(t, map[string]string{
		"groq.api_key": "groq-key-456",
	})
	got := resolveVoiceAPIKey(store, "", "https://api.groq.com/openai/v1/audio/speech")
	if got != "groq-key-456" {
		t.Errorf("got %q, want %q", got, "groq-key-456")
	}
}

// TestResolveVoiceAPIKey_MissingReturnsEmpty verifies that missing secrets
// return an empty string without error.
func TestResolveVoiceAPIKey_MissingReturnsEmpty(t *testing.T) {
	store := newTestStore(t, map[string]string{})
	got := resolveVoiceAPIKey(store, "nonexistent.key", "")
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

// TestResolveVoiceAPIKey_NoEndpoint verifies that an empty endpoint with no
// explicit secret returns empty.
func TestResolveVoiceAPIKey_NoEndpoint(t *testing.T) {
	store := newTestStore(t, map[string]string{})
	got := resolveVoiceAPIKey(store, "", "")
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

// TestResolveTTS_DefaultFallback verifies that resolveTTS falls back to
// the default ("") entry when the requested ID is not found.
func TestResolveTTS_DefaultFallback(t *testing.T) {
	base := &voice.OpenAITTS{Model: "tts-1"}
	ttsMap := map[string]voice.TTS{
		"":     base,
		"edge": &voice.EdgeTTS{Voice: "test"},
	}
	entries := []config.TTSConfig{{ID: "", Rate: 1.3}}

	result := resolveTTS(ttsMap, entries, "nonexistent", 0, nil)
	if result == nil {
		t.Fatal("expected non-nil TTS from default fallback")
	}
}

// TestResolveTTS_RateComposition verifies that entry rate and agent rate
// are multiplied together (0 treated as 1.0).
func TestResolveTTS_RateComposition(t *testing.T) {
	base := &voice.OpenAITTS{Model: "tts-1"}
	ttsMap := map[string]voice.TTS{
		"fast": base,
	}
	entries := []config.TTSConfig{{ID: "fast", Rate: 1.3}}

	// entry=1.3, agent=1.5 → effective=1.95
	result := resolveTTS(ttsMap, entries, "fast", 1.5, nil)
	oai, ok := result.(*voice.OpenAITTS)
	if !ok {
		t.Fatalf("expected *OpenAITTS, got %T", result)
	}
	// 1.3 * 1.5 = 1.95
	if oai.Speed < 1.94 || oai.Speed > 1.96 {
		t.Errorf("speed = %v, want ~1.95", oai.Speed)
	}
}

// TestResolveTTS_EntryRateOnly verifies that when agentRate is 0 (no override),
// only the entry rate is applied.
func TestResolveTTS_EntryRateOnly(t *testing.T) {
	base := &voice.OpenAITTS{Model: "tts-1"}
	ttsMap := map[string]voice.TTS{"x": base}
	entries := []config.TTSConfig{{ID: "x", Rate: 1.3}}

	result := resolveTTS(ttsMap, entries, "x", 0, nil)
	oai, ok := result.(*voice.OpenAITTS)
	if !ok {
		t.Fatalf("expected *OpenAITTS, got %T", result)
	}
	if oai.Speed < 1.29 || oai.Speed > 1.31 {
		t.Errorf("speed = %v, want ~1.3", oai.Speed)
	}
}

// TestResolveTTS_NilMap verifies that resolveTTS returns nil when given
// an empty map.
func TestResolveTTS_NilMap(t *testing.T) {
	result := resolveTTS(nil, nil, "", 0, nil)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

// TestResolveSTT_DefaultFallback verifies that resolveSTT falls back to
// the default ("") entry when the requested ID is not found.
func TestResolveSTT_DefaultFallback(t *testing.T) {
	base := &voice.OpenAISTT{Model: "whisper-large-v3"}
	sttMap := map[string]voice.STT{
		"": base,
	}

	result := resolveSTT(sttMap, nil, "nonexistent", nil)
	if result != base {
		t.Error("expected default STT fallback")
	}
}

// TestResolveSTT_ExactMatch verifies that resolveSTT returns the exact match
// when the requested ID exists.
func TestResolveSTT_ExactMatch(t *testing.T) {
	base := &voice.OpenAISTT{Model: "whisper-large-v3"}
	other := &voice.OpenAISTT{Model: "whisper-1"}
	sttMap := map[string]voice.STT{
		"":     base,
		"fast": other,
	}

	result := resolveSTT(sttMap, nil, "fast", nil)
	if result != other {
		t.Error("expected exact match, got fallback")
	}
}

// TestResolveSTT_NilMap verifies that resolveSTT returns nil when given
// an empty map.
func TestResolveSTT_NilMap(t *testing.T) {
	result := resolveSTT(nil, nil, "", nil)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

// ptr is a local helper for the *string config fields below.
func ptr[T any](v T) *T { return &v }

// chainIDs returns the ids of a resolved TTS fallback chain, or a single-element
// slice for a bare (unchained) provider.
func chainIDs(t *testing.T, tts voice.TTS) []string {
	t.Helper()
	f, ok := tts.(*voice.FallbackTTS)
	if !ok {
		return []string{"<bare>"}
	}
	ids := make([]string, 0, len(f.Chain))
	for _, e := range f.Chain {
		ids = append(ids, e.ID)
	}
	return ids
}

// TestInitVoice_RegistersBuiltinFallback: a configured cloud provider must gain
// a key-less local provider to fall back to, without it being declared.
func TestInitVoice_RegistersBuiltinFallback(t *testing.T) {
	cfg := &config.Config{TTS: []config.TTSConfig{
		{ID: "groq", Format: "openai", Endpoint: "https://api.groq.com/openai/v1/audio/speech", Model: "m"},
	}}
	ttsMap, _ := initVoice(cfg, newTestStore(t, map[string]string{"groq.api_key": "k"}))

	if _, ok := ttsMap[builtinFallbackTTSID]; !ok {
		t.Fatalf("ttsMap has no %q provider; got keys %v", builtinFallbackTTSID, mapKeys(ttsMap))
	}
	if ttsMap[""] == ttsMap[builtinFallbackTTSID] {
		t.Error("built-in fallback became the default provider; it must only ever be a fallback")
	}
}

// TestInitVoice_NoTTSConfiguredStaysEmpty: graceful text-only degradation (#1439)
// must survive the built-in — a deployment that configured no TTS gets none.
func TestInitVoice_NoTTSConfiguredStaysEmpty(t *testing.T) {
	ttsMap, _ := initVoice(&config.Config{}, newTestStore(t, nil))
	if len(ttsMap) != 0 {
		t.Errorf("ttsMap = %v, want empty when no [[tts]] entries are configured", mapKeys(ttsMap))
	}
}

// TestInitVoice_ConfiguredEdgeEntryWins: declaring the id explicitly must
// replace the built-in, so the edge voice/command stay configurable.
func TestInitVoice_ConfiguredEdgeEntryWins(t *testing.T) {
	cfg := &config.Config{TTS: []config.TTSConfig{
		{ID: builtinFallbackTTSID, Format: "edge-tts", Voice: "en-GB-RyanNeural"},
	}}
	ttsMap, _ := initVoice(cfg, newTestStore(t, nil))

	edge, ok := ttsMap[builtinFallbackTTSID].(*voice.EdgeTTS)
	if !ok {
		t.Fatalf("provider = %T, want *voice.EdgeTTS", ttsMap[builtinFallbackTTSID])
	}
	if edge.Voice != "en-GB-RyanNeural" {
		t.Errorf("voice = %q, want the configured one — the built-in overwrote the [[tts]] entry", edge.Voice)
	}
}

// TestResolveTTS_DefaultsToEdgeFallback is the behaviour Groq's daily token cap
// motivated: an unconfigured cloud entry still ends up with a local backstop.
func TestResolveTTS_DefaultsToEdgeFallback(t *testing.T) {
	ttsMap := map[string]voice.TTS{
		"":                   &voice.OpenAITTS{Model: "tts-1"},
		"groq":               &voice.OpenAITTS{Model: "tts-1"},
		builtinFallbackTTSID: &voice.EdgeTTS{},
	}
	entries := []config.TTSConfig{{ID: "groq", Format: "openai"}}

	got := chainIDs(t, resolveTTS(ttsMap, entries, "groq", 0, nil))
	want := []string{"groq", builtinFallbackTTSID}
	if !equalStrings(got, want) {
		t.Errorf("chain = %v, want %v", got, want)
	}
}

// TestResolveTTS_ExplicitEmptyFallbackDisables: `fallback = ""` must opt out,
// which is why the config field is a pointer rather than a plain string.
func TestResolveTTS_ExplicitEmptyFallbackDisables(t *testing.T) {
	ttsMap := map[string]voice.TTS{
		"groq":               &voice.OpenAITTS{Model: "tts-1"},
		builtinFallbackTTSID: &voice.EdgeTTS{},
	}
	entries := []config.TTSConfig{{ID: "groq", Format: "openai", Fallback: ptr("")}}

	if got := chainIDs(t, resolveTTS(ttsMap, entries, "groq", 0, nil)); !equalStrings(got, []string{"<bare>"}) {
		t.Errorf("chain = %v, want an unchained provider", got)
	}
}

// TestResolveTTS_ExplicitChain: fallback ids compose into a multi-hop chain.
func TestResolveTTS_ExplicitChain(t *testing.T) {
	ttsMap := map[string]voice.TTS{
		"groq":               &voice.OpenAITTS{Model: "a"},
		"openrouter":         &voice.OpenAITTS{Model: "b"},
		builtinFallbackTTSID: &voice.EdgeTTS{},
	}
	entries := []config.TTSConfig{
		{ID: "groq", Format: "openai", Fallback: ptr("openrouter")},
		{ID: "openrouter", Format: "openai"},
	}

	got := chainIDs(t, resolveTTS(ttsMap, entries, "groq", 0, nil))
	want := []string{"groq", "openrouter", builtinFallbackTTSID}
	if !equalStrings(got, want) {
		t.Errorf("chain = %v, want %v", got, want)
	}
}

// TestResolveTTS_EdgeEntryDoesNotChainToItself: an edge-tts entry is already
// the local backstop, so the default fallback must not point it at itself.
func TestResolveTTS_EdgeEntryDoesNotChainToItself(t *testing.T) {
	ttsMap := map[string]voice.TTS{
		"local":              &voice.EdgeTTS{},
		builtinFallbackTTSID: &voice.EdgeTTS{},
	}
	entries := []config.TTSConfig{{ID: "local", Format: "edge-tts"}}

	if got := chainIDs(t, resolveTTS(ttsMap, entries, "local", 0, nil)); !equalStrings(got, []string{"<bare>"}) {
		t.Errorf("chain = %v, want an unchained provider", got)
	}
}

// TestResolveTTS_CycleTerminates: a config that points two entries at each
// other must not loop, and must not repeat a provider in the chain.
func TestResolveTTS_CycleTerminates(t *testing.T) {
	ttsMap := map[string]voice.TTS{
		"a": &voice.OpenAITTS{Model: "a"},
		"b": &voice.OpenAITTS{Model: "b"},
	}
	entries := []config.TTSConfig{
		{ID: "a", Format: "openai", Fallback: ptr("b")},
		{ID: "b", Format: "openai", Fallback: ptr("a")},
	}

	got := chainIDs(t, resolveTTS(ttsMap, entries, "a", 0, nil))
	if !equalStrings(got, []string{"a", "b"}) {
		t.Errorf("chain = %v, want [a b] — the cycle was not broken", got)
	}
}

// TestResolveTTS_ChainLinksKeepTheirOwnRate: each link is decorated with its
// own entry rate, not the head's, so a fallback voice is not left at 1.0.
func TestResolveTTS_ChainLinksKeepTheirOwnRate(t *testing.T) {
	ttsMap := map[string]voice.TTS{
		"groq":               &voice.OpenAITTS{Model: "tts-1"},
		builtinFallbackTTSID: &voice.EdgeTTS{},
	}
	entries := []config.TTSConfig{
		{ID: "groq", Format: "openai", Rate: 1.3},
		{ID: builtinFallbackTTSID, Format: "edge-tts", Rate: 1.1},
	}

	f, ok := resolveTTS(ttsMap, entries, "groq", 2.0, nil).(*voice.FallbackTTS)
	if !ok {
		t.Fatal("expected a *voice.FallbackTTS chain")
	}
	head, ok := f.Chain[0].TTS.(*voice.OpenAITTS)
	if !ok {
		t.Fatalf("head = %T, want *voice.OpenAITTS", f.Chain[0].TTS)
	}
	if head.Speed < 2.59 || head.Speed > 2.61 {
		t.Errorf("head speed = %v, want ~2.6 (1.3 x 2.0)", head.Speed)
	}
	tail, ok := f.Chain[1].TTS.(*voice.EdgeTTS)
	if !ok {
		t.Fatalf("tail = %T, want *voice.EdgeTTS", f.Chain[1].TTS)
	}
	if tail.Rate < 2.19 || tail.Rate > 2.21 {
		t.Errorf("tail rate = %v, want ~2.2 (1.1 x 2.0) — the fallback took the head's rate", tail.Rate)
	}
}

func mapKeys(m map[string]voice.TTS) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

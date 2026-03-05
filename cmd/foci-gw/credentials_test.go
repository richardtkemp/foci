package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/secrets"
	"foci/internal/voice"
)

// newTestStore creates a secrets store from a key-value map.
// Keys use "section.key" format (e.g. "groq.api_key" → [groq] api_key = "...").
func newTestStore(vals map[string]string) *secrets.Store {
	dir, err := os.MkdirTemp("", "foci-test-secrets-*")
	if err != nil {
		panic(err)
	}
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
	store := newTestStore(map[string]string{
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
	store := newTestStore(map[string]string{
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
	store := newTestStore(map[string]string{})
	got := resolveVoiceAPIKey(store, "nonexistent.key", "")
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

// TestResolveVoiceAPIKey_NoEndpoint verifies that an empty endpoint with no
// explicit secret returns empty.
func TestResolveVoiceAPIKey_NoEndpoint(t *testing.T) {
	store := newTestStore(map[string]string{})
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

	result := resolveTTS(ttsMap, entries, "nonexistent", 0)
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
	result := resolveTTS(ttsMap, entries, "fast", 1.5)
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

	result := resolveTTS(ttsMap, entries, "x", 0)
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
	result := resolveTTS(nil, nil, "", 0)
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

	result := resolveSTT(sttMap, "nonexistent")
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

	result := resolveSTT(sttMap, "fast")
	if result != other {
		t.Error("expected exact match, got fallback")
	}
}

// TestResolveSTT_NilMap verifies that resolveSTT returns nil when given
// an empty map.
func TestResolveSTT_NilMap(t *testing.T) {
	result := resolveSTT(nil, "")
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

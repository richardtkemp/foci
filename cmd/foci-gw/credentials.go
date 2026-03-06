package main

import (
	"context"
	"fmt"

	"foci/internal/anthropic"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/secrets"
	"foci/internal/voice"
)

// formatResolvers maps wire format names to custom CredentialResolver implementations.
// Formats without an entry fall back to simple API key resolution.
var formatResolvers = make(map[string]anthropic.CredentialResolver)

// initCredentialResolvers initializes the credential resolver registry.
// Currently registers the anthropic resolver.
func initCredentialResolvers(ctx context.Context, cfg *config.Config, store *secrets.Store) error {
	resolver, err := anthropic.NewResolver(ctx, &cfg.Anthropic, store)
	if err != nil {
		return fmt.Errorf("init anthropic resolver: %w", err)
	}
	formatResolvers["anthropic"] = resolver
	return nil
}

// resolveVoiceAPIKey resolves an API key for a voice provider.
// If explicit is set, it looks up that secret name. Otherwise it derives the
// key name from the endpoint URL hostname via config.HostnameSecretKey.
func resolveVoiceAPIKey(store *secrets.Store, explicit, endpoint string) string {
	if explicit != "" {
		if v, ok := store.Get(explicit); ok {
			return v
		}
		return ""
	}
	key := config.HostnameSecretKey(endpoint)
	if key == "" {
		return ""
	}
	if v, ok := store.Get(key); ok {
		return v
	}
	return ""
}

// initVoice sets up TTS and STT providers from [[tts]] and [[stt]] config arrays.
// Returns maps keyed by entry ID; the first entry is also keyed as "" (default).
func initVoice(cfg *config.Config, store *secrets.Store) (ttsMap map[string]voice.TTS, sttMap map[string]voice.STT) {
	ttsMap = make(map[string]voice.TTS)
	sttMap = make(map[string]voice.STT)

	for i, entry := range cfg.TTS {
		apiKey := resolveVoiceAPIKey(store, entry.Secret, entry.Endpoint)
		t, err := voice.NewTTS(entry, apiKey)
		if err != nil {
			log.Warnf("main", "tts[%d] %q: %v", i, entry.ID, err)
			continue
		}
		ttsMap[entry.ID] = t
		if i == 0 {
			ttsMap[""] = t // default
		}
		log.Infof("main", "TTS %q enabled (format=%s voice=%s)", entry.ID, entry.Format, entry.Voice)
	}

	for i, entry := range cfg.STT {
		apiKey := resolveVoiceAPIKey(store, entry.Secret, entry.Endpoint)
		s, err := voice.NewSTT(entry.Format, entry.Endpoint, apiKey, entry.Model)
		if err != nil {
			log.Warnf("main", "stt[%d] %q: %v", i, entry.ID, err)
			continue
		}
		sttMap[entry.ID] = s
		if i == 0 {
			sttMap[""] = s // default
		}
		log.Infof("main", "STT %q enabled (format=%s model=%s)", entry.ID, entry.Format, entry.Model)
	}

	return ttsMap, sttMap
}

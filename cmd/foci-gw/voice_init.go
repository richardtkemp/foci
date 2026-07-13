package main

import (
	"context"
	"fmt"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/secrets"
	"foci/internal/voice"
)

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

// voiceHTTPOpts resolves the [voice] HTTP timeout + response cap that bound
// STT/TTS calls, falling back to the package defaults when unset/invalid.
func voiceHTTPOpts(cfg *config.Config) voice.HTTPOpts {
	timeout, err := time.ParseDuration(config.DerefStr(cfg.Voice.HTTPTimeout))
	if err != nil || timeout <= 0 {
		timeout, _ = time.ParseDuration(config.DefaultVoiceHTTPTimeout)
	}
	return voice.HTTPOpts{
		Timeout:     timeout,
		MaxResponse: int64(intPtrOr(cfg.Voice.HTTPMaxResponseBytes, config.DefaultVoiceHTTPMaxResponseBytes)),
	}
}

// initVoice sets up TTS and STT providers from [[tts]] and [[stt]] config arrays.
// Returns maps keyed by entry ID; the first entry is also keyed as "" (default).
func initVoice(cfg *config.Config, store *secrets.Store) (ttsMap map[string]voice.TTS, sttMap map[string]voice.STT) {
	ttsMap = make(map[string]voice.TTS)
	sttMap = make(map[string]voice.STT)

	httpOpts := voiceHTTPOpts(cfg)

	for i, entry := range cfg.TTS {
		apiKey := resolveVoiceAPIKey(store, entry.Secret, entry.Endpoint)
		t, err := voice.NewTTS(entry, apiKey, httpOpts)
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
		s, err := voice.NewSTT(entry.Format, entry.Endpoint, apiKey, entry.Model, httpOpts)
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

// resolveTTS looks up a TTS provider by id (empty → default), applies the
// combined rate (entry.Rate × agentRate, 0 treated as 1.0), and wraps with
// merged word replacements (entry → defaults → agent, later wins).
func resolveTTS(ttsMap map[string]voice.TTS, ttsEntries []config.TTSConfig, ttsID string, agentRate float64, replacements map[string]string) voice.TTS {
	baseTTS := ttsMap[ttsID]
	if baseTTS == nil {
		baseTTS = ttsMap[""] // default
	}
	if baseTTS == nil {
		return nil
	}
	// Find entry config.
	var entry *config.TTSConfig
	if ttsID == "" && len(ttsEntries) > 0 {
		entry = &ttsEntries[0]
	} else {
		for i := range ttsEntries {
			if ttsEntries[i].ID == ttsID {
				entry = &ttsEntries[i]
				break
			}
		}
	}
	// Apply rate.
	var entryRate float64
	if entry != nil {
		entryRate = entry.Rate
	}
	eff := entryRate
	if eff == 0 {
		eff = 1.0
	}
	if agentRate != 0 {
		eff *= agentRate
	}
	if eff == 1.0 {
		eff = 0 // WithRate(0) returns the original provider unchanged
	}
	result := voice.WithRate(baseTTS, eff)

	// Merge replacements: entry-level first, then caller's (defaults+agent).
	var entryRepls map[string]string
	if entry != nil {
		entryRepls = entry.Replacements
	}
	merged := voice.MergeReplacements(entryRepls, replacements)
	return voice.WrapTTS(result, merged)
}

// resolveSTT looks up an STT provider by id (empty → default) and wraps with
// merged word replacements (entry → caller, later wins).
func resolveSTT(sttMap map[string]voice.STT, sttEntries []config.STTConfig, sttID string, replacements map[string]string) voice.STT {
	stt := sttMap[sttID]
	if stt == nil {
		stt = sttMap[""] // default
	}
	if stt == nil {
		return stt
	}
	// Find entry replacements.
	var entryRepls map[string]string
	if sttID == "" && len(sttEntries) > 0 {
		entryRepls = sttEntries[0].Replacements
	} else {
		for _, e := range sttEntries {
			if e.ID == sttID {
				entryRepls = e.Replacements
				break
			}
		}
	}
	merged := voice.MergeReplacements(entryRepls, replacements)
	return voice.WrapSTT(stt, merged)
}

// lazySTT re-resolves the underlying voice.STT on every call instead of once
// at setup, so voice.stt changes propagate without a restart (unlike
// resolveSTT's normal callers, which bake the result in at connection setup —
// see setupPlatformConnections's STT/TTS wiring, #1224).
type lazySTT struct {
	resolve func() voice.STT
}

func (l *lazySTT) Transcribe(ctx context.Context, audioData []byte, filename string) (string, error) {
	stt := l.resolve()
	if stt == nil {
		return "", fmt.Errorf("no STT provider configured")
	}
	return stt.Transcribe(ctx, audioData, filename)
}

// lazyTTS re-resolves the underlying voice.TTS on every call instead of once
// at setup, so voice.tts/voice.tts_rate changes propagate without a restart
// for this consumer too (the send_to_chat tool's own TTS already does this
// via the agentTTS closure in agents.go; this covers the bot-connection path).
type lazyTTS struct {
	resolve func() voice.TTS
}

func (l *lazyTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	tts := l.resolve()
	if tts == nil {
		return nil, fmt.Errorf("no TTS provider configured")
	}
	return tts.Synthesize(ctx, text)
}

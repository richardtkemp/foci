package main

import (
	"context"
	"fmt"
	"time"

	"foci/internal/config"
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
			mainLog.Warnf("tts[%d] %q: %v", i, entry.ID, err)
			continue
		}
		ttsMap[entry.ID] = t
		if i == 0 {
			ttsMap[""] = t // default
		}
		mainLog.Infof("TTS %q enabled (format=%s voice=%s)", entry.ID, entry.Format, entry.Voice)
	}

	// Register the built-in edge-tts provider so every configured entry has
	// something to fall back to. It needs no key and no endpoint, so it cannot
	// itself be rate-limited or lose credentials. Declaring a [[tts]] entry
	// with this id replaces it (e.g. to pin a specific edge voice). It is
	// never installed as the default provider (""): a deployment with no
	// [[tts]] entries at all still degrades to text-only, unchanged.
	if len(ttsMap) > 0 {
		if _, ok := ttsMap[builtinFallbackTTSID]; !ok {
			ttsMap[builtinFallbackTTSID] = &voice.EdgeTTS{}
			mainLog.Infof("TTS %q enabled (built-in fallback)", builtinFallbackTTSID)
		}
	}

	for i, entry := range cfg.STT {
		apiKey := resolveVoiceAPIKey(store, entry.Secret, entry.Endpoint)
		s, err := voice.NewSTT(entry.Format, entry.Endpoint, apiKey, entry.Model, httpOpts)
		if err != nil {
			mainLog.Warnf("stt[%d] %q: %v", i, entry.ID, err)
			continue
		}
		sttMap[entry.ID] = s
		if i == 0 {
			sttMap[""] = s // default
		}
		mainLog.Infof("STT %q enabled (format=%s model=%s)", entry.ID, entry.Format, entry.Model)
	}

	return ttsMap, sttMap
}

// builtinFallbackTTSID is the id of the free, key-less edge-tts provider foci
// registers automatically, and the default fallback for every other [[tts]]
// entry.
const builtinFallbackTTSID = "edge-tts"

// maxTTSChain bounds a fallback chain. The visited set below already breaks a
// cycle; this bounds a pathological config that merely chains very deep.
const maxTTSChain = 8

// resolveTTS builds the fallback chain starting at ttsID (empty → default
// entry), applying each link's own rate and replacements combined with the
// caller's agent-level ones. A chain of one is returned bare, so a provider
// with no reachable fallback behaves exactly as it did before chains existed.
func resolveTTS(ttsMap map[string]voice.TTS, ttsEntries []config.TTSConfig, ttsID string, agentRate float64, replacements map[string]string) voice.TTS {
	var chainLinks []voice.ChainEntry
	seen := map[string]bool{}
	id := ttsID
	for len(chainLinks) < maxTTSChain {
		base, entry, key := lookupTTS(ttsMap, ttsEntries, id)
		if base == nil || seen[key] {
			break
		}
		seen[key] = true
		chainLinks = append(chainLinks, voice.ChainEntry{
			ID:  key,
			TTS: decorateTTS(base, entry, agentRate, replacements),
		})
		next := ttsFallbackID(entry)
		if next == "" {
			break
		}
		id = next
	}

	switch len(chainLinks) {
	case 0:
		return nil
	case 1:
		return chainLinks[0].TTS
	default:
		return &voice.FallbackTTS{Chain: chainLinks}
	}
}

// lookupTTS resolves a TTS id to its provider, its config entry (nil when the
// id names no entry), and a stable key identifying the link for cycle
// detection and logging. An unknown id resolves to the default provider — but
// deliberately NOT to the default entry, preserving the pre-chain behaviour
// where an unrecognised agent override got the default provider at no rate.
func lookupTTS(ttsMap map[string]voice.TTS, ttsEntries []config.TTSConfig, id string) (voice.TTS, *config.TTSConfig, string) {
	base, key := ttsMap[id], id
	if base == nil {
		base, key = ttsMap[""], ""
	}
	if base == nil {
		return nil, nil, ""
	}
	if id == "" {
		if len(ttsEntries) > 0 {
			return base, &ttsEntries[0], key
		}
		return base, nil, key
	}
	for i := range ttsEntries {
		if ttsEntries[i].ID == id {
			return base, &ttsEntries[i], key
		}
	}
	return base, nil, key
}

// ttsFallbackID returns the id to try when entry's provider fails: an explicit
// fallback if configured (including "" to disable), otherwise the built-in
// edge-tts provider — except for an entry that already IS edge-tts, which has
// nowhere better to go.
func ttsFallbackID(entry *config.TTSConfig) string {
	if entry != nil && entry.Fallback != nil {
		return *entry.Fallback
	}
	if entry != nil && (entry.ID == builtinFallbackTTSID || entry.Format == "edge-tts") {
		return ""
	}
	return builtinFallbackTTSID
}

// decorateTTS applies a chain link's effective rate (entry.rate × agent rate,
// 0 treated as 1.0) and its merged word replacements (entry → caller, later
// wins).
func decorateTTS(base voice.TTS, entry *config.TTSConfig, agentRate float64, replacements map[string]string) voice.TTS {
	var entryRate float64
	var entryRepls map[string]string
	if entry != nil {
		entryRate, entryRepls = entry.Rate, entry.Replacements
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
	return voice.WrapTTS(voice.WithRate(base, eff), voice.MergeReplacements(entryRepls, replacements))
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

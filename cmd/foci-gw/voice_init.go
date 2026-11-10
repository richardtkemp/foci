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

// initVoice sets up the TTS voices and the STT providers from the [[tts]] and
// [[stt]] config arrays. The STT map is keyed by entry ID, the first entry also
// keyed as "" (default). The TTS voices live in a registry that a [[tts]] edit
// rebuilds (#2942).
func initVoice(cfg *config.Config, store *secrets.Store) (*ttsRegistry, map[string]voice.STT) {
	return newTTSRegistry(cfg, store), initSTT(cfg, store)
}

// buildTTSProviders builds one provider per [[tts]] entry, keyed by entry ID.
// There is no default key: which voice goes first is the priority order
// (resolveTTS, #2974).
func buildTTSProviders(entries []config.TTSConfig, store *secrets.Store, httpOpts voice.HTTPOpts) map[string]voice.TTS {
	ttsMap := make(map[string]voice.TTS)
	for i, entry := range entries {
		apiKey := resolveVoiceAPIKey(store, entry.Secret, entry.Endpoint)
		t, err := voice.NewTTS(entry, apiKey, httpOpts)
		if err != nil {
			mainLog.Warnf("tts[%d] %q: %v", i, entry.ID, err)
			continue
		}
		ttsMap[entry.ID] = t
		mainLog.Infof("TTS %q enabled (format=%s voice=%s)", entry.ID, entry.Format, entry.Voice)
	}

	// Register the built-in edge-tts provider so every chain ends at a voice
	// that needs no key and no endpoint, so it cannot itself be rate-limited
	// or lose credentials. Declaring a [[tts]] entry with this id replaces it
	// (e.g. to pin a specific edge voice, or to give it a priority). A
	// deployment with no [[tts]] entries at all still degrades to text-only.
	if len(ttsMap) > 0 {
		if _, ok := ttsMap[builtinFallbackTTSID]; !ok {
			ttsMap[builtinFallbackTTSID] = &voice.EdgeTTS{}
			mainLog.Infof("TTS %q enabled (built-in fallback)", builtinFallbackTTSID)
		}
	}
	return ttsMap
}

// initSTT builds the batch STT providers from [[stt]]. They are restart-only.
// A streaming entry (deepgram-flux, #3018) is not a batch provider: it is
// left out here and built by initSTTStream, and the default ("") is the first
// batch entry.
func initSTT(cfg *config.Config, store *secrets.Store) map[string]voice.STT {
	sttMap := make(map[string]voice.STT)
	httpOpts := voiceHTTPOpts(cfg)

	first := true
	for i, entry := range cfg.STT {
		if entry.Streaming() {
			continue
		}
		isFirst := first
		first = false
		apiKey := resolveVoiceAPIKey(store, entry.Secret, entry.Endpoint)
		s, err := voice.NewSTT(entry.Format, entry.Endpoint, apiKey, entry.Model, httpOpts)
		if err != nil {
			mainLog.Warnf("stt[%d] %q: %v", i, entry.ID, err)
			continue
		}
		sttMap[entry.ID] = s
		if isFirst {
			sttMap[""] = s // default
		}
		mainLog.Infof("STT %q enabled (format=%s model=%s)", entry.ID, entry.Format, entry.Model)
	}
	return sttMap
}

// sttStreamProvider is the streaming STT for hands-free voice input (#3018):
// the first streaming [[stt]] entry that built. The provider is restart-only
// like every [[stt]] entry; its end-of-turn settings are read from the
// agent's live voice config each time a stream opens.
type sttStreamProvider struct {
	entry config.STTConfig
	stt   voice.StreamingSTT
}

// initSTTStream builds the streaming provider from the first streaming [[stt]]
// entry, or returns nil when there is none or it cannot build (one startup
// warning, e.g. a missing deepgram.api_key).
func initSTTStream(cfg *config.Config, store *secrets.Store) *sttStreamProvider {
	for i, entry := range cfg.STT {
		if !entry.Streaming() {
			continue
		}
		apiKey := resolveVoiceAPIKey(store, entry.Secret, entry.SecretEndpoint())
		s, _, err := voice.NewStreamingSTT(entry.Format, entry.Endpoint, apiKey, entry.Model)
		if err != nil {
			mainLog.Warnf("stt[%d] %q: hands-free voice input off: %v", i, entry.ID, err)
			return nil
		}
		mainLog.Infof("STT %q enabled for hands-free voice input (format=%s)", entry.ID, entry.Format)
		return &sttStreamProvider{entry: entry, stt: s}
	}
	return nil
}

// setup resolves one stream's setup from the agent's live voice config: the
// entry's and the caller's word replacements (later wins, as resolveSTT), and
// the stream_* end-of-turn settings. A nil p has no provider.
func (p *sttStreamProvider) setup(vc config.ResolvedVoice, replacements map[string]string) voice.StreamSetup {
	if p == nil {
		return voice.StreamSetup{}
	}
	return voice.StreamSetup{
		STT:            voice.WrapStreamingSTT(p.stt, voice.MergeReplacements(p.entry.Replacements, replacements)),
		Options:        streamOptions(vc),
		SilenceTimeout: streamSilence(vc),
	}
}

// Flux's documented ranges (https://developers.deepgram.com/docs/flux/configuration).
const (
	streamEOTThresholdMin = 0.5
	streamEOTThresholdMax = 1.0
	streamEOTTimeoutMin   = 500 * time.Millisecond
	streamEOTTimeoutMax   = 60 * time.Second
)

// streamOptions converts the stream_* settings. A value out of the provider's
// range is left out (the provider's default applies) with a warning, rather
// than failing every stream: the app would silently fall back each time.
func streamOptions(vc config.ResolvedVoice) voice.StreamOptions {
	var o voice.StreamOptions
	if t := vc.StreamEOTThreshold; t != 0 {
		if t < streamEOTThresholdMin || t > streamEOTThresholdMax {
			mainLog.Warnf("voice.stream_eot_threshold %v is outside %v-%v: using the provider default", t, streamEOTThresholdMin, streamEOTThresholdMax)
		} else {
			o.EOTThreshold = t
		}
	}
	if s := vc.StreamEOTTimeout; s != "" {
		d, err := time.ParseDuration(s)
		switch {
		case err != nil:
			mainLog.Warnf("voice.stream_eot_timeout %q: %v: using the provider default", s, err)
		case d < streamEOTTimeoutMin || d > streamEOTTimeoutMax:
			mainLog.Warnf("voice.stream_eot_timeout %s is outside %s-%s: using the provider default", d, streamEOTTimeoutMin, streamEOTTimeoutMax)
		default:
			o.EOTTimeoutMs = int(d / time.Millisecond)
		}
	}
	return o
}

// defaultStreamSilence mirrors the stream_silence_timeout default tag, for a
// config built without tag defaults.
const defaultStreamSilence = 15 * time.Second

// streamSilence is the silence close. An unset or unreadable value is the
// default; "0" turns the silence close off (the max duration still applies).
func streamSilence(vc config.ResolvedVoice) time.Duration {
	s := vc.StreamSilenceTimeout
	if s == "" {
		return defaultStreamSilence
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		mainLog.Warnf("voice.stream_silence_timeout %q is not a duration: using %s", s, defaultStreamSilence)
		return defaultStreamSilence
	}
	return d
}

// builtinFallbackTTSID is the id of the free, key-less edge-tts provider foci
// registers automatically. It is the last voice every chain tries, unless a
// [[tts]] entry declares the id and so gives it a priority.
const builtinFallbackTTSID = "edge-tts"

// resolveTTS builds the chain of voices a speech request tries (#2974): the ids
// in prefer first (the agent's voice.tts, then voice.tts_order), then every
// other [[tts]] entry by priority, highest first (config.TTSOrder), then the
// built-in edge-tts if no entry declares it. A voice whose provider did not
// build is left out. Each link has its own entry rate and replacements,
// combined with the caller's agent-level ones. A chain of one is returned as a
// voice.Named, which reports its id like a chain does (#2992).
func resolveTTS(ttsMap map[string]voice.TTS, ttsEntries []config.TTSConfig, prefer []string, agentRate float64, replacements map[string]string) voice.TTS {
	byID := make(map[string]*config.TTSConfig, len(ttsEntries))
	for i := range ttsEntries {
		if _, ok := byID[ttsEntries[i].ID]; !ok {
			byID[ttsEntries[i].ID] = &ttsEntries[i]
		}
	}
	ids := config.TTSOrder(ttsEntries, prefer)
	if _, declared := byID[builtinFallbackTTSID]; !declared {
		ids = append(ids, builtinFallbackTTSID)
	}

	var chainLinks []voice.ChainEntry
	for _, id := range ids {
		base := ttsMap[id]
		if base == nil {
			continue
		}
		chainLinks = append(chainLinks, voice.ChainEntry{
			ID:  id,
			TTS: decorateTTS(base, byID[id], agentRate, replacements),
		})
	}

	switch len(chainLinks) {
	case 0:
		return nil
	case 1:
		// Named, not bare: the app's Message info reports which voice spoke
		// (#2992), and a chain of one must say so too.
		return &voice.Named{ID: chainLinks[0].ID, TTS: chainLinks[0].TTS}
	default:
		return &voice.FallbackTTS{Chain: chainLinks}
	}
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
	// Find entry replacements. The default is the first batch entry: a
	// streaming entry is never the batch default (initSTT).
	var entryRepls map[string]string
	if sttID == "" {
		for _, e := range sttEntries {
			if !e.Streaming() {
				entryRepls = e.Replacements
				break
			}
		}
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

// STTCredential resolves the live provider and delegates, so /app/stt issues a
// snapshot of exactly the provider the NEXT Transcribe would use — including
// after a hot voice.stt change. A resolution that currently has no provider
// yields the zero credential (CredentialOf reports nothing to issue).
func (l *lazySTT) STTCredential() voice.STTCredential {
	cred, _ := voice.CredentialOf(l.resolve())
	return cred
}

// lazyTTS re-resolves the underlying voice.TTS on every call instead of once
// at setup, so voice.tts/voice.tts_rate and [[tts]] changes propagate without
// a restart for this consumer too (the send_to_chat tool's own TTS already
// does this via the agentTTS closure in agents.go; this covers the
// bot-connection path).
//
// A *lazyTTS is never nil, even while no voice is configured, so it reports
// that through Configured: a consumer checks voice.Configured, not `== nil`,
// and stays text-only until a live [[tts]] edit adds a voice (#2942).
type lazyTTS struct {
	resolve func() voice.TTS
}

// Configured reports whether a call made now would reach a provider.
func (l *lazyTTS) Configured() bool { return l.resolve() != nil }

func (l *lazyTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	tts := l.resolve()
	if tts == nil {
		return nil, fmt.Errorf("no TTS provider configured")
	}
	return tts.Synthesize(ctx, text)
}

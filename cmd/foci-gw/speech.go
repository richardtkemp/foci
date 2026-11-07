package main

import (
	"net"
	"net/url"
	"os/exec"
	"strings"

	"foci/internal/config"
	"foci/internal/procx"
	"foci/internal/secrets"
	"foci/internal/voice"
)

// speechCatalog answers what /app/speech can synthesize with (#2858). Each source
// is ONE provider, with no fallback chain: a reading keeps one voice from start
// to end, and the app, not the server, decides what to try next. It covers every
// [[tts]] entry this host can actually run, plus the built-in edge-tts. Unlike
// initVoice's fallback, the built-in is offered with no [[tts]] entries at all
// (Dick, 2026-11-05: "use edge-tts if available on the host").
type speechCatalog struct {
	cfg     *config.Config
	ttsMap  map[string]voice.TTS
	hasKey  func(entry config.TTSConfig) bool
	hasCmd  func(name string) bool
	builtin voice.TTS
}

func newSpeechCatalog(cfg *config.Config, ttsMap map[string]voice.TTS, store *secrets.Store) *speechCatalog {
	return &speechCatalog{
		cfg:    cfg,
		ttsMap: ttsMap,
		hasKey: func(entry config.TTSConfig) bool {
			return resolveVoiceAPIKey(store, entry.Secret, entry.Endpoint) != "" || isLoopbackEndpoint(entry.Endpoint)
		},
		hasCmd:  commandOnTrustedPath,
		builtin: &voice.EdgeTTS{},
	}
}

// commandOnTrustedPath reports whether EdgeTTS could start name: it spawns in the
// Trusted population, whose PATH is what a bare name resolves against.
func commandOnTrustedPath(name string) bool {
	if strings.ContainsRune(name, '/') {
		_, err := exec.LookPath(name) //nolint:forbidigo // a path check only, nothing is spawned
		return err == nil
	}
	for _, dir := range procx.PathDirs(procx.Trusted) {
		if _, err := exec.LookPath(dir + "/" + name); err == nil { //nolint:forbidigo // a path check only, nothing is spawned
			return true
		}
	}
	return false
}

// isLoopbackEndpoint is the one keyless case that can work: a voice on this host.
func isLoopbackEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	if strings.EqualFold(u.Hostname(), "localhost") {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

// available reports whether a [[tts]] entry can run here. A missing key or binary
// hides the source: naming it would only promise a failure.
func (c *speechCatalog) available(entry config.TTSConfig) bool {
	if entry.ID == "" || c.ttsMap[entry.ID] == nil {
		return false
	}
	if entry.Format == "edge-tts" {
		cmd := entry.Command
		if cmd == "" {
			cmd = builtinFallbackTTSID
		}
		return c.hasCmd(cmd)
	}
	return c.hasKey(entry)
}

// Sources lists the voices in config order, the built-in last. It names them
// only: no endpoint, model or key leaves this function.
func (c *speechCatalog) Sources() []voice.SpeechSource {
	var out []voice.SpeechSource
	declaredBuiltin := false
	for _, entry := range c.cfg.TTS {
		declaredBuiltin = declaredBuiltin || entry.ID == builtinFallbackTTSID
		if !c.available(entry) {
			continue
		}
		kind := voice.SpeechKindAPI
		if entry.Format == "edge-tts" {
			kind = voice.SpeechKindEdge
		}
		out = append(out, voice.SpeechSource{ID: entry.ID, Kind: kind, MaxChars: entry.MaxChars})
	}
	if !declaredBuiltin && c.hasCmd(builtinFallbackTTSID) {
		out = append(out, voice.SpeechSource{ID: builtinFallbackTTSID, Kind: voice.SpeechKindEdge})
	}
	return out
}

// Provider returns the one voice for sourceID, with the agent's rate and word
// replacements so a reading says a word as that agent's voice-mode replies do.
// Nil when the source is not one Sources lists.
func (c *speechCatalog) Provider(agentID, sourceID string) voice.TTS {
	rate, repls := c.agentVoice(agentID)
	for i := range c.cfg.TTS {
		entry := &c.cfg.TTS[i]
		if entry.ID != sourceID {
			continue
		}
		if !c.available(*entry) {
			return nil
		}
		return decorateTTS(c.ttsMap[entry.ID], entry, rate, repls)
	}
	if sourceID == builtinFallbackTTSID && c.hasCmd(builtinFallbackTTSID) {
		return decorateTTS(c.builtin, nil, rate, repls)
	}
	return nil
}

// agentVoice is the agent's speech rate and word replacements over the global
// ones. An unknown agent gets the global ones.
func (c *speechCatalog) agentVoice(agentID string) (float64, map[string]string) {
	rate, repls := config.DerefFloat(c.cfg.Voice.TTSRate), c.cfg.Voice.TTSReplacements
	for i := range c.cfg.Agents {
		acfg := &c.cfg.Agents[i]
		if acfg.ID != agentID {
			continue
		}
		if acfg.Voice.TTSRate != nil {
			rate = *acfg.Voice.TTSRate
		}
		return rate, voice.MergeReplacements(repls, acfg.Voice.TTSReplacements)
	}
	return rate, repls
}

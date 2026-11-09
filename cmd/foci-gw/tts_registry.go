package main

import (
	"sync/atomic"

	"foci/internal/config"
	"foci/internal/secrets"
	"foci/internal/voice"
)

// ttsVoices is one build of the [[tts]] voices: the config entries and the
// provider built from each. A build is never changed after it is made. A
// [[tts]] edit makes a new build and swaps it in (ttsRegistry.Configure), so a
// speech request that already holds a build, or a provider resolved from one,
// finishes with the voices it started with (#2942).
type ttsVoices struct {
	entries   []config.TTSConfig
	providers map[string]voice.TTS // keyed by entry id, plus the built-in edge-tts
}

// resolve is resolveTTS over this build. A nil build resolves to nil.
func (v *ttsVoices) resolve(prefer []string, agentRate float64, replacements map[string]string) voice.TTS {
	if v == nil {
		return nil
	}
	return resolveTTS(v.providers, v.entries, prefer, agentRate, replacements)
}

// ttsRegistry holds the current [[tts]] build. Every path that speaks reads it
// once per speech request (Voices or Resolve), never at setup, so a live
// [[tts]] edit reaches the next request with no restart (#2942).
type ttsRegistry struct {
	store *secrets.Store
	// httpOpts is the [voice] HTTP timeout and response cap, read once at
	// startup: those keys are restart-only, so a [[tts]] edit must not carry
	// a pending hand edit to them into the running process.
	httpOpts voice.HTTPOpts
	cur      atomic.Pointer[ttsVoices]
}

// newTTSRegistry builds the startup voices from cfg's [[tts]] entries.
func newTTSRegistry(cfg *config.Config, store *secrets.Store) *ttsRegistry {
	r := &ttsRegistry{store: store, httpOpts: voiceHTTPOpts(cfg)}
	r.Configure(cfg.TTS)
	return r
}

// Configure builds the voices for entries and makes them current. Requests
// already holding the previous build keep it.
func (r *ttsRegistry) Configure(entries []config.TTSConfig) {
	own := append([]config.TTSConfig(nil), entries...)
	r.cur.Store(&ttsVoices{entries: own, providers: buildTTSProviders(own, r.store, r.httpOpts)})
}

// Voices is the current build. Read it once per request and use that build for
// the whole request.
func (r *ttsRegistry) Voices() *ttsVoices {
	if r == nil {
		return nil
	}
	return r.cur.Load()
}

// Resolve is resolveTTS over the current build.
func (r *ttsRegistry) Resolve(prefer []string, agentRate float64, replacements map[string]string) voice.TTS {
	return r.Voices().resolve(prefer, agentRate, replacements)
}

// liveApplyTTSAddrs is the [[tts]] object-list section. Like [[modelinfo]] it
// has no per-field registry rows: `foci config set tts …` and
// `foci config set 'tts[<id>]' …` both apply at the bare section address.
var liveApplyTTSAddrs = []string{"tts"}

// registerTTSLiveApplier rebuilds the voices from the freshly loaded [[tts]]
// entries on a [[tts]] edit: an add, a remove, a reorder or any key of an entry.
// After the rebuild it calls changed (nil = none): the gateway passes
// app.NotifySpeechSourcesChanged, so connected apps get the new voice list
// (hello.caps.speech) at once, not at their next connect (#2945).
func registerTTSLiveApplier(la *liveApply, reg *ttsRegistry, changed func()) {
	la.register(liveApplyTTSAddrs, func(fresh *config.Config) error {
		reg.Configure(fresh.TTS)
		mainLog.Infof("TTS voices rebuilt live: %d [[tts]] entries", len(fresh.TTS))
		if changed != nil {
			changed()
		}
		return nil
	})
}

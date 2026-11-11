package main

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"foci/internal/config"
	"foci/internal/secrets"
	"foci/internal/voice"
)

// sttBuild is one build of the [[stt]] providers: the config entries, the API
// key each entry resolved to, the batch providers and the streaming one. A
// build is never changed after it is made. A [[stt]] edit makes a new build
// and swaps it in (sttRegistry.Configure), so a transcription or a stream that
// already holds a provider from a build finishes on it (#3042).
type sttBuild struct {
	entries []config.STTConfig
	// keys is the API key each entry resolved to when the build was made, in
	// entry order. A different key now (secrets.toml edited) means the build
	// is stale (sttRegistry.current).
	keys   []string
	batch  map[string]voice.STT // keyed by entry id; "" is the first batch entry
	stream *sttStreamProvider   // the first streaming entry that built; nil = none
}

// resolve is resolveSTT over this build. A nil build resolves to nil.
func (b *sttBuild) resolve(sttID string, replacements map[string]string) voice.STT {
	if b == nil {
		return nil
	}
	return resolveSTT(b.batch, b.entries, sttID, replacements)
}

// sttRegistry holds the current [[stt]] build. Every path that transcribes
// reads it once per transcription or per stream, never at setup, so a live
// [[stt]] edit reaches the next upload or stream with no restart (#3042).
type sttRegistry struct {
	store *secrets.Store
	// httpOpts is the [voice] HTTP timeout and response cap, read once at
	// startup: those keys are restart-only, so a [[stt]] edit must not carry
	// a pending hand edit to them into the running process.
	httpOpts voice.HTTPOpts
	mu       sync.Mutex // serialises builds
	cur      atomic.Pointer[sttBuild]
}

// newSTTRegistry builds the startup providers from cfg's [[stt]] entries.
func newSTTRegistry(cfg *config.Config, store *secrets.Store) *sttRegistry {
	r := &sttRegistry{store: store, httpOpts: voiceHTTPOpts(cfg)}
	r.Configure(cfg.STT)
	return r
}

// Configure builds the providers for entries and makes them current.
// Transcriptions and streams already holding a provider keep it.
func (r *sttRegistry) Configure(entries []config.STTConfig) {
	own := append([]config.STTConfig(nil), entries...)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur.Store(r.build(own))
}

// build makes one build. Callers hold mu.
func (r *sttRegistry) build(entries []config.STTConfig) *sttBuild {
	return &sttBuild{
		entries: entries,
		keys:    sttKeys(entries, r.store),
		batch:   buildSTTBatch(entries, r.store, r.httpOpts),
		stream:  buildSTTStream(entries, r.store),
	}
}

// sttKeys is the API key each entry resolves to now, in entry order.
func sttKeys(entries []config.STTConfig, store *secrets.Store) []string {
	keys := make([]string, len(entries))
	for i, e := range entries {
		keys[i] = resolveVoiceAPIKey(store, e.Secret, e.SecretEndpoint())
	}
	return keys
}

// current is the build to use for one transcription or stream. A key added,
// changed or removed in secrets.toml since the build was made (the store
// re-reads the file when it changes) rebuilds it here, so a key edit applies
// at the next use with no [[stt]] edit and no restart. A nil registry has no
// build.
func (r *sttRegistry) current() *sttBuild {
	if r == nil {
		return nil
	}
	b := r.cur.Load()
	if b == nil || slices.Equal(b.keys, sttKeys(b.entries, r.store)) {
		return b
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Another caller, or a Configure, may have made a newer build meanwhile.
	b = r.cur.Load()
	if !slices.Equal(b.keys, sttKeys(b.entries, r.store)) {
		b = r.build(b.entries)
		r.cur.Store(b)
		mainLog.Infof("STT providers rebuilt: an [[stt]] API key changed in secrets")
	}
	return b
}

// Resolve is resolveSTT over the current build: nil when no batch provider
// built.
func (r *sttRegistry) Resolve(sttID string, replacements map[string]string) voice.STT {
	return r.current().resolve(sttID, replacements)
}

// Stream is the current streaming provider: nil when none built.
func (r *sttRegistry) Stream() *sttStreamProvider {
	if b := r.current(); b != nil {
		return b.stream
	}
	return nil
}

// Live is the transcriber a connection holds: it resolves the current build at
// every transcription, so it is installed even with no [[stt]] entry and
// reports whether one is configured through voice.STTConfigured. sttID is
// read per call too (the agent's live voice.stt); nil means the default.
func (r *sttRegistry) Live(sttID func() string, replacements map[string]string) voice.STT {
	return &lazySTT{resolve: func() voice.STT {
		id := ""
		if sttID != nil {
			id = sttID()
		}
		return r.Resolve(id, replacements)
	}}
}

// LiveFor is Live for a fixed [[stt]] id ("" = the default): the platforms'
// ResolveSTT (platform.ProviderDeps).
func (r *sttRegistry) LiveFor(sttID string, replacements map[string]string) voice.STT {
	return r.Live(func() string { return sttID }, replacements)
}

// liveApplySTTAddrs is the [[stt]] object-list section. Like [[tts]] it has no
// per-field registry rows: `foci config set stt …` and
// `foci config set 'stt[<id>]' …` both apply at the bare section address.
var liveApplySTTAddrs = []string{"stt"}

// registerSTTLiveApplier rebuilds the providers from the freshly loaded [[stt]]
// entries on a [[stt]] edit: an add, a remove, a reorder or any key of an entry.
// After the rebuild it calls changed (nil = none): the gateway passes
// app.NotifySpeechSourcesChanged, so connected apps get the new hello
// capabilities ("voice", "sttStream") at once, not at their next connect.
func registerSTTLiveApplier(la *liveApply, reg *sttRegistry, changed func()) {
	la.register(liveApplySTTAddrs, func(fresh *config.Config) error {
		reg.Configure(fresh.STT)
		mainLog.Infof("STT providers rebuilt live: %d [[stt]] entries", len(fresh.STT))
		if changed != nil {
			changed()
		}
		return nil
	})
}

// lazySTT re-resolves the underlying voice.STT on every call instead of once
// at setup, so voice.stt and [[stt]] changes propagate without a restart.
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

// Configured reports whether a transcription now would have a provider
// (voice.STTConfigured).
func (l *lazySTT) Configured() bool { return l.resolve() != nil }

// STTCredential resolves the live provider and delegates, so /app/stt issues a
// snapshot of exactly the provider the NEXT Transcribe would use — including
// after a hot voice.stt or [[stt]] change. A resolution that currently has no
// provider yields the zero credential (CredentialOf reports nothing to issue).
func (l *lazySTT) STTCredential() voice.STTCredential {
	cred, _ := voice.CredentialOf(l.resolve())
	return cred
}

package main

import (
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/voice"
)

// A deepgram-flux [[stt]] entry (#3018) is a streaming provider only: it is
// never in the batch map, and never the batch default even when listed first.
func TestInitSTT_StreamingEntryIsNotBatch(t *testing.T) {
	cfg := &config.Config{STT: []config.STTConfig{
		{ID: "flux", Format: config.STTFormatDeepgramFlux, Replacements: map[string]string{"x": "flux-repl"}},
		{ID: "groq", Format: "openai", Endpoint: "https://api.groq.com/openai/v1/audio/transcriptions", Replacements: map[string]string{"x": "groq-repl"}},
	}}
	store := newTestStore(t, map[string]string{"groq.api_key": "g", "deepgram.api_key": "d"})
	b := newSTTRegistry(cfg, store).current()
	m := b.batch
	if _, ok := m["flux"]; ok {
		t.Error("the flux entry is in the batch map")
	}
	if m[""] == nil || m[""] != m["groq"] {
		t.Errorf("batch default = %v, want the first batch entry (groq)", m[""])
	}
	// The default's replacements are the first batch entry's too.
	r, ok := resolveSTT(m, cfg.STT, "", nil).(*voice.ReplacingSTT)
	if !ok || r.Replacements["x"] != "groq-repl" {
		t.Errorf("default replacements = %+v, want the groq entry's", r)
	}

	sp := b.stream
	if sp == nil {
		t.Fatal("no stream provider from a flux entry with deepgram.api_key")
	}
	f, ok := sp.stt.(*voice.FluxSTT)
	if !ok || f.APIKey != "d" {
		t.Errorf("stream provider = %#v, want Flux with the hostname-convention key", sp.stt)
	}
}

func TestInitSTTStream_NoKeyIsOff(t *testing.T) {
	cfg := &config.Config{STT: []config.STTConfig{{ID: "flux", Format: config.STTFormatDeepgramFlux}}}
	if sp := newSTTRegistry(cfg, newTestStore(t, nil)).Stream(); sp != nil {
		t.Errorf("stream provider built with no key: %+v", sp)
	}
	if sp := newSTTRegistry(&config.Config{}, newTestStore(t, nil)).Stream(); sp != nil {
		t.Error("stream provider built with no streaming entry")
	}
}

// The stream_* settings are read per stream from the resolved voice config;
// out-of-range values fall back to the provider default instead of failing
// every stream.
func TestSTTStreamSetup_Settings(t *testing.T) {
	sp := &sttStreamProvider{stt: &voice.FluxSTT{APIKey: "k"}}
	for _, tc := range []struct {
		name string
		vc   config.ResolvedVoice
		opts voice.StreamOptions
		sil  time.Duration
	}{
		{"unset", config.ResolvedVoice{}, voice.StreamOptions{}, 15 * time.Second},
		{"set", config.ResolvedVoice{StreamEOTThreshold: 0.8, StreamEOTTimeout: "3s", StreamSilenceTimeout: "20s"}, voice.StreamOptions{EOTThreshold: 0.8, EOTTimeoutMs: 3000}, 20 * time.Second},
		{"out of range", config.ResolvedVoice{StreamEOTThreshold: 0.3, StreamEOTTimeout: "90s"}, voice.StreamOptions{}, 15 * time.Second},
		{"silence off", config.ResolvedVoice{StreamSilenceTimeout: "0s"}, voice.StreamOptions{}, 0},
		{"bad silence", config.ResolvedVoice{StreamSilenceTimeout: "soon"}, voice.StreamOptions{}, 15 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sp.setup(tc.vc, nil)
			if s.STT == nil {
				t.Fatal("no STT")
			}
			if s.Options != tc.opts {
				t.Errorf("options = %+v, want %+v", s.Options, tc.opts)
			}
			if s.SilenceTimeout != tc.sil {
				t.Errorf("silence = %s, want %s", s.SilenceTimeout, tc.sil)
			}
		})
	}
	var none *sttStreamProvider
	if none.setup(config.ResolvedVoice{}, nil).STT != nil {
		t.Error("a nil provider resolved a stream STT")
	}
}

// The default tag and the Go fallback agree.
func TestSTTStreamSilenceDefaultMatchesTag(t *testing.T) {
	var cfg config.Config
	config.ApplyTagDefaults(&cfg)
	got := config.DerefStr(cfg.Voice.StreamSilenceTimeout)
	d, err := time.ParseDuration(got)
	if err != nil || d != defaultStreamSilence {
		t.Errorf("stream_silence_timeout default tag = %q, want %s", got, defaultStreamSilence)
	}
}

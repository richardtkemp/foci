package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"foci/internal/anthropic"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/secrets"
	"foci/internal/voice"
)

// tokenHolder is a thread-safe, swappable credential string.
// Used with NewClientWithTokenFunc so that credentials can be hot-reloaded
// (e.g. after `foci auth` saves a new setup-token) without restarting.
type tokenHolder struct {
	mu    sync.RWMutex
	token string
}

func (h *tokenHolder) Get() (string, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.token == "" {
		return "", fmt.Errorf("no credential configured")
	}
	return h.token, nil
}

func (h *tokenHolder) Set(token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.token = token
}

// resolveCredentials resolves the Anthropic API client and usage client.
//
// API client priority: (1) setup-token, (2) API key, (3) Claude Code credentials.
// Usage client: always from CC credentials (polls ~/.claude/.credentials.json).
//
// For static tokens, the client uses a tokenFunc backed by a tokenHolder,
// enabling hot-reload via /-/reload-credentials.
// Returns the tokenHolder (nil for CC-backed client, which polls the file).
func resolveCredentials(cfg *config.Config, store *secrets.Store, ctx context.Context) (*anthropic.Client, *anthropic.UsageClient, *tokenHolder) {
	setupToken, _ := store.Get("anthropic.setup_token")
	apiKey, _ := store.Get("anthropic.api_key")
	httpTimeout, err := time.ParseDuration(cfg.Anthropic.HTTPTimeout)
	if err != nil {
		log.Warnf("main", "invalid anthropic.http_timeout, using default: %v", err)
		httpTimeout = 120 * time.Second
	}
	ccPollInterval, err := time.ParseDuration(cfg.Anthropic.CCCredentialsPollInterval)
	if err != nil {
		log.Warnf("main", "invalid anthropic.cc_credentials_poll_interval, using default: %v", err)
		ccPollInterval = 30 * time.Second
	}

	const ccCredsFile = "~/.claude/.credentials.json"

	// CC token source — shared between usage client and (optionally) main client.
	// Created once; polls the file, never refreshes tokens.
	var ccSrc *anthropic.CCTokenSource
	if src, err := anthropic.NewCCTokenSource(ccCredsFile, ccPollInterval); err == nil {
		src.OnExpired(func() {
			log.Warnf("main", "CC credentials expired — starting claude to refresh")
			go startClaudeForRefresh()
		})
		src.Start(ctx)
		ccSrc = src
		log.Infof("main", "CC token source configured (%s, poll %s)", ccCredsFile, ccPollInterval)
	}

	// Usage client — always from CC credentials (required for /api/oauth/usage).
	var usageClient *anthropic.UsageClient
	if ccSrc != nil {
		usageClient = anthropic.NewUsageClientWithFunc(ccSrc.Token)
		log.Infof("main", "usage client configured (CC credentials)")
	}

	// Source 1: setup-token (from `foci auth` / `claude setup-token`)
	if setupToken != "" {
		log.Infof("main", "using setup-token from secrets.toml")
		holder := &tokenHolder{token: setupToken}
		return anthropic.NewClientWithTokenFunc(holder.Get, httpTimeout),
			usageClient, holder
	}

	// Source 2: Anthropic API key
	if apiKey != "" {
		log.Infof("main", "using API key from secrets.toml")
		holder := &tokenHolder{token: apiKey}
		return anthropic.NewClientWithTokenFunc(holder.Get, httpTimeout),
			usageClient, holder
	}

	// Source 3: Claude Code credentials (passive — poll file, never refresh)
	if ccSrc != nil {
		log.Infof("main", "using CC credentials from %s (passive, poll-based)", ccCredsFile)
		return anthropic.NewClientWithTokenFunc(ccSrc.Token, httpTimeout),
			usageClient, nil
	}

	log.Errorf("main", "no Anthropic token found — run: foci auth")
	os.Exit(1)
	return nil, nil, nil // unreachable
}

// startClaudeForRefresh sends a trivial query via Claude Code to force a
// token refresh. `claude auth status` doesn't actually refresh tokens —
// only a real API call does. Fire-and-forget — logs errors but never blocks.
func startClaudeForRefresh() {
	cmd := exec.Command("claude",
		"--model", "haiku",
		"--system-prompt", "",
		"--print",
		"--effort", "low",
		"1+1",
	)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		log.Warnf("main", "claude token refresh failed (CC may not be installed): %v", err)
	} else {
		log.Infof("main", "claude token refresh completed")
	}
}

// initVoice sets up STT and TTS providers based on config and available API keys.
func initVoice(cfg *config.Config, groqKey, openrouterKey string) (voice.STT, voice.TTS) {
	var sttProvider voice.STT
	var ttsProvider voice.TTS

	// STT: Whisper API (Groq by default, any OpenAI-compatible endpoint)
	sttEndpoint := cfg.Voice.STTEndpoint
	if sttEndpoint == "" {
		sttEndpoint = "https://api.groq.com/openai/v1/audio/transcriptions"
	}
	sttModel := cfg.Voice.STTModel
	if sttModel == "" {
		sttModel = "whisper-large-v3"
	}
	if groqKey != "" {
		sttProvider = &voice.WhisperSTT{
			Endpoint: sttEndpoint,
			APIKey:   groqKey,
			Model:    sttModel,
		}
		log.Infof("main", "voice STT enabled (whisper, %s)", sttModel)
	}

	// TTS: edge-tts (default, free) or openai-compatible API
	ttsProviderName := cfg.Voice.TTSProvider
	if ttsProviderName == "" {
		ttsProviderName = "edge-tts"
	}
	switch ttsProviderName {
	case "edge-tts":
		ttsProvider = &voice.EdgeTTS{
			Voice: cfg.Voice.TTSVoice,
			Rate:  cfg.Voice.TTSRate,
		}
		log.Infof("main", "voice TTS enabled (edge-tts, voice=%s rate=%.2f)", cfg.Voice.TTSVoice, cfg.Voice.TTSRate)
	case "openai":
		ttsEndpoint := cfg.Voice.TTSEndpoint
		if ttsEndpoint == "" {
			ttsEndpoint = "https://openrouter.ai/api/v1/audio/speech"
		}
		ttsModel := cfg.Voice.TTSModel
		if ttsModel == "" {
			ttsModel = "openai/tts-1-mini"
		}
		ttsVoice := cfg.Voice.TTSVoice
		if ttsVoice == "" {
			ttsVoice = "alloy"
		}
		ttsProvider = &voice.OpenAITTS{
			Endpoint: ttsEndpoint,
			APIKey:   openrouterKey,
			Model:    ttsModel,
			Voice:    ttsVoice,
			Speed:    cfg.Voice.TTSRate,
		}
		log.Infof("main", "voice TTS enabled (openai, %s, voice=%s rate=%.2f)", ttsModel, ttsVoice, cfg.Voice.TTSRate)
	default:
		log.Warnf("main", "unknown tts_provider %q, TTS disabled", ttsProviderName)
	}

	return sttProvider, ttsProvider
}

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
		if ttl, err := time.ParseDuration(cfg.Anthropic.UsageCacheTTL); err == nil && ttl > 0 {
			usageClient.SetCacheTTL(ttl)
		}
		log.Infof("main", "usage client configured (CC credentials, cache_ttl=%s)", cfg.Anthropic.UsageCacheTTL)
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

// resolveVoiceAPIKey resolves an API key for a voice provider.
// If explicit is set, it looks up that secret name. Otherwise it extracts
// the hostname prefix from the endpoint URL and tries "{prefix}.api_key".
func resolveVoiceAPIKey(store *secrets.Store, explicit, endpoint string) string {
	if explicit != "" {
		if v, ok := store.Get(explicit); ok {
			return v
		}
		return ""
	}
	if endpoint == "" {
		return ""
	}
	// Extract hostname prefix: "https://api.groq.com/..." → "groq"
	host := endpoint
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	// Strip "api." prefix: "api.groq.com" → "groq.com"
	host = strings.TrimPrefix(host, "api.")
	// Take first segment: "groq.com" → "groq"
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	if host == "" {
		return ""
	}
	key := host + ".api_key"
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

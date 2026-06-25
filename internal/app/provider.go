// Package app is the foci "app" messaging provider: the server end of the
// Foci App Protocol (FAP v1) WebSocket that the native Android client speaks
// (github.com/richardtkemp/foci-android). It is a platform.MessagingProvider
// like telegram/discord, but a connection is the server side of one device's
// WebSocket rather than a vendor-API client.
//
// Slice 1 (this file set) is the "echo slice" from docs/02-foci-server-changes
// §11: provider + hub + appConn for text + streaming + interactive buttons over
// /app/ws, authenticated by a shared key (secret app.api_key). Reliability
// replay, media/blobs, push, and per-device pairing tokens are later slices.
package app

import (
	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/platform"
)

func init() {
	platform.RegisterMessagingProvider("app", &appProvider{})
	agent.RegisterPlatformTrigger("app")
}

// appProvider implements platform.MessagingProvider.
type appProvider struct {
	hub     *Hub
	connMgr platform.ConnectionManager
	deps    platform.ProviderDeps
}

func (p *appProvider) Name() string { return "app" }

// IsConfigured reports whether an [[platforms]] id="app" entry exists. The
// shared API key (secret app.api_key) is resolved at Init; a missing key
// disables the WebSocket endpoint with a logged warning rather than aborting
// startup of the other providers.
func (p *appProvider) IsConfigured(cfg *config.Config) (bool, string) {
	if cfg.Platform("app") == nil {
		return false, "no [[platforms]] entry with id=\"app\""
	}
	return true, ""
}

func (p *appProvider) Init(deps platform.ProviderDeps) error {
	p.deps = deps
	p.hub = newHub(deps)
	p.connMgr = platform.NewConnectionManagerAdapter[*appConn](p.hub)
	setActiveHub(p.hub)
	return nil
}

func (p *appProvider) ConnectionManager() platform.ConnectionManager {
	return p.connMgr
}

func (p *appProvider) SetupAgentConnection(params platform.AgentConnectionParams) *platform.SetupResult {
	conn := p.hub.setupAgent(params)
	if conn == nil {
		return nil
	}
	return &platform.SetupResult{
		DefaultSessionKeyFn: conn.DefaultSessionKeyOrEmpty,
		// Facet support and per-session display overrides are later slices.
	}
}

// The app provider does not (yet) support shared facets, facet restore, or
// per-agent lifecycle callbacks — these are no-ops.
func (p *appProvider) SetupSharedFacet(platform.SharedFacetParams)                  {}
func (p *appProvider) RestoreFacetSessions(platform.RestoreParams)                  {}
func (p *appProvider) SetLifecycleCallback(string, platform.LifecycleEvent, func()) {}

func (p *appProvider) ToolDetailStore() platform.ToolDetailStore { return nil }

func (p *appProvider) AgentPreFlight(string) []string { return nil }

// DefaultPlatformConfig returns the app provider's code defaults. Streaming is
// ON by default — native delta streaming is the whole point of the app.
func (p *appProvider) DefaultPlatformConfig() config.PlatformConfig {
	off := config.ToolCallOff
	thinkOff := config.ShowThinkingOff
	streamOn := true
	startupNotify := false
	pushOn := true
	replayBuf := defaultReplayBufferDepth
	maxBlobMB := defaultMaxBlobMB
	return config.PlatformConfig{
		ID:     "app",
		Notify: config.NotifyConfig{StartupNotify: &startupNotify},
		Display: config.DisplayConfig{
			ShowToolCalls: &off,
			ShowThinking:  &thinkOff,
			StreamOutput:  &streamOn,
		},
		MessageQueueSize: 64,
		App: &config.AppSpecific{
			Push:         &pushOn,
			ReplayBuffer: &replayBuf,
			ReplayTTL:    defaultReplayTTL.String(),
			MaxBlobMB:    &maxBlobMB,
			BlobTTL:      defaultBlobTTL.String(),
			PushCoalesce: defaultPushCoalesce.String(),
			DevicesPath:  defaultDevicesFile,
		},
	}
}

func (p *appProvider) ValidateConfig(config.PlatformConfig) []string { return nil }

func (p *appProvider) Close() error {
	if p.hub != nil {
		return p.hub.Close()
	}
	return nil
}

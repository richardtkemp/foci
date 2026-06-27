// Package app is the foci "app" messaging provider: the server end of the
// Foci App Protocol (FAP v1) WebSocket that the native Android client speaks
// (github.com/richardtkemp/foci-android). It is a platform.MessagingProvider
// like telegram/discord, but a connection is the server side of one device's
// WebSocket rather than a vendor-API client.
//
// Slice 1 (this file set) is the "echo slice" from docs/02-foci-server-changes
// §11: provider + hub + appConn for text + streaming + interactive buttons over
// /app/ws, authenticated by per-device tokens (minted via single-use pairing
// replay, media/blobs, push, and per-device pairing tokens are later slices.
package app

import (
	"context"
	"runtime/debug"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/log"
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
// app provider is enabled by the [[platforms]] entry alone; per-device tokens
// (and single-use pairing keys) are minted at runtime — there is no shared key
// to configure (#862).
func (p *appProvider) IsConfigured(cfg *config.Config) (bool, string) {
	if cfg.Platform("app") == nil {
		return false, "no [[platforms]] entry with id=\"app\""
	}
	return true, ""
}

func (p *appProvider) Init(deps platform.ProviderDeps) (err error) {
	// First-run safety: a bug in newHub (blob dir creation, FCM setup, secret
	// resolution) must disable the app provider, not abort the whole gateway.
	// InitMessaging treats a returned error as fatal (log.Fatalf), so we recover
	// any panic here, leave the provider inert (nil hub, activeHub unset →
	// Enabled() false → endpoints answer 503), and return nil so the other
	// providers (telegram/discord) still start. SetupAgentConnection and
	// ConnectionManager are nil-hub-safe for this degraded state.
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("app", "recovered panic during Init — app provider disabled: %v\n%s", r, debug.Stack())
			p.hub = nil
			p.connMgr = disabledConnMgr{}
		}
	}()
	p.deps = deps
	p.hub = newHub(deps)
	p.connMgr = platform.NewConnectionManagerAdapter[*appConn](p.hub)
	setActiveHub(p.hub)
	return nil
}

func (p *appProvider) ConnectionManager() platform.ConnectionManager {
	if p.connMgr == nil {
		// Init panicked before connMgr was assigned — hand back an inert manager
		// so the aggregator's StartAll/lookup calls can't nil-deref.
		return disabledConnMgr{}
	}
	return p.connMgr
}

func (p *appProvider) SetupAgentConnection(params platform.AgentConnectionParams) *platform.SetupResult {
	if p.hub == nil {
		// Provider failed to initialise (recovered Init panic); no connections.
		return nil
	}
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
			Push:            &pushOn,
			ReplayBuffer:    &replayBuf,
			ReplayTTL:       defaultReplayTTL.String(),
			ReplayStoreTTL:  defaultReplayStoreTTL.String(),
			ReplayStorePath: defaultReplayStoreFile,
			MaxBlobMB:       &maxBlobMB,
			BlobTTL:         defaultBlobTTL.String(),
			PushCoalesce:    defaultPushCoalesce.String(),
			DevicesPath:     defaultDevicesFile,
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

// disabledConnMgr is the ConnectionManager used when the app provider failed to
// initialise (a recovered panic in Init). It is inert: no connections exist, so
// every lookup returns empty and StartAll/Wait are no-ops. This lets a degraded
// app provider sit harmlessly in the active provider set without ever
// nil-dereferencing the (nil) hub the real adapter would delegate to.
type disabledConnMgr struct{}

func (disabledConnMgr) Primary(string) platform.Connection                     { return nil }
func (disabledConnMgr) AllForAgent(string) []platform.Connection               { return nil }
func (disabledConnMgr) ForSession(string) platform.Connection                  { return nil }
func (disabledConnMgr) ForSessionOrPrimary(string, string) platform.Connection { return nil }
func (disabledConnMgr) AcquireFacet(string) (platform.Connection, bool)        { return nil, false }
func (disabledConnMgr) HasFacet(string) bool                                   { return false }
func (disabledConnMgr) StartAll(context.Context)                               {}
func (disabledConnMgr) Wait()                                                  {}

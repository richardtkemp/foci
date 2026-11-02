package slack

import (
	"net/http"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
)

func init() {
	platform.RegisterMessagingProvider(platformName, &slackProvider{})
	agent.RegisterPlatformTrigger(platformName)
}

// slackProvider implements platform.MessagingProvider for Slack. Init is the
// only place the secrets store is visible; a credential problem leaves the
// provider INERT (logged, nil error) rather than aborting the gateway — the
// same rule as the app provider's recovered-panic Init: one bad Slack token
// must not take Telegram down.
type slackProvider struct {
	mgr     *Manager // nil when inert
	connMgr platform.ConnectionManager
	deps    platform.ProviderDeps
}

var _ platform.MessagingProvider = (*slackProvider)(nil)

func (p *slackProvider) Name() string { return platformName }

// IsConfigured reports whether a global [[platforms]] id="slack" entry
// exists — config only. Credentials cannot be checked here (the interface
// sees no secrets); Init does that, through config.SlackProblems, and
// foci check config runs the same oracle against secrets.toml. It deliberately
// does not require a non-empty allowlist: the interim access rule (#2459
// A1, req 7) is "nobody allowed" until part A2 flips the default, and an
// empty list is a valid (if quiet) state.
func (p *slackProvider) IsConfigured(cfg *config.Config) (bool, string) {
	if cfg.Platform(platformName) == nil {
		return false, "no [[platforms]] entry with id=\"slack\""
	}
	return true, ""
}

// Init resolves the tokens from the secrets store and builds the shared
// manager. config.SlackProblems lists every reason the config cannot work;
// each is logged at ERROR naming the secret KEY (never a value), and the
// provider stays inert: ConnectionManager returns nothing, no connection is
// ever set up, StartAll is a no-op. A nil/typed-nil secrets store counts as
// "every secret missing" rather than panicking.
func (p *slackProvider) Init(deps platform.ProviderDeps) error {
	p.deps = deps
	var secrets config.SecretGetter
	if deps.SecretStore != nil {
		secrets = deps.SecretStore
	}
	if problems := config.SlackProblems(deps.Config, secrets); len(problems) > 0 {
		lg := log.NewComponentLogger(platformName)
		for _, problem := range problems {
			lg.Errorf("%s (Slack stays off until it is fixed; the rest of foci starts normally)", problem)
		}
		return nil
	}

	entry := deps.Config.Platform(platformName)
	botToken, _ := secrets.Get(entry.Slack.BotTokenKey())
	appToken, _ := secrets.Get(entry.Slack.AppTokenKey())
	redact := func(s string) string {
		s = strings.ReplaceAll(s, botToken, "[REDACTED]")
		return strings.ReplaceAll(s, appToken, "[REDACTED]")
	}

	// Every outbound Slack call is bounded: the HTTP client's timeout covers
	// all Web API calls AND apps.connections.open (a stalled endpoint must
	// not hold a session worker or the socket read loop forever), and the
	// dial timeout bounds the WebSocket handshake. Both come from config.
	hc := &http.Client{Timeout: entry.Slack.RequestTimeoutDuration()}
	api := newWebAPI(entry.Slack.APIBaseURL(), botToken, hc)
	sock := newSocketClient(entry.Slack.APIBaseURL(), appToken, hc, entry.Slack.ConnectTimeoutDuration(), func() *log.ComponentLogger {
		return log.NewComponentLogger(platformName)
	})
	p.mgr = newManager(entry.Slack, api, sock, deps.SessionIndex, redact)
	p.connMgr = platform.NewConnectionManagerAdapter[*Conn](p.mgr)
	return nil
}

// ConnectionManager is nil-safe on the inert provider: platform's shared
// no-op manager answers every lookup with nothing, so the aggregator's
// StartAll and routing calls cannot nil-dereference a manager that was
// never built.
func (p *slackProvider) ConnectionManager() platform.ConnectionManager {
	if p.connMgr == nil {
		return &platform.NoopConnectionManager{}
	}
	return p.connMgr
}

// SetupAgentConnection registers the agent's Slack state when the router
// names it (default_agent or a mapped channel) and Slack is live. Returns
// nil when this agent gets no Slack connection.
func (p *slackProvider) SetupAgentConnection(params platform.AgentConnectionParams) *platform.SetupResult {
	if p.mgr == nil {
		return nil
	}
	agentID := params.AgentID
	if !p.mgr.router.names(agentID) {
		return nil
	}
	// The inbox target is the agent itself, narrowed to the three methods
	// Slack needs (the app asserts the concrete type because it reads status
	// chips; an interface keeps this package testable with a fake agent).
	ag, ok := params.Handler.(inboxAgent)
	if !ok || ag == nil {
		return nil
	}

	access := config.ResolveAllowedAccess(p.deps.Config, params.AgentConfig, platformName) // static-cfg:ignore: one-time setup bake, mirroring telegram/discord's SetupAgent
	if access.Only && len(access.Users) == 0 {
		log.NewComponentLogger(platformName).Warnf("agent %s: allowed_users is empty and allowed_users_only defaults to true — nobody can talk to this agent on Slack yet (list Slack member IDs, e.g. U012AB3CDE)", agentID)
	}
	accessMap := make(map[string]bool, len(access.Users))
	for _, u := range access.Users {
		accessMap[u] = true
	}
	dc := params.Resolved.PlatformDisplay(platformName) // static-cfg:ignore: one-time setup bake — display settings are restart-required on Slack (no live re-apply wired)
	streamInterval := parseStreamInterval(agentID, dc.StreamInterval)
	core := &agentCore{
		agentID:        agentID,
		api:            p.mgr.api,
		targets:        p.mgr.targets,
		enq:            ag,
		access:         access,
		accessMap:      accessMap,
		streamOutput:   dc.StreamOutput,
		streamInterval: streamInterval,
		injectedHeader: dc.InjectedMessageHeader,
		logger:         func() *log.ComponentLogger { return log.NewComponentLogger(platformName + ":" + agentID) },
	}
	p.mgr.addCore(core)

	// Steer dispatch follows the agent's behavior config, like every
	// interactive transport; the inbox owns per-session workers.
	ag.SetInboxSteerMode(params.Resolved.Behavior.SteerMode) // static-cfg:ignore: initial value; the agent's LiveConfigFn takes over (same as telegram/discord/app)
	ag.StartInbox(p.deps.Ctx)

	return &platform.SetupResult{}
}

// defaultStreamInterval is the chat.update cadence when streaming is enabled
// and display.stream_interval is unset or unparsable (chat.update is Tier 3,
// ~50/min; 2s is well inside it).
const defaultStreamInterval = 2 * time.Second

// parseStreamInterval resolves the configured stream buffer interval for one
// agent, falling back to the default (with a WARN naming the bad value) so a
// typo can never zero out the cadence — the same lenient-parse shape
// telegram's ApplyAgentDisplaySettings uses.
func parseStreamInterval(agentID, raw string) time.Duration {
	if raw == "" {
		return defaultStreamInterval
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	log.NewComponentLogger(platformName).Warnf("agent %s: display.stream_interval = %q is not a positive duration; using the default %s", agentID, raw, defaultStreamInterval)
	return defaultStreamInterval
}

// SetLifecycleCallback stores the OnUserMessage hook on the agent's core
// (fired on each accepted inbound message; the periodic runner's receipt
// stamp).
func (p *slackProvider) SetLifecycleCallback(agentID string, event platform.LifecycleEvent, fn func()) {
	if p.mgr == nil {
		return
	}
	if event != platform.OnUserMessage {
		return
	}
	if core := p.mgr.core(agentID); core != nil {
		core.onUserMessage = fn
	}
}

func (p *slackProvider) SetupSharedFacet(platform.SharedFacetParams) {} // no facets on Slack
func (p *slackProvider) RestoreFacetSessions(platform.RestoreParams) {}
func (p *slackProvider) ToolDetailStore() platform.ToolDetailStore   { return nil }
func (p *slackProvider) AgentPreFlight(string) []string              { return nil }

// NeverDefaultChat implements platform.NeverDefaultProvider: Slack
// conversations are never default chats (#2459 req 6) — the aggregating
// connection manager orders the provider last in its Primary fallback.
func (p *slackProvider) NeverDefaultChat() bool { return true }

// ConfinesSessionReplies implements platform.ReplyConfiningProvider: every
// Slack conversation belongs to one user or one thread's readers, so the
// platform declares at provider level the confinement its connections carry
// (Conn.ConfinesReplies). The declaration, not any live connection, is what
// the aggregating manager answers for a session with no connection, so a
// Slack session's replies are never fanned out to other platforms' chats
// (#2459 req 5).
func (p *slackProvider) ConfinesSessionReplies() bool { return true }

// DefaultPlatformConfig: streaming is off by default (chat.update is Tier 3)
// and no startup notice (there is no destination an agent-wide notice may
// reach on Slack). The stream-interval default is derived from
// defaultStreamInterval — the one cadence constant, shared with the
// unparsable-value fallback in parseStreamInterval.
func (p *slackProvider) DefaultPlatformConfig() config.PlatformConfig {
	off := config.ToolCallOff
	thinkOff := config.ShowThinkingOff
	so := false
	notify := false
	return config.PlatformConfig{
		ID:     platformName,
		Notify: config.NotifyConfig{StartupNotify: &notify},
		Display: config.DisplayConfig{
			ShowToolCalls:  &off,
			ShowThinking:   &thinkOff,
			StreamOutput:   &so,
			StreamInterval: config.Ptr(defaultStreamInterval.String()),
		},
	}
}

func (p *slackProvider) ValidateConfig(config.PlatformConfig) []string { return nil }

func (p *slackProvider) Close() error { return nil }

package command

import (
	"context"
	"strings"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/memory"
	"foci/internal/modelinfo"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"

	"foci/internal/tools"
	"foci/internal/workspace"
)

// Request holds the parsed command name and arguments.
type Request struct {
	Name       string
	Args       string
	SessionKey string
	UserID     string
	ChatID     int64  // platform conversation identifier
	Source     string // originating transport (a Source* constant); "" for unidentified callers
	// PressedBy is the user who pressed the cmd: button that triggered this
	// dispatch; empty for a typed command. A button press carries no UserID —
	// the sender of the message the button rode on is unknown, so user-keyed
	// commands (/repeat) answer for nobody — but the presser is who is asking,
	// so Caller() reports the presser to the operator gate (#2279).
	PressedBy string
}

// Source values for Request.Source — the transport a command arrived on.
// The app package stamps its own literal "app" (internal/app, pre-existing);
// "" is an unidentified caller and always gets the surface-agnostic behaviour.
// The chat-transport values double as the [[platforms]] id of their platform,
// which is how the operator gate keys its policies (OperatorPolicies).
const (
	SourceTelegram = "telegram"
	SourceDiscord  = "discord"
	SourceHTTP     = "http"
)

// Caller is one dispatch's trigger identity: the transport it arrived on and
// the user on that transport — what the operator gate checks (#2279).
type Caller struct {
	Source string // a Source* constant; "" for unidentified callers
	UserID string
}

// Caller returns who triggered this request: the presser of a cmd: button
// (PressedBy), else the sender (UserID). It is the ONE identity the operator
// gate, the listings (/help, "Did you mean") and command bodies judge, so a
// press is judged as the presser everywhere (#2279).
func (r Request) Caller() Caller {
	if r.PressedBy != "" {
		return Caller{Source: r.Source, UserID: r.PressedBy}
	}
	return Caller{Source: r.Source, UserID: r.UserID}
}

// RequestFromText parses a slash command string like "/status args" into a Request.
func RequestFromText(text, sessionKey, userID string, chatID int64) Request {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "/") {
		text = text[1:]
	}
	name, args, _ := strings.Cut(text, " ")
	return Request{
		Name:       strings.ToLower(strings.TrimSpace(name)),
		Args:       strings.TrimSpace(args),
		SessionKey: sessionKey,
		UserID:     userID,
		ChatID:     chatID,
	}
}

// Response holds the command result text and optional document path.
// When Parts is non-empty, each element is sent as a separate message
// (replacing the legacy \x00-separated text convention).
//
// DocPath, when set, points to a temp file that the platform layer sends
// to the originating chat and then removes.
type Response struct {
	Text     string
	Parts    []string // when set, each part is sent as a separate message
	DocPath  string
	Keyboard []KeyboardOption // optional inline keyboard to show with the response
	// OpenConversationID, when set, asks the requesting client (only) to
	// foreground this conversation — e.g. the new conversation minted by /facet.
	OpenConversationID string
}

// DisplayParts renders the response as the message parts to show the user:
// each element of Parts when the command answered in parts, else Text as a
// single part. A response with nothing readable yields no parts, so a
// surface that iterates the parts sends nothing rather than a blank
// message. This is the one renderer for command replies — every transport
// (app, Telegram, Discord) and the suggestion press path draws from it.
func (r Response) DisplayParts() []string {
	if len(r.Parts) > 0 {
		return r.Parts
	}
	if strings.TrimSpace(r.Text) != "" {
		return []string{r.Text}
	}
	return nil
}

// DisplayText renders the response as a single text: the display parts
// joined by a blank line. This is what a surface that can show only one
// message presents — an edited button prompt, a one-line reply;
// multi-message surfaces iterate DisplayParts instead.
func (r Response) DisplayText() string {
	return strings.Join(r.DisplayParts(), "\n\n")
}

// CommandContext bundles all per-agent dependencies that slash commands need.
// Function-typed fields are ONLY runtime state resolvers or cross-boundary
// capability injection — never command handler logic.
type CommandContext struct {
	// Core agent references
	Agent        *agent.Agent
	Sessions     *session.Store
	Bootstrap    *workspace.Bootstrap
	SessionIndex *session.SessionIndex
	Config       *config.Config
	AgentConfig  config.AgentConfig

	// Provider clients
	Client         provider.Client
	ClientProvider provider.ClientProvider // GetClient, PeekClient, ResolveEndpointClient

	// Platform connection management (no circular dep: platform doesn't import command)
	ConnMgr platform.ConnectionManager

	// Paths
	PromptSearchDirs []string
	APILogPath       string
	EventLogPath     string
	ConfigPath       string

	// Model resolution
	GroupResolver *config.GroupResolver
	FallbackFunc  provider.FallbackFunc // nil disables automatic model fallback

	// Tools (command already imports tools)
	ToolsRegistry *tools.Registry
	TmuxTool      *tools.Tool // nil if tmux unavailable

	// Build info
	BuildInfo BuildInfo

	// Timing / thresholds
	StartTime           time.Time
	CompactionThreshold float64

	// Model metadata (config-defined overrides for model properties)
	ModelMetaFn func(model string) modelinfo.ModelMeta

	// Secrets
	SecretsStore     SecretsStore       // interface defined in command package
	BitwardenStore   BitwardenStoreInfo // interface defined in command package
	BitwardenEnabled bool

	// Agent listing (dynamic — returns current running agents)
	AgentListFn func() []AgentInfo

	// Stores
	LastMessageStore *LastMessageStore

	// Wizard support
	ConfigSetDeps *ConfigSetDeps
	AgentNewDeps  *AgentNewDeps
	SecretsDeps   *SecretsDeps
	AndroidDeps   *AndroidDeps

	// Todo store (for /todo command)
	TodoStore *memory.TodoStore

	// Token count cache (for /context — avoids re-counting on every call)
	TokenCountCache *TokenCountCache

	// ContextInfoFn overrides the default buildContextInfo for testing.
	// When nil, buildContextInfo is used.
	ContextInfoFn func(ctx context.Context, cc CommandContext) ContextInfo

	// PromptsDataFn overrides buildPromptsData for testing.
	// When nil, buildPromptsData is used.
	PromptsDataFn func(cc CommandContext) PromptsData

	// Facet configuration callback
	ConfigureFacet func(platform.Connection)

	// MintFacetConversation surfaces a facet branch to an app client: it creates
	// a new app conversation bound to sessionKey, adds it to the shared open-set,
	// and pushes the updated roster to all devices, returning the new conversation
	// ID. Nil for transports without app conversations (telegram/discord use the
	// secondary-bot facet pool instead). Injected by the gateway over the app hub.
	MintFacetConversation func(agentID, sessionKey string) (convID string, err error)

	// Turn cancellation (injected by platform bot)
	StopFunc       func() // cancels the current agent turn; nil = no-op
	ReleaseFunc    func() // releases a secondary bot back to its pool; nil = no-op
	IsSecondaryBot bool   // true for facet/secondary bots

	// Resolved holds the pre-merged agent+global config, frozen at agent
	// construction. Prefer ResolvedLive.Load() for anything read on a hot
	// field (internal/config/live.go) — Resolved only sees a config edit
	// after a restart.
	Resolved *config.ResolvedAgentConfig

	// ResolvedLive is the hot-swappable counterpart to Resolved — the same
	// snapshot, re-resolved and swapped in by a live config edit. Call
	// .Load() for the current value instead of reading Resolved directly.
	ResolvedLive *config.LiveValue[*config.ResolvedAgentConfig]

	// PprofControl toggles or queries the live pprof gate. action is one of
	// "on", "off", "toggle", "status"; returns the resulting enabled state.
	// Nil = pprof not available (command reports unavailable).
	PprofControl func(action string) bool

	// ActivityFunc resolves the unified app activity (kind + detail) for a
	// session, so /status renders the same value the app shows. ok is false when
	// there is no app binding for the session (non-app platforms), in which case
	// /status omits the activity line. Nil = never rendered.
	ActivityFunc func(sessionKey string) (kind, detail string, ok bool)

	// Operators gates chat-platform callers (#2279): an entry per transport
	// (keyed by Request.Source) where the agent has operators configured.
	// Nil/empty gates nobody. See OperatorPolicies.
	Operators OperatorPolicies
}

// TokenCountCache caches token counting results so /context doesn't re-count every call.
type TokenCountCache struct {
	mu       sync.Mutex
	counts   *TokenCounts
	msgCount int
	sysChars int
}

// NewTokenCountCache creates a new token count cache.
func NewTokenCountCache() *TokenCountCache {
	return &TokenCountCache{}
}

// Get returns cached token counts if the context hasn't changed.
func (c *TokenCountCache) Get(msgCount, sysChars int) *TokenCounts {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts != nil && c.msgCount == msgCount && c.sysChars == sysChars {
		return c.counts
	}
	return nil
}

// Set stores token counts with the current context key.
func (c *TokenCountCache) Set(msgCount, sysChars int, counts *TokenCounts) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgCount = msgCount
	c.sysChars = sysChars
	c.counts = counts
}

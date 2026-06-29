package command

import (
	"context"
	"strings"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/mana"
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
	ChatID     int64 // platform conversation identifier
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
	ManaName            string
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

	// Skills (for /reload)
	SkillsDirs []string

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

	// Turn cancellation (injected by platform bot)
	StopFunc       func() // cancels the current agent turn; nil = no-op
	ReleaseFunc    func() // releases a secondary bot back to its pool; nil = no-op
	IsSecondaryBot bool   // true for facet/secondary bots

	// Usage client provider (for mana command)
	UsageClientProvider mana.UsageClientProvider

	// Resolved holds the pre-merged agent+global config. Prefer reading
	// from here instead of calling config.Merge() at runtime.
	Resolved *config.ResolvedAgentConfig
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

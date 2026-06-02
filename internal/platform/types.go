package platform

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/secrets"
	"foci/internal/session"

	"foci/internal/voice"
	"foci/internal/warnings"
)

type Message struct {
	ID        string
	Text      string
	SenderID  string
	ChatID    string
	Timestamp time.Time
	Media     []Attachment
	ReplyTo   *string
}

type Attachment struct {
	Type      string
	Data      []byte
	MimeType  string
	SavedPath string
}

// legacyMIMEMap maps legacy MIME types to their modern convertible equivalents.
var legacyMIMEMap = map[string]string{
	"application/msword":                                                          "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"application/vnd.ms-excel":                                                    "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"application/vnd.ms-powerpoint":                                               "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.template":      "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.template":         "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"application/vnd.openxmlformats-officedocument.presentationml.template":        "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"application/vnd.openxmlformats-officedocument.presentationml.slideshow":       "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"application/vnd.ms-word.document.macroEnabled.12":                             "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"application/vnd.ms-excel.sheet.macroEnabled.12":                               "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"application/vnd.ms-powerpoint.presentation.macroEnabled.12":                   "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

// NormalizeMIME strips parameters (e.g. "; charset=utf-8") and maps legacy
// MIME types to their modern equivalents.
func NormalizeMIME(mime string) string {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if mapped, ok := legacyMIMEMap[mime]; ok {
		return mapped
	}
	return mime
}

// IsConvertibleDocMIME returns true if the MIME type is a document format
// that can be converted to text for LLM consumption.
// Handles parameterized and legacy MIME types.
func IsConvertibleDocMIME(mime string) bool {
	switch NormalizeMIME(mime) {
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"text/html", "text/csv", "text/plain":
		return true
	}
	return false
}

// SessionIndex abstracts session index operations used by chat session
// management (session key lookup, default chat tracking, username recording).
// Implemented by *session.SessionIndex; extracted as an interface for testability.
type SessionIndex interface {
	GetChatMetadata(agentID, platform string, chatID int64, key string) (string, error)
	SetChatMetadata(agentID, platform string, chatID int64, key, value string) error
	SetAgentMetadata(agentID, key, value string) error
	SetDefaultChat(agentID, platform string, chatID int64) error
	DefaultChatForAgent(agentID, platform string) int64
	ClearDefaultChat(agentID, platform string) error
}

type SendOptions struct {
	ParseMode string
	ReplyTo   string
}

// TextSender is implemented by platform bots for text delivery.
//
// Silent-message filtering (platform.IsSilent) is applied upstream of the
// platform layer, at the renderer/sink chokepoints (turn.TurnRenderer.OnReply
// and Finalize, turn.SessionSink.Emit) and at the BufferSink→platform
// forwarding site (asyncDispatch in cmd/foci-gw/http.go). SendText and
// SendTextToChat themselves only filter whitespace-empty input — they trust
// that any caller routing through them has already applied the silencing
// policy, or has an explicit reason not to.
type TextSender interface {
	SendText(text string) error
	SendTextToChat(chatID int64, text string) error
}

// silencingSentinels are the literal strings (post-TrimSpace) that suppress
// delivery: empty, the [[NO_RESPONSE]] marker agents emit when they have
// nothing to say, and CC's synthetic "No response requested." message.
// IsSilent and IsSilencingPrefix are both built from this list.
var silencingSentinels = []string{"[[NO_RESPONSE]]", "No response requested."}

// StripSilencingSuffix removes one or more trailing silencing sentinels (and
// surrounding whitespace) from text. "real reply.[[NO_RESPONSE]]" becomes
// "real reply."; text that is *entirely* sentinel(s) becomes "". The loop
// handles stacked/repeated trailing sentinels (an agent emitting two in a
// row). Only trailing sentinels are stripped — a sentinel embedded mid-text is
// left untouched, matching the observed failure where agents append the marker
// to a real reply. Idempotent: stripping already-clean text is a no-op.
//
// This is the single matcher; IsSilent is defined in terms of it so adding a
// new sentinel to silencingSentinels updates both behaviours at once.
func StripSilencingSuffix(text string) string {
	s := strings.TrimSpace(text)
	for {
		trimmed := false
		for _, sent := range silencingSentinels {
			if strings.HasSuffix(s, sent) {
				s = strings.TrimSpace(strings.TrimSuffix(s, sent))
				trimmed = true
			}
		}
		if !trimmed {
			return s
		}
	}
}

// IsSilent returns true if text should not be sent to users: empty/whitespace,
// or text that is entirely silencing sentinel(s) (possibly stacked or padded).
// Equivalently, nothing remains after stripping trailing sentinels.
//
// Note this is exact-suppression only: "real reply.[[NO_RESPONSE]]" is NOT
// silent (it has real content). Callers that deliver must run the text through
// StripSilencingSuffix first so the trailing marker is removed before sending —
// IsSilent alone no longer guarantees a clean payload for non-silent text.
func IsSilent(text string) bool {
	return StripSilencingSuffix(text) == ""
}

// IsSilencingPrefix returns true if the streamed text-so-far might still
// resolve to a silencing sentinel (see IsSilent). Used by streaming
// transports to gate partial flushes — while a stream's accumulated text
// is a prefix of "[[NO_RESPONSE]]" or "No response requested.", hold
// delivery. Once divergence is established (this returns false), release
// the buffer and stream normally; the final IsSilent check at TurnComplete
// remains the authoritative gate for the eventual full text.
//
// Whitespace is trimmed from both ends before the prefix check, mirroring
// IsSilent's TrimSpace behaviour. An empty/whitespace-only buffer returns
// true (could still go either way).
func IsSilencingPrefix(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return true
	}
	for _, s := range silencingSentinels {
		if strings.HasPrefix(s, t) {
			return true
		}
	}
	return false
}


type Sender interface {
	TextSender

	SessionKey() string

	// File-send methods take a caption string. Empty means no caption —
	// equivalent to the previous (caption-less) behaviour. Captioned sends
	// produce a single platform message instead of separate text+file.
	// Caption length limits are platform-specific (Telegram: 1024 chars,
	// Discord: 2000 chars); over-limit captions fall back to a separate
	// text message via the helper in this package.
	SendDocument(filePath, caption string) error
	SendVoice(filePath string) error
	SendVideo(filePath, caption string) error
	SendPhoto(filePath, caption string) error
	SendAudio(filePath, caption string) error
	SendAnimation(filePath, caption string) error
	SendVoiceData(audioData []byte) error

	SendDocumentToChat(chatID int64, filePath, caption string) error
	SendVoiceToChat(chatID int64, filePath string) error
	SendVideoToChat(chatID int64, filePath, caption string) error
	SendPhotoToChat(chatID int64, filePath, caption string) error
	SendAudioToChat(chatID int64, filePath, caption string) error
	SendAnimationToChat(chatID int64, filePath, caption string) error
	SendVoiceDataToChat(chatID int64, audioData []byte) error
}

// MaxCaptionLen is the lowest common caption-length limit across supported
// platforms. Telegram: 1024 chars. Discord allows 2000 in message Content.
// Captions longer than this should be sent as a separate text message
// followed by an uncaptioned file (the fallback path in tools/message.go).
const MaxCaptionLen = 1024

// Connection is the runtime interface for a platform instance (e.g. a Telegram
// bot, a Discord guild connection, etc.). Used by commands, notifications,
// HTTP handlers, and periodic tasks.
type Connection interface {
	Sender

	// Identity
	PlatformName() string // "telegram", "discord", etc.

	// Session management
	SessionKeyForChat(chatID int64) string
	DefaultSessionKey() string
	SetSessionKey(key string)
	SetSessionKeyDirect(key string)
	SetChatID(chatID int64)
	ChatID() int64
	Username() string
	UpdateChatSessionKey(chatID int64, newKey string) // update cached + persisted session key for a chat

	// Messaging
	SendInjectedMessage(sessionKey, text string) error // sends with system injection header
	SendToSession(sessionKey, text string) error       // sends without header (for agent replies)
	SendNotification(text string)
	SendNotificationDirect(text string) string // sends immediately, bypassing turn buffering; returns platform msg ID ("" if unknown)
	SetTyping(typing bool)              // true starts typing indicator, false stops it
}

// ButtonChoice represents an inline keyboard button for interactive prompts.
type ButtonChoice struct {
	Label string // button text shown to user
	Data  string // callback data sent when pressed
	Row   int    // which row this button goes in (0-indexed)
}

// ButtonSender is optionally implemented by Connection types that support
// inline keyboard buttons and message editing. Platforms that implement this
// get interactive messages (buttons with callbacks that edit the message).
type ButtonSender interface {
	// SendTextWithButtons sends a message with inline buttons.
	// Returns the platform message ID (as string) for later editing.
	SendTextWithButtons(text string, buttons []ButtonChoice, callbackPrefix string) (msgID string, err error)
	// EditMessageText edits an existing message's text (removes buttons).
	EditMessageText(msgID string, text string) error
	// EditMessageWithButtons edits an existing message's text and replaces its buttons.
	EditMessageWithButtons(msgID string, text string, buttons []ButtonChoice, callbackPrefix string) error
}

// ConnectionManager manages platform connection instances and facet pools.
type ConnectionManager interface {
	Primary(agentID string) Connection
	AllForAgent(agentID string) []Connection
	ForSession(sessionKey string) Connection
	ForSessionOrPrimary(sessionKey, agentID string) Connection
	AcquireFacet(agentID string) (Connection, bool)
	HasFacet(agentID string) bool
	StartAll(ctx context.Context)
	Wait()
}

// SetupResult holds the outputs from setting up platform connections for an agent.
type SetupResult struct {
	// DefaultSessionKeyFn resolves the current default session key.
	// Returns "" if no message has been received yet.
	DefaultSessionKeyFn func() string

	// ConfigureFacetConn applies platform-specific configuration to
	// a newly acquired facet connection (handler, commands, display settings).
	// May be nil if facet is not supported.
	ConfigureFacetConn func(Connection)

	// DisplayDefaultsFn returns the platform's resolved display defaults.
	// Called lazily at query time by the /display command.
	// May be nil if the platform doesn't provide display defaults.
	DisplayDefaultsFn func() DisplaySettings
}

// MessageHandler processes user messages through an agent. Text delivery,
// tool visibility, thinking, retries, and typing lifecycle flow through the
// turnevent.Sink attached to ctx (see internal/agent/turnevent) — callers that
// want the final text wire a BufferSink; interactive platforms wire a
// StreamingSink for per-turn rendering.
type MessageHandler interface {
	HandleMessage(ctx context.Context, sessionKey string, texts []string, attachments []Attachment) error
	IsProcessing() bool
	TransformMessage(text string) string
	Warnings() *warnings.Queue
}

// LifecycleEvent identifies a platform lifecycle event.
type LifecycleEvent int

const (
	OnUserMessage  LifecycleEvent = iota
	OnTurnComplete
	OnTurnEnd // fires after turn's final message is sent and cleanup is done
)

// ToolDetailStore is the interface for platform-specific tool detail persistence.
type ToolDetailStore interface {
	ExpireAndVacuum()
	Close() error
}

// StartupDiagnosis provides formatted restart diagnostic info.
type StartupDiagnosis interface {
	FormatNotification() string
}

// MessagingProvider is a platform-specific messaging implementation (e.g. telegram).
// Providers register themselves via init() and are initialised at startup.
type MessagingProvider interface {
	Name() string
	IsConfigured(cfg *config.Config) (ok bool, reason string)
	Init(deps ProviderDeps) error
	ConnectionManager() ConnectionManager
	SetupAgentConnection(params AgentConnectionParams) *SetupResult
	SetupSharedFacet(params SharedFacetParams)
	RestoreFacetSessions(params RestoreParams)
	SetLifecycleCallback(agentID string, event LifecycleEvent, fn func())
	ToolDetailStore() ToolDetailStore // may return nil
	AgentPreFlight(agentID string) []string // warnings for /agents new wizard
	DefaultPlatformConfig() config.PlatformConfig
	ValidateConfig(cfg config.PlatformConfig) []string
	Close() error
}

// ProviderDeps holds shared dependencies passed to providers at init time.
type ProviderDeps struct {
	Config       *config.Config
	SecretStore  *secrets.Store
	Sessions     *session.Store
	SessionIndex *session.SessionIndex
	STTMap       map[string]voice.STT
	TTSMap       map[string]voice.TTS
	Ctx          context.Context
	ResolveSTT   func(map[string]voice.STT, []config.STTConfig, string, map[string]string) voice.STT
	ResolveTTS   func(map[string]voice.TTS, []config.TTSConfig, string, float64, map[string]string) voice.TTS
}

// DisplaySettings holds resolved display configuration values.
// Used for both per-session overrides and platform defaults.
// Empty strings mean "not set" / "not overridden".
type DisplaySettings struct {
	ShowToolCalls string // "off"/"preview"/"full"
	ShowThinking  string // "off"/"compact"/"true"
	StreamOutput  string // "on"/"off"
	DisplayWidth  string // e.g. "44"
}

// AgentConnectionParams holds the per-agent parameters for setting up platform connections.
// Commands and LastMsgStore are typed as any to avoid importing command (which
// imports agent, which imports platform — circular). Providers type-assert.
//
// AllowedUsers is resolved by each provider from its own config section
// (e.g. telegram reads from [[platforms]] and [[agents.platforms]]).
type AgentConnectionParams struct {
	AgentID        string
	Handler        MessageHandler
	Commands       any // *command.Registry
	CommandContext any // command.CommandContext
	LastMsgStore   any // *command.LastMessageStore
	AgentConfig    config.AgentConfig
	STT            voice.STT
	TTS            voice.TTS
	ReclaimHook    func(sessionKey string)

	// DisplayOverrideFn returns per-session display overrides.
	// Empty fields mean "not overridden — use config default".
	// May be nil if per-session display overrides are not supported.
	DisplayOverrideFn func(sessionKey string) DisplaySettings

	// Resolved holds the pre-merged agent+global config.
	Resolved *config.ResolvedAgentConfig
}

// SharedFacetParams holds parameters for setting up shared facet bots.
// SessionTTL is resolved by each provider from its own config section.
type SharedFacetParams struct {
	FirstHandler     MessageHandler
	FirstCommands    any // *command.Registry
	FirstAgentConfig config.AgentConfig
	AgentOrder       []string
	ReclaimHook      func(sessionKey string)
}

// RestoreParams holds parameters for restoring facet sessions after restart.
type RestoreParams struct {
	AgentOrder []string
	// Resolver returns the handler, commands, command context, and config for a given agent.
	// Used to reconfigure facet bots with the correct agent after restart.
	// handler: platform.MessageHandler, commands: any (*command.Registry),
	// commandContext: any (command.CommandContext), config: config.AgentConfig
	Resolver func(agentID string) (handler MessageHandler, commands any, commandContext any, agentCfg config.AgentConfig, ok bool)
}

// --- Setup Wizard ---

// ErrSetupBack is returned by RunSetup when the user navigated back.
var ErrSetupBack = errors.New("setup: navigated back")

// SetupWizard is optionally implemented by MessagingProvider to contribute
// interactive setup steps to `foci first-run`.
type SetupWizard interface {
	// SetupFlags returns CLI flag definitions for non-interactive mode.
	SetupFlags() []SetupFlag

	// RunSetup runs the provider's setup flow (interactive or non-interactive).
	// Returns config/secrets fragments, or ErrSetupBack if user went back.
	RunSetup(ui SetupUI, flags map[string]string, nonInteractive bool) (*WizardResult, error)
}

// SetupFlag describes a CLI flag contributed by a provider.
type SetupFlag struct {
	Name     string // CLI flag name, e.g. "bot-token"
	Usage    string // help text
	Required bool   // required in non-interactive mode
}

// WizardResult holds the outputs from a provider's setup flow.
type WizardResult struct {
	ConfigTOML string            // TOML fragment appended to foci.toml
	Secrets    map[string]string // key→value pairs to store in secrets.toml
}

// SetupUI provides console interaction primitives to providers.
type SetupUI interface {
	Prompt(prompt string, current string) (input string, back bool)
	Menu(prompt string, options []string) (index int, back bool)
	MultiSelect(prompt string, options []string) (selected []int, back bool)
	Print(text string)
}

// NamedSetupWizard pairs a provider's registry name with its SetupWizard.
type NamedSetupWizard struct {
	Name   string
	Wizard SetupWizard
}

// SetupProviders returns all registered providers that implement SetupWizard,
// sorted by provider name for deterministic ordering.
func SetupProviders() []NamedSetupWizard {
	registryMu.Lock()
	defer registryMu.Unlock()

	// Collect names for sorted iteration.
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)

	var wizards []NamedSetupWizard
	for _, name := range names {
		if w, ok := providers[name].(SetupWizard); ok {
			wizards = append(wizards, NamedSetupWizard{Name: name, Wizard: w})
		}
	}
	return wizards
}

// --- Registry ---

var (
	registryMu sync.Mutex
	providers  = make(map[string]MessagingProvider)
)

// RegisterMessagingProvider registers a named messaging provider.
// Typically called from a provider package's init() function.
func RegisterMessagingProvider(name string, p MessagingProvider) {
	registryMu.Lock()
	defer registryMu.Unlock()
	providers[name] = p
}

// GetProvider returns the registered provider with the given name, or nil.
func GetProvider(name string) MessagingProvider {
	registryMu.Lock()
	defer registryMu.Unlock()
	return providers[name]
}

// InitMessaging initialises all registered providers that are configured,
// and returns a Messaging facade wrapping all active providers.
func InitMessaging(cfg *config.Config, deps ProviderDeps) (*Messaging, error) {
	registryMu.Lock()
	defer registryMu.Unlock()

	var active []MessagingProvider
	for name, p := range providers {
		if ok, reason := p.IsConfigured(cfg); !ok {
			log.Infof("platform", "provider %q not configured, skipping: %s", name, reason)
			continue
		}
		if err := p.Init(deps); err != nil {
			return nil, fmt.Errorf("init provider %q: %w", name, err)
		}
		active = append(active, p)
		log.Infof("platform", "provider %q initialised", name)
	}

	m := &Messaging{providers: active}
	if len(active) > 0 {
		var chatPlatformFn func(string, int64) string
		if deps.SessionIndex != nil {
			chatPlatformFn = deps.SessionIndex.PlatformForChat
		}
		var defaultSessionKeyFn func(string) string
		if deps.SessionIndex != nil {
			defaultSessionKeyFn = deps.SessionIndex.DefaultSessionKeyForAgent
		}
		m.connMgr = newAggregatingConnMgr(active, chatPlatformFn, defaultSessionKeyFn)
	} else {
		m.connMgr = &noopConnMgr{}
	}
	return m, nil
}

// --- Messaging facade ---

// Messaging wraps all active messaging providers behind a single API.
// All methods are nil-safe (noop when no providers are configured).
type Messaging struct {
	providers []MessagingProvider
	connMgr   ConnectionManager
}

func (m *Messaging) ConnectionManager() ConnectionManager {
	if m == nil {
		return &noopConnMgr{}
	}
	return m.connMgr
}

// ActivePlatformNames returns the names of all active messaging providers.
func (m *Messaging) ActivePlatformNames() []string {
	if m == nil {
		return nil
	}
	names := make([]string, len(m.providers))
	for i, p := range m.providers {
		names[i] = p.Name()
	}
	return names
}

func (m *Messaging) SetupAgentConnection(params AgentConnectionParams) []*SetupResult {
	if m == nil {
		return nil
	}
	var results []*SetupResult
	for _, p := range m.providers {
		if r := p.SetupAgentConnection(params); r != nil {
			results = append(results, r)
		}
	}
	return results
}

func (m *Messaging) SetupSharedFacet(params SharedFacetParams) {
	if m == nil {
		return
	}
	for _, p := range m.providers {
		p.SetupSharedFacet(params)
	}
}

func (m *Messaging) RestoreFacetSessions(params RestoreParams) {
	if m == nil {
		return
	}
	for _, p := range m.providers {
		p.RestoreFacetSessions(params)
	}
}

func (m *Messaging) SetLifecycleCallback(agentID string, event LifecycleEvent, fn func()) {
	if m == nil {
		return
	}
	for _, p := range m.providers {
		p.SetLifecycleCallback(agentID, event, fn)
	}
}

// NotifyAgent sends a text notification to ALL connections for an agent.
func (m *Messaging) NotifyAgent(agentID string, text string) {
	if m == nil {
		return
	}
	for _, conn := range m.connMgr.AllForAgent(agentID) {
		conn.SendNotification(text)
	}
}

// AgentPreFlight collects pre-flight warnings from all providers for a new agent.
func (m *Messaging) AgentPreFlight(agentID string) []string {
	if m == nil {
		return nil
	}
	var warnings []string
	for _, p := range m.providers {
		warnings = append(warnings, p.AgentPreFlight(agentID)...)
	}
	return warnings
}

// ToolDetailStore returns the first non-nil ToolDetailStore from providers.
func (m *Messaging) ToolDetailStore() ToolDetailStore {
	if m == nil {
		return nil
	}
	for _, p := range m.providers {
		if s := p.ToolDetailStore(); s != nil {
			return s
		}
	}
	return nil
}

func (m *Messaging) StartAll(ctx context.Context) {
	if m == nil {
		return
	}
	for _, p := range m.providers {
		p.ConnectionManager().StartAll(ctx)
	}
}

func (m *Messaging) Close() error {
	if m == nil {
		return nil
	}
	var firstErr error
	for _, p := range m.providers {
		if err := p.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// --- Noop ConnectionManager ---

type noopConnMgr struct{}

func (n *noopConnMgr) Primary(string) Connection                          { return nil }
func (n *noopConnMgr) AllForAgent(string) []Connection                    { return nil }
func (n *noopConnMgr) ForSession(string) Connection                       { return nil }
func (n *noopConnMgr) ForSessionOrPrimary(string, string) Connection      { return nil }
func (n *noopConnMgr) AcquireFacet(string) (Connection, bool)         { return nil, false }
func (n *noopConnMgr) HasFacet(string) bool                           { return false }
func (n *noopConnMgr) StartAll(context.Context)                           {}
func (n *noopConnMgr) Wait()                                              {}

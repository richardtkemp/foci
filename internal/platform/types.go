package platform

import (
	"context"
	"fmt"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/secrets"
	"foci/internal/session"
	"foci/internal/state"
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

type SendOptions struct {
	ParseMode string
	ReplyTo   string
}

type Sender interface {
	SessionKey() string

	SendText(text string) error
	SendDocument(filePath string) error
	SendVoice(filePath string) error
	SendVideo(filePath string) error
	SendPhoto(filePath string) error
	SendAudio(filePath string) error
	SendAnimation(filePath string) error
	SendVoiceData(audioData []byte) error

	SendTextToChat(chatID int64, text string) error
	SendDocumentToChat(chatID int64, filePath string) error
	SendVoiceToChat(chatID int64, filePath string) error
	SendVideoToChat(chatID int64, filePath string) error
	SendPhotoToChat(chatID int64, filePath string) error
	SendAudioToChat(chatID int64, filePath string) error
	SendAnimationToChat(chatID int64, filePath string) error
	SendVoiceDataToChat(chatID int64, audioData []byte) error
}

// Connection is the runtime interface for a platform instance (e.g. a Telegram
// bot, a Discord guild connection, etc.). Used by commands, notifications,
// HTTP handlers, and periodic tasks.
type Connection interface {
	Sender

	// Session management
	SessionKeyForChat(chatID int64) string
	DefaultSessionKey() string
	SetSessionKey(key string)
	SetSessionKeyDirect(key string)
	SetChatID(chatID int64)
	ChatID() int64
	Username() string

	// Messaging
	SendToSession(sessionKey, text string) error
	SendNotification(text string)
}

// ConnectionManager manages platform connection instances and multiball pools.
type ConnectionManager interface {
	Primary(agentID string) Connection
	AllForAgent(agentID string) []Connection
	ForSession(sessionKey string) Connection
	ForSessionOrPrimary(sessionKey, agentID string) Connection
	AcquireMultiball(agentID string) (Connection, bool)
	HasMultiball(agentID string) bool
	StartAll(ctx context.Context)
	Wait()
}

// SetupResult holds the outputs from setting up platform connections for an agent.
type SetupResult struct {
	// DefaultSessionKeyFn resolves the current default session key.
	// Returns "" if no message has been received yet.
	DefaultSessionKeyFn func() string

	// ConfigureMultiballConn applies platform-specific configuration to
	// a newly acquired multiball connection (handler, commands, display settings).
	// May be nil if multiball is not supported.
	ConfigureMultiballConn func(Connection)
}

type MessageHandler interface {
	HandleMessage(ctx context.Context, sessionKey, text string) (string, error)
	HandleMessageWithAttachments(ctx context.Context, sessionKey, text string, attachments []Attachment) (string, error)
	IsProcessing() bool
	TransformMessage(text string) string
	Warnings() *warnings.Queue
}

// LifecycleEvent identifies a platform lifecycle event.
type LifecycleEvent int

const (
	OnUserMessage  LifecycleEvent = iota
	OnTurnComplete
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
	IsConfigured(cfg *config.Config) bool
	Init(deps ProviderDeps) error
	ConnectionManager() ConnectionManager
	SetupAgentConnection(params AgentConnectionParams) *SetupResult
	SetupSharedMultiball(params SharedMultiballParams)
	RestoreMultiballSessions(params RestoreParams)
	SetLifecycleCallback(agentID string, event LifecycleEvent, fn func())
	ToolDetailStore() ToolDetailStore // may return nil
	Close() error
}

// ProviderDeps holds shared dependencies passed to providers at init time.
type ProviderDeps struct {
	Config       *config.Config
	SecretStore  *secrets.Store
	Sessions     *session.Store
	StateStore   *state.Store
	SessionIndex *session.SessionIndex
	STTMap       map[string]voice.STT
	TTSMap       map[string]voice.TTS
	Ctx          context.Context
	ResolveSTT   func(map[string]voice.STT, string) voice.STT
	ResolveTTS   func(map[string]voice.TTS, []config.TTSConfig, string, float64) voice.TTS
}

// AgentConnectionParams holds the per-agent parameters for setting up platform connections.
// Commands and LastMsgStore are typed as any to avoid importing command (which
// imports agent, which imports platform — circular). Providers type-assert.
type AgentConnectionParams struct {
	AgentID      string
	Handler      MessageHandler
	Commands     any // *command.Registry
	LastMsgStore any // *command.LastMessageStore
	AgentConfig  config.AgentConfig
	AllowedUsers []string
	STT          voice.STT
	TTS          voice.TTS
	ReclaimHook  func(sessionKey string)
}

// SharedMultiballParams holds parameters for setting up shared multiball bots.
type SharedMultiballParams struct {
	FirstHandler     MessageHandler
	FirstCommands    any // *command.Registry
	FirstAgentConfig config.AgentConfig
	AgentOrder       []string
	SessionTTL       time.Duration
	ReclaimHook      func(sessionKey string)
}

// RestoreParams holds parameters for restoring multiball sessions after restart.
type RestoreParams struct {
	AgentOrder []string
	// Resolver returns the handler, commands, and config for a given agent.
	// Used to reconfigure multiball bots with the correct agent after restart.
	// handler: platform.MessageHandler, commands: any (*command.Registry), config: config.AgentConfig
	Resolver func(agentID string) (handler MessageHandler, commands any, agentCfg config.AgentConfig, ok bool)
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

// InitMessaging initialises all registered providers that are configured,
// and returns a Messaging facade wrapping all active providers.
func InitMessaging(cfg *config.Config, deps ProviderDeps) (*Messaging, error) {
	registryMu.Lock()
	defer registryMu.Unlock()

	var active []MessagingProvider
	for name, p := range providers {
		if !p.IsConfigured(cfg) {
			log.Debugf("platform", "provider %q not configured, skipping", name)
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
		m.connMgr = newAggregatingConnMgr(active)
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

func (m *Messaging) SetupSharedMultiball(params SharedMultiballParams) {
	if m == nil {
		return
	}
	for _, p := range m.providers {
		p.SetupSharedMultiball(params)
	}
}

func (m *Messaging) RestoreMultiballSessions(params RestoreParams) {
	if m == nil {
		return
	}
	for _, p := range m.providers {
		p.RestoreMultiballSessions(params)
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

// NotifyAgentDoc sends a document to ALL connections for an agent.
func (m *Messaging) NotifyAgentDoc(agentID string, path string) {
	if m == nil {
		return
	}
	for _, conn := range m.connMgr.AllForAgent(agentID) {
		_ = conn.SendDocument(path)
	}
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

func (m *Messaging) Wait() {
	if m == nil {
		return
	}
	for _, p := range m.providers {
		p.ConnectionManager().Wait()
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

// --- Aggregating ConnectionManager ---

type aggregatingConnMgr struct {
	managers []ConnectionManager
}

func newAggregatingConnMgr(providers []MessagingProvider) *aggregatingConnMgr {
	managers := make([]ConnectionManager, len(providers))
	for i, p := range providers {
		managers[i] = p.ConnectionManager()
	}
	return &aggregatingConnMgr{managers: managers}
}

func (a *aggregatingConnMgr) Primary(agentID string) Connection {
	for _, m := range a.managers {
		if c := m.Primary(agentID); c != nil {
			return c
		}
	}
	return nil
}

func (a *aggregatingConnMgr) AllForAgent(agentID string) []Connection {
	var conns []Connection
	for _, m := range a.managers {
		conns = append(conns, m.AllForAgent(agentID)...)
	}
	return conns
}

func (a *aggregatingConnMgr) ForSession(sessionKey string) Connection {
	for _, m := range a.managers {
		if c := m.ForSession(sessionKey); c != nil {
			return c
		}
	}
	return nil
}

func (a *aggregatingConnMgr) ForSessionOrPrimary(sessionKey, agentID string) Connection {
	if c := a.ForSession(sessionKey); c != nil {
		return c
	}
	return a.Primary(agentID)
}

func (a *aggregatingConnMgr) AcquireMultiball(agentID string) (Connection, bool) {
	for _, m := range a.managers {
		if c, ok := m.AcquireMultiball(agentID); ok {
			return c, true
		}
	}
	return nil, false
}

func (a *aggregatingConnMgr) HasMultiball(agentID string) bool {
	for _, m := range a.managers {
		if m.HasMultiball(agentID) {
			return true
		}
	}
	return false
}

func (a *aggregatingConnMgr) StartAll(ctx context.Context) {
	for _, m := range a.managers {
		m.StartAll(ctx)
	}
}

func (a *aggregatingConnMgr) Wait() {
	for _, m := range a.managers {
		m.Wait()
	}
}

// --- Noop ConnectionManager ---

type noopConnMgr struct{}

func (n *noopConnMgr) Primary(string) Connection                          { return nil }
func (n *noopConnMgr) AllForAgent(string) []Connection                    { return nil }
func (n *noopConnMgr) ForSession(string) Connection                       { return nil }
func (n *noopConnMgr) ForSessionOrPrimary(string, string) Connection      { return nil }
func (n *noopConnMgr) AcquireMultiball(string) (Connection, bool)         { return nil, false }
func (n *noopConnMgr) HasMultiball(string) bool                           { return false }
func (n *noopConnMgr) StartAll(context.Context)                           {}
func (n *noopConnMgr) Wait()                                              {}

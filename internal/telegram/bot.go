package telegram

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/agent"
	"foci/internal/chatmeta"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/dispatch"
	"foci/internal/display"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/tooldetail"
	"foci/internal/turn"
	"foci/internal/voice"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

var _ platform.Sender = (*Bot)(nil)
var _ platform.ButtonSender = (*Bot)(nil)
var _ platform.SessionNotifier = (*Bot)(nil) // #911: per-session compaction-notice routing

// botClient abstracts Telegram API methods for testability.
type botClient interface {
	SendMessage(chatId int64, text string, opts *gotgbot.SendMessageOpts) (*gotgbot.Message, error)
	EditMessageText(text string, opts *gotgbot.EditMessageTextOpts) (*gotgbot.Message, bool, error)
	EditMessageReplyMarkup(opts *gotgbot.EditMessageReplyMarkupOpts) (*gotgbot.Message, bool, error)
	SendDocument(chatId int64, document gotgbot.InputFileOrString, opts *gotgbot.SendDocumentOpts) (*gotgbot.Message, error)
	SendVoice(chatId int64, voice gotgbot.InputFileOrString, opts *gotgbot.SendVoiceOpts) (*gotgbot.Message, error)
	SendVideo(chatId int64, video gotgbot.InputFileOrString, opts *gotgbot.SendVideoOpts) (*gotgbot.Message, error)
	SendPhoto(chatId int64, photo gotgbot.InputFileOrString, opts *gotgbot.SendPhotoOpts) (*gotgbot.Message, error)
	SendAudio(chatId int64, audio gotgbot.InputFileOrString, opts *gotgbot.SendAudioOpts) (*gotgbot.Message, error)
	SendAnimation(chatId int64, animation gotgbot.InputFileOrString, opts *gotgbot.SendAnimationOpts) (*gotgbot.Message, error)
	SendChatAction(chatId int64, action string, opts *gotgbot.SendChatActionOpts) (bool, error)
	GetFile(fileId string, opts *gotgbot.GetFileOpts) (*gotgbot.File, error)
	SetMyCommands(commands []gotgbot.BotCommand, opts *gotgbot.SetMyCommandsOpts) (bool, error)
	AnswerCallbackQuery(callbackQueryId string, opts *gotgbot.AnswerCallbackQueryOpts) (bool, error)
	DeleteMessage(chatId int64, messageId int64, opts *gotgbot.DeleteMessageOpts) (bool, error)
}

// attachment is a downloaded file ready for the agent (image, PDF, or convertible document).
type attachment struct {
	data      []byte
	mediaType string
	savedPath string // non-empty if saved to disk
}

// queuedMessage is a message waiting for the agent to process.
type queuedMessage struct {
	msg         *gotgbot.Message
	userID      string
	text        string
	attachments []attachment
}

// Bot wraps the Telegram bot API with agent integration.
// Messages are received on one goroutine and processed on another.
// Slash commands execute immediately on the receiver goroutine.
type Bot struct {
	log                *log.ComponentLogger
	api                *gotgbot.Bot // for receiving updates (Run)
	client             botClient    // for sending messages and files (mockable in tests)
	handler            platform.MessageHandler
	commands           *command.Registry
	dispatcher         *dispatch.Dispatcher      // platform-agnostic command dispatch
	lastMsgStore       *command.LastMessageStore // for // repeat command
	allowedUsers       map[string]bool
	agentID            string                            // agent ID for session key derivation
	sessionKey         string                            // override session key (facet secondary bots only)
	sessionMu          sync.RWMutex                      // protects sessionKey (mutable for secondary bots)
	isSecondary        bool                              // true for secondary bots (facet)
	pool               *Pool                             // back-reference to pool (secondary bots only)
	OnSessionKeyChange func(username, sessionKey string) // fires after SetSessionKey (fork/release)
	OnUserMessage      func()                            // fires on each inbound user message (for keepalive interaction tracking)
	OnTurnComplete     func()                            // fires after each agent turn completes (for cache warming tracking)
	OnTurnEnd          func()                            // fires after turn's final message is sent and cleanup is done
	botToken           string                            // for building file download URLs
	apiBase            string                            // override for /file/bot<token>/<path> base URL ("" = api.telegram.org)

	transcriber voice.STT // nil = voice notes not supported
	tts         voice.TTS // nil = TTS not available

	mq       *platform.MessageQueue // shared message queue (commands + receive funnel)
	agentRef *agent.Agent           // per-agent inbox + Enqueue access; nil for tests, set in agent_setup
	// turnActive is true while Bot.Drive is executing an agent turn.
	// Used by SendNotification to buffer notifications during turns. Set
	// at Drive entry, cleared on return. Replaces the old turnCancel-as-
	// activity-indicator after TODO #746 moved cancellable ctx ownership
	// into agent.driveOnce.
	turnActive atomic.Bool
	chatID     int64 // last known chat ID (for notifications)
	chatMu     sync.Mutex

	sessionIndex platform.SessionIndex // nil = no session key persistence across restarts
	chatmeta     *chatmeta.Resolver    // shared session key management

	display       BotDisplayConfig
	fileMode      os.FileMode          // permission bits for saved files (media, etc.)
	toolStore     turn.ToolResultStore // tool-call display state (in-memory + optional SQLite write-through)
	thinkingStore sync.Map             // message ID (int64) → thinkingEntry; ephemeral, for inline keyboard expansion
	subagentStore sync.Map             // token (string) → *subagentGroup; rolling "Hide this" button state per subagent run

	pendingNotifsMu sync.Mutex // protects pendingNotifs
	pendingNotifs   []string   // notifications buffered during active turns; drained after turn ends

	displayOverrideFn DisplayOverrideFn // per-session display override callback; nil = use bot defaults

	requireMention bool // require @mention in group chats (Telegram)

	// longPollTimeout is the HTTP-client timeout for getUpdates.
	// The Telegram-side long-poll timeout is derived as (longPollTimeout - 5s),
	// preserving the buffer required for network roundtrip and Telegram-side
	// processing. Set via ApplyAgentDisplaySettings; zero means use default.
	longPollTimeout time.Duration

	typingMu     sync.Mutex
	typingCancel context.CancelFunc // non-nil while typing ticker is running

	// mediaGroups tracks Telegram media groups (albums). Album images arrive
	// as separate bot messages sharing a media_group_id; this lets every file
	// in a set share one timestamp and get a sequential suffix (_1, _2, …)
	// instead of colliding on an identical seconds-resolution filename. Guarded
	// by mediaGroupMu; lazily created; stale entries evicted in mediaGroupStamp.
	mediaGroupMu sync.Mutex
	mediaGroups  map[string]*mediaGroupEntry

	// forceIPv4, when latched true, makes the bot's HTTP transport dial
	// api.telegram.org over IPv4 only. switchToIPv4 sets it after a sustained
	// run of IPv6 read timeouts (TODO #809); revertToDualStack clears it again
	// once polling recovers, so a transient blackhole doesn't abandon IPv6 for
	// the process lifetime (bounded by maxIPv4Reverts to stop flapping).
	// Pointer (not value) so the dialer closure — built in NewBot before this
	// struct exists — and the poll loop share one flag without copying an
	// atomic. nil on test-constructed bots (struct literal); always read via
	// ipv4Latched(), never directly.
	forceIPv4 *atomic.Bool
	// transport is the bot's HTTP transport. switchToIPv4/revertToDualStack call
	// CloseIdleConnections on it so pooled sockets for the abandoned address
	// family are dropped and the next dial re-resolves. nil on test bots.
	transport *http.Transport
}

// ipv4Latched reports whether the bot has switched to IPv4-only dialing.
// Safe on test-constructed bots where forceIPv4 is nil.
func (b *Bot) ipv4Latched() bool {
	return b.forceIPv4 != nil && b.forceIPv4.Load()
}

// BotDisplayConfig groups all display-related settings. Write-once at startup
// via ApplyAgentDisplaySettings; never mutated after. Per-session overrides
// are handled separately via DisplayOverrideFn.
type BotDisplayConfig struct {
	ShowToolCalls         string        // "off", "preview", "full"
	ShowThinking          string        // "off", "compact", "true"
	StreamOutput          bool          // stream model output to Telegram in real-time
	StreamUpdateInterval  time.Duration // duration between Telegram message edits during streaming
	DisplayWidth          int           // character width for dividers (default 44)
	TableWrapLines        int           // max wrapped lines per table cell (default 5)
	TableStyle            string        // "pretty" (default) or "markdown"
	ToolCallPreviewChars  int           // max chars for tool call preview (default 450)
	MessagesInLog         bool          // log user message content to event log
	ReceivedFilesDir      string        // if non-empty, save received files to this directory
	InjectedMessageHeader string        // prepended to injected (system) messages; empty disables
}

// resolveDisplay snapshots all display settings for a turn with the given session key.
// Applies per-session overrides from displayOverrideFn on top of bot defaults.
func (b *Bot) resolveDisplay(sessionKey string) turn.TurnDisplay {
	d := b.display
	if b.displayOverrideFn != nil {
		ov := b.displayOverrideFn(sessionKey)
		if ov.ShowToolCalls != "" {
			d.ShowToolCalls = ov.ShowToolCalls
		}
		if ov.ShowThinking != "" {
			d.ShowThinking = ov.ShowThinking
		}
		if ov.DisplayWidth > 0 {
			d.DisplayWidth = ov.DisplayWidth
		}
		switch ov.StreamOutput {
		case "true":
			d.StreamOutput = true
		case "false":
			d.StreamOutput = false
		}
	}
	return turn.TurnDisplay{
		ShowToolCalls: d.ShowToolCalls,
		ShowThinking:  d.ShowThinking,
		StreamOutput:  d.StreamOutput,
		DisplayWidth:  d.DisplayWidth,
		RenderOpts:    display.RenderOpts{MaxWidth: d.DisplayWidth, WrapLines: d.TableWrapLines, Style: d.TableStyle},
	}
}

// DisplayOverrides holds per-session overrides for display settings.
// Empty strings / zero values mean "not overridden, use bot default".
type DisplayOverrides struct {
	ShowToolCalls string // "off"/"preview"/"full"
	ShowThinking  string // "off"/"compact"/"true"
	StreamOutput  string // "true"/"false"
	DisplayWidth  int    // 0 = not overridden
}

// DisplayOverrideFn returns per-session display overrides for the given session key.
type DisplayOverrideFn func(sessionKey string) DisplayOverrides

// defaultLogger is used when a Bot is constructed without a ComponentLogger
// (e.g. in tests that build the struct literal directly).
var defaultLogger = log.NewComponentLogger("telegram")

// logger returns the bot's ComponentLogger, falling back to a package-level
// default so that test-constructed bots never nil-deref.
func (b *Bot) logger() *log.ComponentLogger {
	if b.log != nil {
		return b.log
	}
	return defaultLogger
}

// telegramAPIBaseOf extracts the Bot API base URL override from a platform
// config. Returns "" if the config is nil or the [telegram] sub-block is
// absent — NewBot then falls back to gotgbot's default
// ("https://api.telegram.org"). Used by integration tests to point bots at
// an httptest stub via foci.toml.
func telegramAPIBaseOf(pc *config.PlatformConfig) string {
	if pc == nil || pc.Telegram == nil {
		return ""
	}
	return pc.Telegram.APIBase
}

// NewBot creates a new Telegram bot.
// agentID is used for per-chat session key derivation (agent:ID:chat:CHATID).
// For secondary (facet) bots, pass agentID="" — their session key is set dynamically via SetSessionKey.
// apiBase, if non-empty, overrides the Bot API base URL — used by integration
// tests to point at an httptest stub. Empty falls back to gotgbot's default
// ("https://api.telegram.org").
func NewBot(token string, allowedUsers []string, handler platform.MessageHandler, cmds *command.Registry, lastMsgStore *command.LastMessageStore, agentID string, apiBase string) (*Bot, error) {
	// Use a transport with enough connections for concurrent API calls.
	// The default http.Transport has MaxIdleConnsPerHost=2 which is too low:
	// GetUpdates long-poll holds 1 connection, the agent worker sends typing
	// indicators + tool call messages on another, leaving 0 for the receiver
	// goroutine to handle callback queries or slash commands.
	// DefaultRequestOpts.Timeout=30s overrides gotgbot's 5s DefaultTimeout for
	// all API calls (sendMessage, editMessage, etc.). Per-call RequestOpts still
	// take precedence: long-poll getUpdates keeps its 65s, shutdown ack keeps 5s.
	// 5s was too tight under transient network blips (observed scout sendMessage
	// context-deadlining during a routine reply, 2026-05-14).
	defaultReqOpts := &gotgbot.RequestOpts{
		Timeout: 30 * time.Second,
	}
	if apiBase != "" {
		// gotgbot trims trailing slashes itself; we hand it the raw value
		// so the override is uniform across getUpdates, sendMessage, etc.
		defaultReqOpts.APIURL = apiBase
	}
	// IPv6 fallback dialer.
	//
	// Telegram's IPv6 address for api.telegram.org (2001:67c:4e8:f004::9) has
	// intermittently *blackholed*: the TCP handshake completes but subsequent
	// reads stall until the client timeout, so getUpdates long-polls fail in a
	// sustained run while IPv4 stays perfectly healthy (TODO #809; the scout
	// outage on 2026-06-07 ran 34 min / 36 consecutive failures). Happy-eyeballs
	// (RFC 6555) does NOT save us here: it only races connection *setup*, and the
	// v6 setup succeeds — the stall is on the data path, after the handshake.
	//
	// So we install a custom dialer whose address family can be flipped at
	// runtime. Normal operation dials dual-stack ("tcp", v6-preferred). When the
	// poll loop sees the timeout signature (see classifyPollError / switchToIPv4
	// in bot_poll.go) it latches forceIPv4, and every subsequent dial uses
	// "tcp4" — IPv4 only. The latch is NOT permanent: revertToDualStack clears
	// it once polling recovers, because the observed blackhole self-healed in
	// ~34 min (#809) and v6 is the preferred path. A persistent blackhole that
	// keeps re-tripping the switch latches IPv4 for the process after
	// maxIPv4Reverts flap cycles. A fresh process always starts dual-stack.
	forceIPv4 := &atomic.Bool{}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		MaxIdleConnsPerHost: 8,
		// A custom DialContext disables Go's automatic HTTP/2 upgrade unless we
		// opt back in; keep H2 so behaviour matches the previous bare transport.
		ForceAttemptHTTP2: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if forceIPv4.Load() {
				network = "tcp4"
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	// Construct via connectBot so a transient DNS/network blip at boot
	// (e.g. systemd brings foci up before DNS is ready) doesn't permanently
	// disable the bot. See bot_connect.go and TODO #796.
	lg := log.NewComponentLogger("telegram:" + agentID)
	api, err := connectBot(token, &gotgbot.BotOpts{
		BotClient: &gotgbot.BaseBotClient{
			Client: http.Client{
				Transport: transport,
			},
			DefaultRequestOpts: defaultReqOpts,
		},
	}, lg, defaultConnectBackoff)
	if err != nil {
		// err is already token-redacted by connectBot.
		return nil, err
	}

	allowed := make(map[string]bool, len(allowedUsers))
	for _, u := range allowedUsers {
		allowed[u] = true
	}

	bot := &Bot{
		log:          lg,
		api:          api,
		client:       api,
		handler:      handler,
		commands:     cmds,
		lastMsgStore: lastMsgStore,
		allowedUsers: allowed,
		agentID:      agentID,
		botToken:     token,
		apiBase:      apiBase,
		forceIPv4:    forceIPv4,
		transport:    transport,
		chatmeta: &chatmeta.Resolver{
			AgentID:      agentID,
			PlatformName: platformName,
			Logger:       func() *log.ComponentLogger { return lg },
		},
	}
	bot.mq = platform.NewMessageQueue(platform.MessageQueueConfig{
		Size:   64,
		Logger: lg,
	})
	return bot, nil
}

// SessionKeyForChatID returns the session key for a given chat ID, accounting
// for facet/secondary bot routing. Exported so the urgent-steer dispatcher
// (installed by SetupAgent in agent_setup.go) can resolve a queued message's
// session without reaching into private bot state.
func (b *Bot) SessionKeyForChatID(chatID int64) string {
	if b.isSecondary {
		return b.SessionKey()
	}
	if b.agentID != "" {
		return b.sessionKeyForMsg(chatID)
	}
	return b.SessionKey()
}

// SetTranscriber sets the STT provider for inbound voice notes.
func (b *Bot) SetTranscriber(t voice.STT) {
	b.transcriber = t
}

// SetTTS sets the TTS provider for outbound voice notes.
func (b *Bot) SetTTS(t voice.TTS) {
	b.tts = t
}

// SetDisplayOverrideFn sets the callback that provides per-session display overrides.
func (b *Bot) SetDisplayOverrideFn(fn DisplayOverrideFn) { b.displayOverrideFn = fn }

// ShowToolCallsDefault returns the bot's configured show_tool_calls default.
func (b *Bot) ShowToolCallsDefault() string { return b.display.ShowToolCalls }

// ShowThinkingDefault returns the bot's configured show_thinking default.
func (b *Bot) ShowThinkingDefault() string { return b.display.ShowThinking }

// StreamOutputDefault returns the bot's configured stream_output default.
func (b *Bot) StreamOutputDefault() bool { return b.display.StreamOutput }

// DisplayWidthDefault returns the bot's configured display_width default.
func (b *Bot) DisplayWidthDefault() int { return b.display.DisplayWidth }

// tableOpts returns the RenderOpts using the bot's default display settings.
// For turn-aware rendering with per-session overrides, use resolveDisplay().renderOpts.
func (b *Bot) tableOpts() display.RenderOpts {
	return display.RenderOpts{MaxWidth: b.display.DisplayWidth, WrapLines: b.display.TableWrapLines, Style: b.display.TableStyle}
}

// streamInterval returns the configured stream update interval, defaulting to 250ms.
func (b *Bot) streamInterval() time.Duration {
	if b.display.StreamUpdateInterval > 0 {
		return b.display.StreamUpdateInterval
	}
	return 250 * time.Millisecond
}

// messageContainsMention returns true if the message @mentions the bot.
func (b *Bot) messageContainsMention(msg *gotgbot.Message) bool {
	if b.api == nil || b.api.User.Id == 0 {
		return false
	}
	botUsername := b.api.User.Username
	botUserID := b.api.User.Id
	for _, entity := range msg.Entities {
		if entity.Type == "mention" && botUsername != "" {
			mentioned := msg.Text[entity.Offset : entity.Offset+entity.Length]
			if mentioned == "@"+botUsername {
				return true
			}
		}
		if entity.Type == "text_mention" && entity.User != nil && entity.User.Id == botUserID {
			return true
		}
	}
	return false
}

// SetToolDetailStore sets the persistent store for tool call details.
// On startup, loads entries <48h old into the in-memory map.
func (b *Bot) SetToolDetailStore(store *tooldetail.Store) {
	b.toolStore.SetDetailStore(store, b.logger())
}

// SetCommandContext configures the command dispatcher with the unified CommandContext.
func (b *Bot) SetCommandContext(cc command.CommandContext) {
	cc.StopFunc = b.cancelTurn
	cc.IsSecondaryBot = b.isSecondary
	if b.isSecondary {
		cc.ReleaseFunc = func() {
			// Capture session key before pool.Release clears it.
			sk := b.SessionKey()
			if b.pool != nil {
				b.pool.Release(b)
			}
			// Close any delegated branch backend so the CC process doesn't leak.
			if sk != "" && cc.Agent != nil && cc.Agent.DelegatedManager != nil {
				cc.Agent.DelegatedManager.ResetSession(sk)
			}
			b.logger().Infof("secondary bot released")
		}
	}
	b.dispatcher = dispatch.NewDispatcher(b.commands, cc, b.agentID)
	b.dispatcher.SetSessionKeyFunc(b.dispatchSessionKey)
}

// dispatchSessionKey resolves the session key for command dispatch.
// Secondary bots with an override session key use it directly;
// primary bots resolve per-chat keys as usual.
func (b *Bot) dispatchSessionKey(chatID int64) string {
	if b.isSecondary {
		b.sessionMu.RLock()
		sk := b.sessionKey
		b.sessionMu.RUnlock()
		if sk != "" {
			return sk
		}
	}
	return b.sessionKeyForMsg(chatID)
}

// DisplaySettings returns the bot's default display settings for inspection/testing.
func (b *Bot) DisplaySettings() (showToolCalls, showThinking string, displayWidth int, messagesInLog bool, receivedFilesDir string, injectedMessageHeader string) {
	d := b.display
	return d.ShowToolCalls, d.ShowThinking, d.DisplayWidth, d.MessagesInLog, d.ReceivedFilesDir, d.InjectedMessageHeader
}

// SetSessionIndex sets the session index for persisting chat-to-session-key mappings.
// Must be called before the bot starts receiving messages to ensure session continuity across restarts.
func (b *Bot) SetSessionIndex(idx platform.SessionIndex) {
	b.sessionIndex = idx
	if b.chatmeta != nil {
		b.chatmeta.Index = idx
	}
	b.seedDefaultChatFromAllowedUser()
}

// seedDefaultChatFromAllowedUser persists a default chat when none exists yet
// and the agent has exactly one allowed user.
//
// The default chat is normally recorded on the first inbound message
// (bot_receive.go). On a fresh install nobody has messaged the bot yet, so no
// default chat exists — which leaves proactive sends (a startup re-login URL,
// keepalive, cron) with nowhere to go (#853: the login URL was extracted but
// "no chat ID for session and no default chat", so onboarding stalled).
//
// For a Telegram private chat the chat ID equals the user's ID, so a single
// allowed user unambiguously identifies the owner's DM. With zero or multiple
// allowed users, or a non-numeric (@username) entry, we cannot safely guess —
// we skip and behaviour is unchanged. The DefaultChatForAgent==0 guard makes
// this a no-op on any install that already has a default chat.
func (b *Bot) seedDefaultChatFromAllowedUser() {
	if b.agentID == "" || b.sessionIndex == nil {
		return // secondary/facet bot, or no persistence
	}
	if len(b.allowedUsers) != 1 {
		return // ambiguous (zero or many) — cannot choose an owner
	}
	if b.sessionIndex.DefaultChatForAgent(b.agentID, platformName) != 0 {
		return // a default chat already exists
	}
	var only string
	for u := range b.allowedUsers {
		only = u
	}
	chatID, err := strconv.ParseInt(only, 10, 64)
	if err != nil || chatID == 0 {
		return // non-numeric entry (e.g. @username) — not usable as a chat ID
	}
	if err := b.sessionIndex.SetDefaultChat(b.agentID, platformName, chatID); err != nil {
		b.logger().Errorf("seed default chat from allowed user: %v", err)
		return
	}
	b.logger().Infof("seeded default chat %d from sole allowed user — no prior default; enables proactive delivery before first inbound message", chatID)
}

// SetSecondary marks this bot as a secondary bot in the given pool.
func (b *Bot) SetSecondary(pool *Pool) {
	b.isSecondary = true
	b.pool = pool
}

// SetAgentAndCommands re-wires the bot to a different agent and command registry.
// Only safe to call on idle secondary bots (no active session key) between
// pool acquisition and setting the session key.
func (b *Bot) SetHandlerAndCommands(handler platform.MessageHandler, cmds *command.Registry) {
	b.handler = handler
	b.commands = cmds
}

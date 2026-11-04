package slack

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/netretry"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/usercmd"
)

// Manager is the one shared Slack connection state: the router (config), the
// Web API client, the target store, and one unbound Conn per agent. It
// implements platform.ConnectionSource[*Conn]; the per-agent Conn is minted
// at SetupAgentConnection.
type Manager struct {
	router       router
	api          *webAPI
	sock         *socketClient
	targets      *targetStore
	redact       func(string) string // scrubs both tokens from any logged error text
	slackLog     func() *log.ComponentLogger
	unmappedHold time.Duration // per-thread "not set up here" reply window (config)

	botUserID atomic.Value // string, set once auth.test succeeds

	mu    sync.RWMutex
	cores map[string]*agentCore // per-agent state (agentID → core)

	// unmapped holds the last "not set up here" reply time per thread
	// identity, so repeated mentions in one thread get one notice.
	unmapped   map[string]time.Time
	unmappedMu sync.Mutex

	wait sync.WaitGroup
}

func newManager(cfg *config.SlackSpecific, api *webAPI, sock *socketClient, idx platform.SessionIndex, redact func(string) string) *Manager {
	logger := func() *log.ComponentLogger { return log.NewComponentLogger(platformName) }
	return &Manager{
		router:       newRouter(cfg),
		api:          api,
		sock:         sock,
		targets:      newTargetStore(idx, logger),
		redact:       redact,
		slackLog:     logger,
		unmappedHold: cfg.UnmappedReplyHoldDuration(),
		cores:        make(map[string]*agentCore),
		unmapped:     make(map[string]time.Time),
	}
}

// botUserIDFor is classifyEvent's self-message filter before auth.test
// returns ("" matches nothing).
func (m *Manager) botUserIDFor() string {
	if v, ok := m.botUserID.Load().(string); ok {
		return v
	}
	return ""
}

// --- platform.ConnectionSource[*Conn] ---

func (m *Manager) PrimaryBot(agentID string) *Conn {
	core := m.core(agentID)
	if core == nil {
		return nil
	}
	return &Conn{core: core}
}

// BotForSession returns a view of the agent's connection pinned to
// sessionKey — but only when the session's conversation is REGISTERED (the
// slack_target row exists): a session with no Slack conversation (an
// independent session, a chat owned by another platform) has nothing to
// deliver to, and a nil return routes delivery to the offline path instead of
// a guessed conversation.
func (m *Manager) BotForSession(sessionKey string) *Conn {
	if sessionKey == "" {
		return nil
	}
	core := m.core(session.AgentIDFromKey(sessionKey))
	if core == nil {
		return nil
	}
	if _, ok := core.targetFor(sessionKey); !ok {
		return nil
	}
	return (&Conn{core: core}).boundTo(sessionKey)
}

func (m *Manager) BotForSessionOrPrimary(sessionKey, agentID string) *Conn {
	if c := m.BotForSession(sessionKey); c != nil {
		return c
	}
	return m.PrimaryBot(agentID)
}

func (m *Manager) AcquireFacet(string) (*Conn, bool) { return nil, false } // no facets on Slack
func (m *Manager) HasFacet(string) bool              { return false }

func (m *Manager) core(agentID string) *agentCore {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cores[agentID]
}

// addCore registers an agent's state; called once per SetupAgentConnection.
func (m *Manager) addCore(core *agentCore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cores[core.agentID] = core
}

// StartAll connects in its own goroutine (auth.test under netretry, then the
// socket loop), so a Slack outage never holds up startup — the same shape as
// Telegram/Discord's background connect (#2043).
func (m *Manager) StartAll(ctx context.Context) {
	m.wait.Add(1)
	go func() {
		defer m.wait.Done()
		if err := netretry.Do(ctx, netretry.Policy{
			Name:      "slack auth.test",
			Backoff:   netretry.StartupBackoff,
			Permanent: netretry.PermanentMarkers("invalid_auth", "not_authed", "account_inactive", "token_revoked", "token_expired"),
			Redact:    m.redact,
			Log:       m.slackLog(),
		}, func() error {
			id, err := m.api.authTest(ctx)
			if err != nil {
				return err
			}
			m.botUserID.Store(id)
			return nil
		}); err != nil {
			m.slackLog().Errorf("%v (running without Slack)", m.redact(err.Error()))
			return
		}
		if err := m.sock.run(ctx, m.handleEnvelope); err != nil && ctx.Err() == nil {
			m.slackLog().Errorf("%v (running without Slack; restart foci to reconnect — #2459 A2 adds reconnect)", m.redact(err.Error()))
		}
	}()
}

func (m *Manager) Wait() { m.wait.Wait() }

// --- inbound dispatch ---

// handleEnvelope routes one acked Socket Mode envelope. events_api payloads
// are classified and routed to an agent; interactive payloads are button
// presses. slash_commands and unknown types reach here already acked and are
// dropped: no foci command can be run from Slack (#2459 part B owns them).
func (m *Manager) handleEnvelope(env envelope) {
	switch env.Type {
	case "events_api":
		var payload struct {
			Event slackEvent `json:"event"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			m.slackLog().Warnf("events_api: unparseable event payload ignored")
			return
		}
		msg, reason := classifyEvent(payload.Event, m.botUserIDFor())
		if reason != "" {
			m.slackLog().Debugf("ignored event (%s): type=%s subtype=%s channel=%s", reason, payload.Event.Type, payload.Event.Subtype, payload.Event.Channel)
			return
		}
		m.handleMessage(msg)
	case "interactive":
		press, ok := parsePress(env.Payload)
		if !ok {
			return
		}
		m.handlePress(press)
	}
}

// handleMessage routes one accepted inbound message (#2459 req 2, 7): pick
// the agent (DM default / channel mapping), check the allowlist, register
// the conversation target, then enqueue the turn. Text passes through
// verbatim — no command registry, no wizard; "/restart" is an ordinary
// message until part B.
func (m *Manager) handleMessage(msg inboundMessage) {
	var agentID string
	switch msg.Kind {
	case inboundDM:
		var ok bool
		agentID, ok = m.router.forDM()
		if !ok {
			m.slackLog().Infof("dm from %s ignored: no default_agent configured", msg.User)
			return
		}
	case inboundMention:
		var ok bool
		agentID, ok = m.router.forChannel(msg.Channel)
		if !ok {
			m.replyNotSetUp(msg)
			return
		}
		if msg.Text == "" {
			// A bare "<@BOT>" in a mapped channel carries nothing to answer:
			// no turn, no registration (classifyEvent accepts it only so an
			// unmapped channel can reply; review F1).
			m.slackLog().Debugf("agent %s: bare mention from %s in %s starts no turn", agentID, msg.User, msg.Channel)
			return
		}
	}
	core := m.core(agentID)
	if core == nil {
		// Config named an agent whose platform connection was never set up
		// (e.g. Slack left inert on a credential problem).
		m.slackLog().Errorf("message routed to agent %q which has no Slack connection", agentID)
		return
	}
	if !core.userAllowed(msg.User) {
		m.slackLog().Warnf("agent %s: rejected message from user %s (not on the allow list)", agentID, msg.User)
		return
	}
	target := targetFor(msg)
	chatID, err := core.targets.register(agentID, target)
	if err != nil {
		m.slackLog().Errorf("agent %s: refusing message from %s: %v", agentID, msg.User, err)
		return
	}
	sessionKey := sessionKeyFor(agentID, target)
	core.fireOnUserMessage()
	env := agent.Envelope{
		SessionKey:  sessionKey,
		Text:        msg.Text,
		UserID:      msg.User,
		ChatID:      chatID,
		IsGroupChat: msg.Kind == inboundMention,
		ReceivedAt:  time.Now(),
		Original:    msg,
		Driver:      (&Conn{core: core}).boundTo(sessionKey),
	}
	if script, ok := usercmd.Parse(msg.Text); ok && len(msg.Files) == 0 {
		m.runShellCommand(core, env, script)
		return
	}
	if !core.enq.Enqueue(env) {
		// A refused enqueue (the agent's inbox queue is full) drops the
		// message: the sender sees nothing, so the drop must be visible
		// here (#2459 req 1's "never silently" delivery hygiene; A2 owns
		// the retry).
		m.slackLog().Warnf("agent %s: dropped message from %s for session %s (agent inbox refused the enqueue)", agentID, msg.User, sessionKey)
	}
}

// replyNotSetUp posts the one-per-thread "not set up here" notice for a
// mention in a channel with no agent mapped (#2459 req 2): one reply per
// thread per unmapped_reply_hold window. Expired holds are pruned on each
// insert so the map stays bounded by the live threads, not by every
// unmapped thread ever seen.
func (m *Manager) replyNotSetUp(msg inboundMessage) {
	thread := msg.Channel + "/" + msg.ThreadTS
	m.unmappedMu.Lock()
	now := time.Now()
	if last, ok := m.unmapped[thread]; ok && now.Sub(last) < m.unmappedHold {
		m.unmappedMu.Unlock()
		return
	}
	for k, at := range m.unmapped {
		if now.Sub(at) >= m.unmappedHold {
			delete(m.unmapped, k)
		}
	}
	m.unmapped[thread] = now
	m.unmappedMu.Unlock()

	m.slackLog().Infof("mention in unmapped channel %s: replying that no agent is set up here", msg.Channel)
	_, err := m.api.postMessage(context.Background(), postParams{
		Channel:  msg.Channel,
		ThreadTS: msg.ThreadTS,
		Text:     "This channel isn't set up for an agent here.",
	})
	if err != nil {
		m.slackLog().Warnf("unmapped-channel reply: %v", err)
	}
}

// handlePress routes a button press: the session travels in the actions
// block's block_id, the presser passes the allowlist of that session's
// agent, and a resolved foci prompt edits its own message in place.
func (m *Manager) handlePress(press pressEvent) {
	core := m.core(session.AgentIDFromKey(press.SessionKey))
	if core == nil {
		return
	}
	if !core.userAllowed(press.User) {
		m.slackLog().Warnf("agent %s: rejected button press from user %s (not on the allow list)", core.agentID, press.User)
		return
	}
	value, found := strings.CutPrefix(press.Value, "im:")
	if !found {
		return // not a foci interactive prompt (nothing else is sent today)
	}
	editText, _, ok := platform.HandleInteractiveCallback(value)
	if ok && editText != "" {
		if err := core.editText(context.Background(), msgRef{Channel: press.Channel, TS: press.MessageTS}, editText); err != nil {
			core.logger().Warnf("press edit: %v", err)
		}
	}
}

// shellCommander is the agent's shared "!cmd" flow (#2812). *agent.Agent
// satisfies it; an enqueue-only test fake gets the refusal.
type shellCommander interface {
	HandleTextCommand(platformID string, base agent.Envelope, script string, reply func(string))
}

// runShellCommand hands a typed "!cmd" to the agent. Text after an @mention is
// a command too, and the group rule refuses it.
func (m *Manager) runShellCommand(core *agentCore, env agent.Envelope, script string) {
	view := (&Conn{core: core}).boundTo(env.SessionKey)
	reply := func(text string) {
		if err := view.SendText(text); err != nil {
			m.slackLog().Warnf("agent %s: shell command reply: %v", core.agentID, err)
		}
	}
	sc, ok := core.enq.(shellCommander)
	if !ok {
		reply(usercmd.RefusedUnavailable)
		return
	}
	sc.HandleTextCommand(platformName, env, script, reply)
}

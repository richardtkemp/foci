package slack

// harness_test.go: the shared offline harness — a fake Slack (Web API +
// Socket Mode), a real SessionIndex in a temp dir, a fake agent recording
// envelopes, and a Manager wired to both.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
)

// fakeAgent records envelopes and lifecycle calls; it satisfies inboxAgent.
// refuseEnqueue makes Enqueue decline (the inbox-full shape) so tests can
// prove the drop is logged.
type fakeAgent struct {
	refuseEnqueue bool
	mu            sync.Mutex
	envelopes     []agent.Envelope
	steer         []bool
	started       int
}

func (a *fakeAgent) Enqueue(env agent.Envelope) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.envelopes = append(a.envelopes, env)
	return !a.refuseEnqueue
}

func (a *fakeAgent) SetInboxSteerMode(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steer = append(a.steer, v)
}

func (a *fakeAgent) StartInbox(context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.started++
}

func (a *fakeAgent) drained() []agent.Envelope {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]agent.Envelope(nil), a.envelopes...)
}

func (a *fakeAgent) last() agent.Envelope {
	envs := a.drained()
	if len(envs) == 0 {
		return agent.Envelope{}
	}
	return envs[len(envs)-1]
}

// harness is one wired-together Slack transport over the fake.
type harness struct {
	t       *testing.T
	fake    *fakeSlack
	idx     *session.SessionIndex
	mgr     *Manager
	cfg     *config.SlackSpecific
	handler *fakeAgent
}

// slackConfigTOML-ish builder: channels map channel→agent; defaultAgent for
// DMs; apiBase points the transport at the fake.
func newHarness(t *testing.T, defaultAgent string, channels map[string]string) *harness {
	t.Helper()
	fake := newFakeSlack(t)
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	cfg := &config.SlackSpecific{DefaultAgent: defaultAgent, APIBase: fake.apiBase()}
	for id, ag := range channels {
		cfg.Channels = append(cfg.Channels, config.SlackChannel{ID: id, Agent: ag})
	}
	h := &harness{t: t, fake: fake, idx: idx, cfg: cfg}
	h.build()
	// The bot's own user id, as auth.test discovers it in production (the
	// fake answers "UBOTID"); without it self-messages and mention
	// stripping could not work.
	h.mgr.botUserID.Store("UBOTID")
	// Seed a core for every agent the router names, with a strict allowlist
	// admitting U1 (tests override with allow); a mapped/default agent with
	// no core would drop every message.
	if defaultAgent != "" {
		h.allow(defaultAgent, true, "U1")
	}
	for _, ag := range channels {
		h.allow(ag, true, "U1")
	}
	return h
}

// build (re)creates the manager — restart tests call it again on the same
// index to prove session keys and routing survive. Cores are re-seeded the
// same way newHarness does.
func (h *harness) build() {
	h.t.Helper()
	redact := func(s string) string { return s }
	h.mgr = newManager(h.cfg, newWebAPI(h.cfg.APIBaseURL(), "xoxb-fake", h.fake.httpc), nil, h.idx, redact)
	h.mgr.botUserID.Store("UBOTID")
	if h.cfg.DefaultAgent != "" {
		h.allow(h.cfg.DefaultAgent, true, "U1")
	}
	for _, ch := range h.cfg.Channels {
		h.allow(ch.Agent, true, "U1")
	}
}

// allow configures an agent's allowlist (empty users + Only=true is the
// interim "nobody allowed" default).
func (h *harness) allow(agentID string, only bool, users ...string) {
	h.t.Helper()
	h.mgr.addCore(h.newCore(agentID, config.AllowedAccess{Users: users, Only: only}))
}

// newCore builds a bare agentCore (tests that need Enqueue wiring use allow).
func (h *harness) newCore(agentID string, access config.AllowedAccess) *agentCore {
	accessMap := make(map[string]bool, len(access.Users))
	for _, u := range access.Users {
		accessMap[u] = true
	}
	return &agentCore{
		agentID:        agentID,
		api:            newWebAPI(h.cfg.APIBaseURL(), "xoxb-fake", h.fake.httpc),
		targets:        h.mgr.targets,
		enq:            &fakeAgent{},
		access:         access,
		accessMap:      accessMap,
		streamInterval: defaultStreamInterval,
		logger:         func() *log.ComponentLogger { return log.NewComponentLogger("slack-test") },
	}
}

// core returns the agent's core (creating one with an open allowlist).
func (h *harness) core(agentID string) *agentCore {
	h.t.Helper()
	if c := h.mgr.core(agentID); c != nil {
		return c
	}
	c := h.newCore(agentID, config.AllowedAccess{Users: []string{"U1"}, Only: true})
	h.mgr.addCore(c)
	return c
}

// boundConn returns a bound view of the agent's connection, after
// registering the target so the view resolves.
func (h *harness) boundConn(agentID string, t Target) *Conn {
	h.t.Helper()
	if _, err := h.mgr.targets.register(agentID, t); err != nil {
		h.t.Fatal(err)
	}
	return h.mgr.PrimaryBot(agentID).boundTo(sessionKeyFor(agentID, t))
}

// dm event for user in channel.
func dmEvent(user, channel, text, ts string) slackEvent {
	return slackEvent{Type: "message", ChannelType: "im", User: user, Channel: channel, Text: text, TS: ts}
}

// mentionEvent builds an app_mention; text is the raw Slack text (it carries
// the "<@BOT>" mention). threadTS "" means a top-level mention.
func mentionEvent(user, channel, text, ts, threadTS string) slackEvent {
	return slackEvent{Type: "app_mention", User: user, Channel: channel, Text: text, TS: ts, ThreadTS: threadTS}
}

// deliver runs one event through the full inbound path (classify → route →
// access → register → enqueue).
func (h *harness) deliver(ev slackEvent) {
	h.t.Helper()
	h.mgr.handleEnvelope(envelopeFor(ev))
}

func envelopeFor(ev slackEvent) envelope {
	payload, _ := json.Marshal(map[string]any{"type": "event_callback", "event": ev})
	return envelope{EnvelopeID: "env-" + ev.Type + "-" + ev.TS + "-" + ev.Channel, Type: "events_api", Payload: payload}
}

// waitEnvelopes polls the fake agent of core until n envelopes arrived.
func (h *harness) waitEnvelopes(core *agentCore, n int) []agent.Envelope {
	h.t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if fa, ok := core.enq.(*fakeAgent); ok {
			if envs := fa.drained(); len(envs) >= n {
				return envs
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %d envelopes", n)
	return nil
}

// configAllowed builds a strict allowlist of the given users.
func configAllowed(users ...string) config.AllowedAccess {
	return config.AllowedAccess{Users: users, Only: true}
}

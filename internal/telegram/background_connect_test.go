package telegram

// #2043: a Telegram outage at boot must not hold up foci startup. The bot's
// getMe connect runs in the background (netretry schedule) and the bot only
// becomes a live connection once it succeeds.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/netretry"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// recordingBotClient is a gotgbot.BotClient that answers like the Bot API and
// records every outbound sendMessage text.
type recordingBotClient struct {
	mu    sync.Mutex
	texts []string
}

func (c *recordingBotClient) RequestWithContext(ctx context.Context, _ string, method string, params map[string]string, _ map[string]gotgbot.FileReader, _ *gotgbot.RequestOpts) (json.RawMessage, error) {
	switch method {
	case "getUpdates":
		// A quiet long-poll: nothing to deliver, without spinning the loop.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		return json.RawMessage("[]"), nil
	case "sendMessage":
		c.mu.Lock()
		c.texts = append(c.texts, params["text"])
		c.mu.Unlock()
		return json.RawMessage(`{"message_id":1,"date":0,"chat":{"id":42,"type":"private"}}`), nil
	}
	return json.RawMessage("true"), nil
}

func (c *recordingBotClient) GetAPIURL(*gotgbot.RequestOpts) string { return "http://fake.local" }
func (c *recordingBotClient) FileURL(_ string, p string, _ *gotgbot.RequestOpts) string {
	return "http://fake.local/file/" + p
}

func (c *recordingBotClient) sent() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.texts...)
}

// switchableTelegram is a botFactory whose getMe fails with the boot DNS
// error until succeed() is called.
type switchableTelegram struct {
	failing  atomic.Bool
	attempts atomic.Int32
	client   *recordingBotClient

	// shutdownAt, if set, is called from inside attempt cancelAt — shutdown
	// arriving mid-attempt, the moment after which no attempt may start.
	cancelAt   int32
	shutdownAt func()
}

func (s *switchableTelegram) succeed() { s.failing.Store(false) }

// withSwitchableTelegram installs a failing factory and a millisecond,
// UNBOUNDED retry schedule — the production shape, so a connect that is not
// moved off the setup path hangs setup rather than giving up and passing.
func withSwitchableTelegram(t *testing.T) *switchableTelegram {
	t.Helper()
	s := &switchableTelegram{client: &recordingBotClient{}}
	s.failing.Store(true)
	withStubFactory(t, func(token string, _ *gotgbot.BotOpts) (*gotgbot.Bot, error) {
		if n := s.attempts.Add(1); n == s.cancelAt && s.shutdownAt != nil {
			s.shutdownAt()
		}
		if s.failing.Load() {
			return nil, &url.Error{Op: "Post", URL: "https://api.telegram.org/bot/getMe", Err: &net.OpError{
				Op: "dial", Net: "tcp",
				Err: &net.DNSError{Err: "server misbehaving", Name: "api.telegram.org", Server: "127.0.0.53:53", IsTemporary: true},
			}}
		}
		return &gotgbot.Bot{Token: token, User: gotgbot.User{Id: 99, Username: "bot-" + token, IsBot: true}, BotClient: s.client}, nil
	})
	orig := defaultConnectBackoff
	defaultConnectBackoff = netretry.Backoff{InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 2}
	t.Cleanup(func() { defaultConnectBackoff = orig })
	return s
}

// within runs fn and fails the test if it has not returned by the deadline.
// The deadline is a hang guard only, far above any real cost.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s blocked while Telegram was unreachable: foci startup would wait on it", what)
	}
}

// eventually polls cond until it holds; the deadline is a hang guard.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestBackgroundConnect_FailingTelegramDoesNotBlockStartup(t *testing.T) {
	sw := withSwitchableTelegram(t)
	mgr := NewBotManager()
	cm := platform.NewConnectionManagerAdapter[*Bot](mgr)

	var res *platform.SetupResult
	within(t, "SetupAgent", func() { res = SetupAgent(mgr, setupAgentFixture(t)) })
	if res == nil {
		t.Fatal("SetupAgent returned nil: the agent's telegram config was dropped instead of connecting later")
	}
	if cm.Primary("scout") != nil {
		t.Fatal("primary bot is a live connection before it ever connected")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); mgr.Wait() }()
	within(t, "StartAll", func() { mgr.StartAll(ctx) })

	eventually(t, "3 failed connect attempts", func() bool { return sw.attempts.Load() >= 3 })
	if cm.Primary("scout") != nil {
		t.Fatal("primary attached while getMe was still failing")
	}
	if _, ok := cm.AcquireFacet("scout"); ok {
		t.Fatal("a not-yet-connected facet bot was handed out")
	}

	sw.succeed()
	eventually(t, "primary to attach after connect succeeds", func() bool { return cm.Primary("scout") != nil })
	conn := cm.Primary("scout")
	if conn.Username() == "" {
		t.Error("attached bot has no username: identity from getMe was not applied")
	}
	if err := conn.SendToSession("scout/c42", "hello after attach"); err != nil {
		t.Fatalf("send via attached bot: %v", err)
	}
	if got := sw.client.sent(); len(got) != 1 || !strings.Contains(got[0], "hello after attach") {
		t.Fatalf("sendMessage texts = %q, want the delivered message", got)
	}
	eventually(t, "facet bot to attach", func() bool {
		f, ok := cm.AcquireFacet("scout")
		return ok && f != nil
	})
}

func TestBackgroundConnect_ShutdownCancelsPendingRetry(t *testing.T) {
	sw := withSwitchableTelegram(t)
	mgr := NewBotManager()
	p := setupAgentFixture(t)
	within(t, "SetupAgent", func() { SetupAgent(mgr, p) })

	p.AgentConfig.Platforms[0].FacetBots = nil // one connect loop, so attempts are countable

	ctx, cancel := context.WithCancel(context.Background())
	sw.cancelAt, sw.shutdownAt = 3, cancel
	within(t, "StartAll", func() { mgr.StartAll(ctx) })
	eventually(t, "shutdown during attempt 3", func() bool { return ctx.Err() != nil })
	within(t, "Wait after shutdown", mgr.Wait)
	// Cancelled during attempt 3: the loop must stop there. Checked again
	// after 20 retry periods, so a loop still running would show up.
	time.Sleep(20 * time.Millisecond)
	if got := sw.attempts.Load(); got != 3 {
		t.Fatalf("connect attempts = %d after shutdown during attempt 3, want 3", got)
	}
	if platform.NewConnectionManagerAdapter[*Bot](mgr).Primary("scout") != nil {
		t.Fatal("bot attached although its connect was cancelled")
	}
}

// A permanent (auth) error still stops the retry at once: the bot never
// attaches and nothing keeps hammering the API.
func TestBackgroundConnect_PermanentErrorStopsRetry(t *testing.T) {
	var attempts atomic.Int32
	withStubFactory(t, func(string, *gotgbot.BotOpts) (*gotgbot.Bot, error) {
		attempts.Add(1)
		return nil, errors.New("Unauthorized")
	})
	orig := defaultConnectBackoff
	defaultConnectBackoff = netretry.Backoff{InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 2}
	t.Cleanup(func() { defaultConnectBackoff = orig })

	mgr := NewBotManager()
	p := setupAgentFixture(t)
	p.AgentConfig.Platforms[0].FacetBots = nil
	within(t, "SetupAgent", func() { SetupAgent(mgr, p) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	within(t, "StartAll", func() { mgr.StartAll(ctx) })
	// No cancel: the connect goroutine must end on its own.
	within(t, "the connect goroutine to give up on a permanent error", mgr.Wait)
	if got := attempts.Load(); got != 1 {
		t.Errorf("getMe attempts = %d, want 1 (auth failure must not retry)", got)
	}
	if platform.NewConnectionManagerAdapter[*Bot](mgr).Primary("scout") != nil {
		t.Error("bot attached despite a rejected token")
	}
}

// A facet bot's persisted session is keyed by its username, which only getMe
// supplies — so it can only be restored once the facet connects, which is now
// after RestoreFacetSessions has run. The restore must happen on connect,
// before the facet is live, or the session is silently lost across a restart.
func TestBackgroundConnect_FacetSessionRestoredOnConnect(t *testing.T) {
	sw := withSwitchableTelegram(t)
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.NewStore(t.TempDir())
	liveKey := "scout/c42/b1"
	if err := sessions.TestAppend(liveKey, provider.Message{Role: "user"}); err != nil {
		t.Fatal(err)
	}
	// The fixture's facet token is "tok2"; the stub factory names it bot-tok2.
	if err := idx.SetAgentMetadata("_system", "facet:bot-tok2", liveKey); err != nil {
		t.Fatal(err)
	}

	fx := setupAgentFixture(t)
	prov := &telegramProvider{
		mgr: NewBotManager(),
		deps: platform.ProviderDeps{
			Config: fx.GlobalConfig, SecretStore: fx.SecretStore, SessionIndex: idx, Sessions: sessions,
			Ctx: fx.Ctx, ResolveSTT: fx.ResolveSTT, ResolveTTS: fx.ResolveTTS,
		},
	}
	// Through the provider, as boot does, so its restore hook is the one wired.
	within(t, "SetupAgentConnection", func() {
		prov.SetupAgentConnection(platform.AgentConnectionParams{
			AgentID: "scout", Handler: fx.Agent, Commands: fx.Commands, LastMsgStore: fx.LastMsgStore,
			AgentConfig: fx.AgentConfig, Resolved: fx.Resolved,
		})
	})
	prov.mgr.RegisteredPrimary("scout").SetChatID(42)

	prov.RestoreFacetSessions(platform.RestoreParams{
		AgentOrder: []string{"scout"},
		Resolver: func(string) (platform.MessageHandler, any, any, config.AgentConfig, bool) {
			return fx.Agent, fx.Commands, fx.CommandContext, fx.AgentConfig, true
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); prov.mgr.Wait() }()
	prov.mgr.StartAll(ctx)
	sw.succeed()

	cm := platform.NewConnectionManagerAdapter[*Bot](prov.mgr)
	eventually(t, "the facet to attach holding its restored session", func() bool { return cm.ForSession(liveKey) != nil })
	if got := cm.ForSession(liveKey).ChatID(); got != 42 {
		t.Errorf("restored facet chat = %d, want the primary's 42", got)
	}
}

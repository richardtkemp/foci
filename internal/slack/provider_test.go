package slack

// provider_test.go: the provider contract (#2459 req 1, 6) — credentials from
// the secrets store, inertness on a credential problem (never a gateway
// abort), no credential in any log, the platform trigger, and the
// never-default/default-config facts.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/secrets"
	"foci/internal/warnings"
)

// providerDeps builds the deps Init needs: a config with a slack platform
// pointed at the fake and a secrets store holding the tokens.
func providerDeps(t *testing.T, fake *fakeSlack, botToken, appToken string) platform.ProviderDeps {
	t.Helper()
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, "secrets.toml")
	body := ""
	if botToken != "" {
		body += "slack.bot_token = \"" + botToken + "\"\n"
	}
	if appToken != "" {
		body += "slack.app_token = \"" + appToken + "\"\n"
	}
	if botToken != "" || appToken != "" {
		if err := os.WriteFile(secretsPath, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := secrets.Load(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Platforms: []config.PlatformConfig{{
			ID:    platformName,
			Slack: &config.SlackSpecific{DefaultAgent: "helen", APIBase: fake.apiBase()},
		}},
	}
	agents := []config.AgentConfig{{ID: "helen"}, {ID: "bob"}}
	cfg.Agents = agents
	idx := newTestIndex(t)
	return platform.ProviderDeps{Config: cfg, SecretStore: store, SessionIndex: idx, Ctx: context.Background()}
}

// TestProvider_IsConfiguredNeedsPlatformEntry proves IsConfigured is
// config-only: true with a slack entry regardless of secrets, false without.
func TestProvider_IsConfiguredNeedsPlatformEntry(t *testing.T) {
	p := &slackProvider{}
	if ok, reason := p.IsConfigured(&config.Config{}); ok || reason == "" {
		t.Errorf("IsConfigured(no entry) = %v, %q; want false with a reason", ok, reason)
	}
	cfg := &config.Config{Platforms: []config.PlatformConfig{{ID: platformName, Slack: &config.SlackSpecific{}}}}
	if ok, _ := p.IsConfigured(cfg); !ok {
		t.Error("IsConfigured(with entry) = false; the entry is enough (credentials are Init's business)")
	}
}

// TestProvider_CredentialsFromSecretStore proves the tokens travel the
// secrets store: the bot token authorises Web API calls, the app-level token
// only apps.connections.open, and custom *_secret keys are honoured.
func TestProvider_CredentialsFromSecretStore(t *testing.T) {
	f := newFakeSlack(t)
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, "secrets.toml")
	if err := os.WriteFile(secretsPath, []byte(strings.Join([]string{
		`slack.custom_bot = "xoxb-custom"`,
		`slack.custom_app = "xapp-custom"`,
		"", "",
	}, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := secrets.Load(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Platforms: []config.PlatformConfig{{
			ID: platformName,
			Slack: &config.SlackSpecific{
				DefaultAgent:   "helen",
				BotTokenSecret: "slack.custom_bot",
				AppTokenSecret: "slack.custom_app",
				APIBase:        f.apiBase(),
			},
		}},
		Agents: []config.AgentConfig{{ID: "helen"}},
	}
	p := &slackProvider{}
	if err := p.Init(platform.ProviderDeps{Config: cfg, SecretStore: store, SessionIndex: newTestIndex(t), Ctx: context.Background()}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.mgr == nil {
		t.Fatal("Init left the provider inert despite healthy credentials")
	}
	p.mgr.StartAll(context.Background())
	f.waitSocket(t)

	for _, c := range f.callsSnapshot() {
		switch c.Method {
		case "apps.connections.open":
			if c.Auth != "Bearer xapp-custom" {
				t.Errorf("apps.connections.open auth = %q, want the app-level token", c.Auth)
			}
		case "auth.test":
			if c.Auth != "Bearer xoxb-custom" {
				t.Errorf("auth.test auth = %q, want the bot token", c.Auth)
			}
		default:
			t.Errorf("unexpected method %q during boot", c.Method)
		}
	}
}

// TestProvider_InitInertOnCredentialProblem proves a credential problem
// leaves the provider inert (nil error!) with one ERROR per problem naming
// the secret KEY — the gateway starts, Telegram keeps running — and that a
// nil secrets store counts as every secret missing instead of panicking.
func TestProvider_InitInertOnCredentialProblem(t *testing.T) {
	f := newFakeSlack(t)
	cases := []struct {
		name    string
		botTok  string
		appTok  string
		nilSess bool
		want    []string
	}{
		{"missing bot secret", "", "xapp-ok", false, []string{config.DefaultSlackBotTokenSecret}},
		{"missing app secret", "xoxb-ok", "", false, []string{config.DefaultSlackAppTokenSecret}},
		{"empty bot value", "", "xapp-ok", false, []string{config.DefaultSlackBotTokenSecret}},
		{"wrong bot prefix", "nope", "xapp-ok", false, []string{config.DefaultSlackBotTokenSecret}},
		{"wrong app prefix", "xoxb-ok", "nope", false, []string{config.DefaultSlackAppTokenSecret}},
		{"nil secrets store", "ignored", "ignored", true, []string{
			config.DefaultSlackBotTokenSecret, config.DefaultSlackAppTokenSecret,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogLines(t, log.ERROR)
			deps := providerDeps(t, f, tc.botTok, tc.appTok)
			if tc.nilSess {
				deps.SecretStore = nil
			}
			p := &slackProvider{}
			if err := p.Init(deps); err != nil {
				t.Fatalf("Init returned %v; an inert provider must return nil (an error aborts the whole gateway)", err)
			}
			if p.mgr != nil {
				t.Fatal("provider is live despite a credential problem")
			}
			// Inert surface: no connections, no setup, no StartAll work.
			if c := p.ConnectionManager().Primary("helen"); c != nil {
				t.Error("inert provider exposes a primary connection")
			}
			if res := p.SetupAgentConnection(platform.AgentConnectionParams{AgentID: "helen"}); res != nil {
				t.Error("inert provider set up an agent connection")
			}
			p.SetLifecycleCallback("helen", platform.OnUserMessage, func() { t.Error("inert lifecycle fired") })
			p.ConnectionManager().StartAll(context.Background())
			p.ConnectionManager().Wait()
			if got := f.totalCalls(); got != 0 {
				t.Errorf("inert provider made %d requests, want 0", got)
			}
			errLines := logs()
			if len(errLines) != len(tc.want) {
				t.Fatalf("ERROR lines = %d (%v), want %d", len(errLines), errLines, len(tc.want))
			}
			for _, key := range tc.want {
				found := false
				for _, l := range errLines {
					if strings.Contains(l, key) && strings.Contains(l, "slack") {
						found = true
					}
				}
				if !found {
					t.Errorf("no ERROR line names secret key %q: %v", key, errLines)
				}
			}
		})
	}

	t.Run("healthy credentials are live (positive control)", func(t *testing.T) {
		deps := providerDeps(t, f, "xoxb-ok", "xapp-ok")
		p := &slackProvider{}
		if err := p.Init(deps); err != nil {
			t.Fatal(err)
		}
		if p.mgr == nil {
			t.Fatal("healthy credentials left the provider inert")
		}
		p.mgr.StartAll(context.Background())
		f.waitSocket(t)
		if got := len(f.callsFor("auth.test")); got != 1 {
			t.Errorf("auth.test calls = %d, want 1", got)
		}
	})
}

// TestNoCredentialInLogs proves neither token appears in captured logs on
// the failure path that carries the most context (a failed auth.test) — with
// a positive control that the error itself IS logged.
func TestNoCredentialInLogs(t *testing.T) {
	f := newFakeSlack(t)
	f.FailAuth = "invalid_auth"
	logs := captureLogLines(t, log.ERROR)

	deps := providerDeps(t, f, "xoxb-secretvalue", "xapp-secretvalue")
	p := &slackProvider{}
	if err := p.Init(deps); err != nil {
		t.Fatal(err)
	}
	p.mgr.StartAll(context.Background())

	// auth.test fails fast (a permanent marker): poll for the ERROR, then
	// check every captured line for the tokens.
	deadline := time.Now().Add(2 * time.Second)
	var errLines []string
	for time.Now().Before(deadline) {
		if errLines = logs(); len(errLines) > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if len(errLines) == 0 {
		t.Fatal("positive control failed: no ERROR lines captured, so the redaction proof proves nothing")
	}
	for _, l := range errLines {
		for _, secret := range []string{"xoxb-secretvalue", "xapp-secretvalue"} {
			if strings.Contains(l, secret) {
				t.Errorf("ERROR line leaks a token: %q", l)
			}
		}
	}
}

// TestSlackProviderIsNeverDefault proves the provider declares the
// never-default capability.
func TestSlackProviderIsNeverDefault(t *testing.T) {
	var p platform.MessagingProvider = &slackProvider{}
	nd, ok := p.(platform.NeverDefaultProvider)
	if !ok || !nd.NeverDefaultChat() {
		t.Error("slack provider does not declare NeverDefaultChat")
	}
}

// TestSlackProviderConfinesSessionReplies proves the provider declares the
// session-reply confinement at PLATFORM level (#2459 req 5): the aggregating
// manager serves this answer for a session with no live connection, so a
// Slack session is never fanned out to other platforms' chats.
func TestSlackProviderConfinesSessionReplies(t *testing.T) {
	var p platform.MessagingProvider = &slackProvider{}
	cp, ok := p.(platform.ReplyConfiningProvider)
	if !ok || !cp.ConfinesSessionReplies() {
		t.Error("slack provider does not declare ConfinesSessionReplies")
	}
}

// TestStartupNotifyOffByDefault proves the platform defaults: no startup
// notice (nowhere to send it) and streaming off (chat.update is Tier 3).
func TestStartupNotifyOffByDefault(t *testing.T) {
	dflt := (&slackProvider{}).DefaultPlatformConfig()
	if dflt.Notify.StartupNotify == nil || *dflt.Notify.StartupNotify {
		t.Error("startup_notify default is not false; agent-wide notices have no Slack destination")
	}
	if dflt.Display.StreamOutput == nil || *dflt.Display.StreamOutput {
		t.Error("stream_output default is not false")
	}
	if dflt.Display.ShowToolCalls == nil || *dflt.Display.ShowToolCalls != config.ToolCallOff {
		t.Error("show_tool_calls default is not off")
	}
	// The one cadence constant is the source of this default too (not a
	// second "2s" literal): parseStreamInterval's fallback and the config
	// default must agree.
	if dflt.Display.StreamInterval == nil || *dflt.Display.StreamInterval != defaultStreamInterval.String() {
		t.Errorf("stream_interval default = %v, want the defaultStreamInterval constant %q", dflt.Display.StreamInterval, defaultStreamInterval.String())
	}
}

// TestPlatformTriggerRegistered proves the init() registration reaches the
// agent's trigger set: the "slack" trigger counts as a platform trigger, so
// Slack turns are interactive (steer-able mid-turn) and triggerToPlatform
// echoes "slack" as the [meta] platform header. Without the registration
// both would mislabel Slack turns as system-initiated.
func TestPlatformTriggerRegistered(t *testing.T) {
	if !agent.IsPlatformTrigger(platformName) {
		t.Errorf("%q is not registered as a platform trigger; Slack turns would not be interactive and the [meta] platform header would mislabel them", platformName)
	}
	if p := platform.GetProvider(platformName); p == nil {
		t.Fatal("slack provider not registered under its name")
	} else if p.Name() != platformName {
		t.Errorf("registered provider name = %q", p.Name())
	}
}

// setupHandler is the agent stand-in for SetupAgentConnection tests: it
// satisfies platform.MessageHandler (the params type) and inboxAgent (the
// interface SetupAgentConnection narrows the handler to).
type setupHandler struct {
	fakeAgent
}

func (h *setupHandler) HandleMessage(context.Context, string, []string, []platform.Attachment) error {
	return nil
}
func (h *setupHandler) TransformMessage(string) string { return "" }
func (h *setupHandler) Warnings() *warnings.Queue      { return nil }

// TestProvider_StreamIntervalFromConfig proves the configured display
// stream_interval reaches the agent's core — the cadence NewTurnSink hands
// turn.NewStreamBuffer — with the documented default when unset (previously
// the setting was read into the resolved config and then ignored for a
// hard-coded constant).
func TestProvider_StreamIntervalFromConfig(t *testing.T) {
	f := newFakeSlack(t)
	deps := providerDeps(t, f, "xoxb-ok", "xapp-ok")
	p := &slackProvider{}
	if err := p.Init(deps); err != nil {
		t.Fatal(err)
	}
	if p.mgr == nil {
		t.Fatal("Init left the provider inert despite healthy credentials")
	}

	setupCore := func() time.Duration {
		t.Helper()
		ag := &setupHandler{}
		acfg := config.AgentConfig{ID: "helen"}
		params := platform.AgentConnectionParams{
			AgentID:     "helen",
			Handler:     ag,
			AgentConfig: acfg,
			Resolved:    config.Resolve(deps.Config, acfg),
		}
		if res := p.SetupAgentConnection(params); res == nil {
			t.Fatal("SetupAgentConnection returned nil for the default agent")
		}
		core := p.mgr.core("helen")
		if core == nil {
			t.Fatal("no core registered")
		}
		return core.streamInterval
	}

	// Unset: the default cadence.
	if got := setupCore(); got != defaultStreamInterval {
		t.Errorf("streamInterval with no config = %v, want the default %v", got, defaultStreamInterval)
	}
	// Configured: the configured cadence, live on the next setup.
	interval := "5s"
	deps.Config.Platforms[0].Display.StreamInterval = &interval
	if got := setupCore(); got != 5*time.Second {
		t.Errorf("streamInterval with display.stream_interval=5s = %v, want 5s", got)
	}
}

// captureLogLines taps the shared event log at min level until the test ends
// and returns a snapshot function. A tap sees every parallel test's lines;
// positive assertions must match on tokens only this run logs, negative ones
// (a secret never logged) hold globally.
func captureLogLines(t *testing.T, min log.Level) func() []string {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	remove := log.AddTap(min, func(level log.Level, component, msg string) {
		mu.Lock()
		lines = append(lines, msg)
		mu.Unlock()
	})
	t.Cleanup(remove)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

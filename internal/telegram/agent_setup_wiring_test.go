package telegram

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/session"
	"foci/internal/voice"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// fakeSTT/fakeTTS are inert voice providers for wiring tests.
type fakeSTT struct{}

func (fakeSTT) Transcribe(_ context.Context, _ []byte, _ string) (string, error) { return "", nil }

type fakeTTS struct{}

func (fakeTTS) Synthesize(_ context.Context, _ string) ([]byte, error) { return nil, nil }

// setupAgentFixture returns AgentSetupParams for agent "scout" with the bot
// token secret present, plus the global config used.
func setupAgentFixture(t *testing.T) AgentSetupParams {
	t.Helper()
	store := emptySecretStore(t)
	store.Set("telegram.scoutbot", "tok")
	store.Set("telegram.facetbot", "tok2")

	cfg := &config.Config{
		Platforms: []config.PlatformConfig{{
			ID:     "telegram",
			Access: config.AccessConfig{AllowedUsers: []string{"111"}},
		}},
	}
	acfg := config.AgentConfig{
		ID: "scout",
		Platforms: []config.PlatformConfig{{
			ID:        "telegram",
			Bot:       "scoutbot",
			FacetBots: []string{"facetbot"},
			Access:    config.AccessConfig{AllowedUsers: []string{"222"}},
		}},
	}
	return AgentSetupParams{
		Agent:        &mockHandler{},
		Commands:     command.NewRegistry(),
		LastMsgStore: command.NewLastMessageStore(),
		AgentConfig:  acfg,
		GlobalConfig: cfg,
		SecretStore:  store,
		Ctx:          context.Background(),
		Resolved:     config.Resolve(cfg, acfg),
		ResolveSTT: func(map[string]voice.STT, []config.STTConfig, string, map[string]string) voice.STT {
			return fakeSTT{}
		},
		ResolveTTS: func(map[string]voice.TTS, []config.TTSConfig, string, float64, map[string]string) voice.TTS {
			return fakeTTS{}
		},
	}
}

func TestSetupAgent_RegistersPrimaryAndFacetBots(t *testing.T) {
	// Proves SetupAgent (with a stubbed Telegram factory) creates the primary
	// bot under the agent's ID, builds the per-agent facet pool, merges
	// agent+global allowed users, and returns a usable SetupResult.
	withStubFactory(t, func(token string, opts *gotgbot.BotOpts) (*gotgbot.Bot, error) {
		return &gotgbot.Bot{User: gotgbot.User{Id: 99, Username: "bot-" + token}, BotClient: &fakeBotClient{}}, nil
	})

	mgr := NewBotManager()
	res := SetupAgent(mgr, setupAgentFixture(t))
	if res == nil {
		t.Fatal("SetupAgent returned nil")
	}

	primary := mgr.PrimaryBot("scout")
	if primary == nil {
		t.Fatal("primary bot not registered")
	}
	if !primary.allowedUsers["111"] || !primary.allowedUsers["222"] {
		t.Errorf("allowed users = %v, want merged agent+global", primary.allowedUsers)
	}
	if pool := mgr.Pool("scout"); pool == nil || pool.Size() != 1 {
		t.Fatalf("facet pool missing or wrong size")
	}

	// DisplayDefaultsFn reflects the primary bot's resolved settings.
	ds := res.DisplayDefaultsFn()
	if ds.StreamOutput != "off" {
		t.Errorf("StreamOutput default = %q, want off", ds.StreamOutput)
	}

	// ConfigureFacetConn rewires a facet connection for this agent.
	facet, ok := mgr.AcquireFacet("scout")
	if !ok {
		t.Fatal("no facet to acquire")
	}
	res.ConfigureFacetConn(facet)
	if facet.handler == nil || facet.dispatcher == nil {
		t.Error("ConfigureFacetConn did not rewire handler/dispatcher")
	}
}

func TestSetupAgent_DisplaySettingsLiveUpdateViaOnChange(t *testing.T) {
	// Proves the #1224 OnChange hook: when ResolvedLive.Store fires a fresh
	// ResolvedAgentConfig with changed stream_output/messages_in_log/table
	// settings/long-poll timeout, the primary bot's live display state picks
	// them up without a reconnect — not just that the registry has an
	// applier (TestLiveApplyCoversHotFields, cmd/foci-gw), but that this
	// package's own OnChange callback actually applies the change.
	withStubFactory(t, func(token string, opts *gotgbot.BotOpts) (*gotgbot.Bot, error) {
		return &gotgbot.Bot{User: gotgbot.User{Id: 99, Username: "bot-" + token}, BotClient: &fakeBotClient{}}, nil
	})

	p := setupAgentFixture(t)
	initial := config.Resolve(p.GlobalConfig, p.AgentConfig)
	live := config.NewLiveValue(initial)
	p.Resolved = initial
	p.ResolvedLive = live

	mgr := NewBotManager()
	if res := SetupAgent(mgr, p); res == nil {
		t.Fatal("SetupAgent returned nil")
	}
	primary := mgr.PrimaryBot("scout")
	if primary == nil {
		t.Fatal("primary bot not registered")
	}

	d := primary.getDisplay()
	if d.StreamOutput || d.MessagesInLog || d.TableWrapLines != 0 || d.TableStyle != "" {
		t.Fatalf("initial display = %+v, want all zero/off", d)
	}

	// Build a fresh Config/AgentConfig with the hot fields turned on, resolve
	// it, and store — this is what a live config-file edit ultimately does.
	freshCfg := &config.Config{
		Display: config.DisplayConfig{
			StreamOutput:   config.Ptr(true),
			TableWrapLines: config.Ptr(12),
			TableStyle:     config.Ptr("markdown"),
		},
		Debug: config.DebugConfig{MessagesInLog: config.Ptr(true)},
		Platforms: []config.PlatformConfig{{
			ID:       "telegram",
			Access:   p.GlobalConfig.Platforms[0].Access,
			Telegram: &config.TelegramSpecific{LongPollTimeout: "20s"},
		}},
	}
	fresh := config.Resolve(freshCfg, p.AgentConfig)
	live.Store(fresh)

	d = primary.getDisplay()
	if !d.StreamOutput {
		t.Error("StreamOutput not live-updated to true")
	}
	if !d.MessagesInLog {
		t.Error("MessagesInLog not live-updated to true")
	}
	if d.TableWrapLines != 12 {
		t.Errorf("TableWrapLines = %d, want 12", d.TableWrapLines)
	}
	if d.TableStyle != "markdown" {
		t.Errorf("TableStyle = %q, want markdown", d.TableStyle)
	}
	if primary.getLongPollTimeout() != 20*time.Second {
		t.Errorf("longPollTimeout = %v, want 20s", primary.getLongPollTimeout())
	}
}

func TestSetupAgent_NoTokenReturnsNil(t *testing.T) {
	// Proves SetupAgent returns nil (agent runs without platform) when the
	// bot token secret is missing.
	p := setupAgentFixture(t)
	p.SecretStore = emptySecretStore(t)
	if res := SetupAgent(NewBotManager(), p); res != nil {
		t.Errorf("res = %v, want nil without token", res)
	}
}

func TestSetupAgent_NoTelegramPlatform(t *testing.T) {
	// Proves an agent without a telegram platform entry is skipped cleanly.
	p := setupAgentFixture(t)
	p.AgentConfig.Platforms = nil
	if res := SetupAgent(NewBotManager(), p); res != nil {
		t.Errorf("res = %v, want nil without platform config", res)
	}
}

func TestResolveAllowedUsers(t *testing.T) {
	// Proves agent and global telegram allowed users are merged with
	// deduplication, and either side may be empty.
	mkAgent := func(users ...string) config.AgentConfig {
		return config.AgentConfig{Platforms: []config.PlatformConfig{{
			ID: "telegram", Access: config.AccessConfig{AllowedUsers: users},
		}}}
	}
	mkGlobal := func(users ...string) *config.Config {
		return &config.Config{Platforms: []config.PlatformConfig{{
			ID: "telegram", Access: config.AccessConfig{AllowedUsers: users},
		}}}
	}

	got := resolveAllowedUsers(mkAgent("111", "222"), mkGlobal("222", "333"))
	if len(got) != 3 {
		t.Errorf("merged = %v, want 3 deduplicated users", got)
	}
	if got := resolveAllowedUsers(config.AgentConfig{}, mkGlobal("333")); len(got) != 1 || got[0] != "333" {
		t.Errorf("global only = %v, want [333]", got)
	}
	if got := resolveAllowedUsers(mkAgent("111"), &config.Config{}); len(got) != 1 || got[0] != "111" {
		t.Errorf("agent only = %v, want [111]", got)
	}
}

func TestConfigureFacetBot_WiresSessionPersistence(t *testing.T) {
	// Proves ConfigureFacetBot installs voice providers and an
	// OnSessionKeyChange hook that persists the facet→session mapping in the
	// session index on assignment and deletes it on release.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatalf("session index: %v", err)
	}
	defer func() { _ = idx.Close() }()

	b, _ := testBot(nil, command.NewRegistry())
	b.api = &gotgbot.Bot{User: gotgbot.User{Id: 1, Username: "facetbot"}}

	cfg := &config.Config{}
	acfg := config.AgentConfig{ID: "scout"}
	ConfigureFacetBot(b, FacetBotConfig{
		STTProvider:  fakeSTT{},
		TTSProvider:  fakeTTS{},
		AgentConfig:  acfg,
		GlobalConfig: cfg,
		Resolved:     config.Resolve(cfg, acfg),
		SessionIndex: idx,
	})
	if b.transcriber == nil || b.tts == nil {
		t.Error("voice providers not installed")
	}

	b.SetSessionKey("agent:scout:branch:x")
	m, err := idx.AgentMetadataByPrefix("_system", "facet:")
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if m["facet:facetbot"] != "agent:scout:branch:x" {
		t.Errorf("persisted = %v, want facet:facetbot mapping", m)
	}

	b.SetSessionKey("")
	m, _ = idx.AgentMetadataByPrefix("_system", "facet:")
	if _, ok := m["facet:facetbot"]; ok {
		t.Error("mapping should be deleted on release")
	}
}

func TestApplyAgentDisplaySettings_TelegramSpecific(t *testing.T) {
	// Proves the telegram-specific knobs (table wrap/style, long-poll
	// timeout, stream interval, injected header) flow into the bot's display
	// config, with invalid durations ignored.
	b := newBotForTest()
	dc := config.ResolvedDisplay{
		StreamInterval:        "750ms",
		InjectedMessageHeader: "[sys]",
		TableWrapLines:        9,
		TableStyle:            "markdown",
	}
	ApplyAgentDisplaySettings(b, dc, config.ResolvedDebug{}, "42s")

	d := b.getDisplay()
	if d.TableWrapLines != 9 || d.TableStyle != "markdown" {
		t.Errorf("table opts = %d/%q, want 9/markdown", d.TableWrapLines, d.TableStyle)
	}
	if b.getLongPollTimeout().Seconds() != 42 {
		t.Errorf("longPollTimeout = %v, want 42s", b.getLongPollTimeout())
	}
	if d.StreamUpdateInterval.Milliseconds() != 750 {
		t.Errorf("stream interval = %v, want 750ms", d.StreamUpdateInterval)
	}
	if d.InjectedMessageHeader != "[sys]" {
		t.Errorf("injected header = %q", d.InjectedMessageHeader)
	}

	// Invalid long-poll duration is ignored.
	prev := b.getLongPollTimeout()
	ApplyAgentDisplaySettings(b, config.ResolvedDisplay{}, config.ResolvedDebug{}, "bogus")
	if b.getLongPollTimeout() != prev {
		t.Errorf("bogus duration changed timeout to %v", b.getLongPollTimeout())
	}
}

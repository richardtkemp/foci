package main

// #963: /agents new starts the new agent in the running process. These tests
// drive liveAgentStarter with the real registry, readers and bot-conflict
// rule, and a fake build/start so no backend or platform is needed.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// mapSecrets is a config.SecretGetter over a map.
type mapSecrets map[string]string

func (m mapSecrets) Get(k string) (string, bool) { v, ok := m[k]; return v, ok }

// hotAddFixture is a starter over d.agents whose config holds the running
// agent plus the ids in newIDs, each built as a real mock-backed agent.
type hotAddFixture struct {
	d       httpHandlerDeps
	mock    *mockClient
	starter *liveAgentStarter
	builds  atomic.Int32
	starts  atomic.Int32
	// visibleDuringBuild records whether a build ever saw its own id in the
	// registry (it must not: the agent is published only once built).
	visibleDuringBuild atomic.Bool
}

func newHotAddFixture(t *testing.T, cfgAgents []config.AgentConfig, secrets config.SecretGetter) *hotAddFixture {
	t.Helper()
	d, mock := httpTestSetup(t, httpTestOpts{})
	f := &hotAddFixture{d: d, mock: mock}
	running := d.agents.at(testAgentID)
	running.agentCfg = config.AgentConfig{ID: testAgentID}
	if secrets == nil {
		secrets = mapSecrets{}
	}
	f.starter = &liveAgentStarter{
		agents: d.agents,
		load: func() (*config.Config, error) {
			return &config.Config{Agents: append([]config.AgentConfig{running.agentCfg}, cfgAgents...)}, nil
		},
		prepare: func(*config.AgentConfig) {},
		conflict: func(acfg config.AgentConfig) error {
			return hotAddBotConflict(d.agents.Configs(), acfg, secrets)
		},
		build: func(acfg config.AgentConfig) (*agentInstance, []string, error) {
			f.builds.Add(1)
			if _, ok := d.agents.Get(acfg.ID); ok {
				f.visibleDuringBuild.Store(true)
			}
			return newTestAgentInstance(t, d, mock, acfg), nil, nil
		},
		start: func(*agentInstance) { f.starts.Add(1) },
	}
	return f
}

// newTestAgentInstance builds a mock-backed agent like the HTTP harness's,
// with its own registered default session and a started inbox.
func newTestAgentInstance(t *testing.T, d httpHandlerDeps, mock *mockClient, acfg config.AgentConfig) *agentInstance {
	t.Helper()
	ag := &agent.Agent{
		Client:       mock,
		Sessions:     d.sessions,
		SessionIndex: d.sessionIndex,
		Tools:        tools.NewRegistry(),
		Bootstrap:    workspace.NewBootstrap(t.TempDir(), nil),
		Model:        "test-model",
	}
	d.sessionIndex.Upsert(session.SessionIndexEntry{SessionKey: acfg.ID + "/i0", FilePath: "x", SessionType: session.SessionTypeChat, Status: session.SessionStatusActive})
	inst := &agentInstance{
		id:       acfg.ID,
		ag:       ag,
		agentCfg: acfg,
		resolved: config.NewLiveValue(&config.ResolvedAgentConfig{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		// As httpTestSetup: let a finished request's turn write its session
		// file before the TempDir goes. Bounded, a hang guard only.
		deadline := time.Now().Add(10 * time.Second)
		for ag.IsAnyTurnInFlight() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	})
	ag.StartInbox(ctx)
	return inst
}

func TestHotAdd_NewAgentReachesEveryReader(t *testing.T) {
	f := newHotAddFixture(t, []config.AgentConfig{{ID: "newbie"}}, nil)

	if _, err := f.starter.Start("newbie"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if f.visibleDuringBuild.Load() {
		t.Error("the agent was visible to readers before it was fully built")
	}
	if f.starts.Load() != 1 {
		t.Errorf("connections started %d times, want 1", f.starts.Load())
	}

	// /agents list (and the wizard's uniqueness check).
	list := newAgentLister(f.d.agents)()
	if len(list) != 2 || list[0].ID != testAgentID || list[1].ID != "newbie" {
		t.Errorf("agent list = %+v, want [%s newbie] in that order", list, testAgentID)
	}
	// The gateway resolver (session_notify, asks).
	if inst := newAgentResolver(f.d.agents)("newbie"); inst == nil || inst.id != "newbie" {
		t.Errorf("agentResolverFn(newbie) = %v", inst)
	}
	// The HTTP resolver, and the default agent unchanged.
	resolveAgent, _ := buildResolvers(f.d)
	if inst, ok := resolveAgent("newbie"); !ok || inst.id != "newbie" {
		t.Errorf("HTTP resolveAgent(newbie) = %v, %v", inst, ok)
	}
	if inst, ok := resolveAgent(""); !ok || inst.id != testAgentID {
		t.Errorf("default agent after hot-add = %v, want %s", inst, testAgentID)
	}

	// A real /send to the new agent gets a reply, with no restart.
	w := postJSON(newTestMux(f.d), "/send", `{"agent":"newbie","session":"newbie/i0","text":"hello new agent"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("/send to the hot-added agent: status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), mockReply) {
		t.Errorf("/send reply = %s, want the backend's %q", w.Body.String(), mockReply)
	}
	if got := f.mock.lastText(); !strings.Contains(got, "hello new agent") {
		t.Errorf("backend saw %q, want the sent text", got)
	}
}

func TestHotAdd_DuplicateIDIsRefused(t *testing.T) {
	f := newHotAddFixture(t, []config.AgentConfig{{ID: "newbie"}}, nil)

	// The running agent's id.
	if _, err := f.starter.Start(testAgentID); err == nil {
		t.Fatal("Start of a running agent's id succeeded")
	}
	// A second start of a hot-added id.
	if _, err := f.starter.Start("newbie"); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, err := f.starter.Start("newbie"); err == nil {
		t.Fatal("second Start of the same id succeeded")
	}
	if n := f.builds.Load(); n != 1 {
		t.Errorf("builds = %d, want 1 (a refused id must build nothing)", n)
	}
	if n := f.d.agents.Len(); n != 2 {
		t.Errorf("registered agents = %d, want 2", n)
	}
}

func TestHotAdd_DuplicateBotTokenIsRefused(t *testing.T) {
	secrets := mapSecrets{"telegram.old-bot": "SAME-TOKEN", "telegram.new-bot": "SAME-TOKEN"}
	newCfg := config.AgentConfig{ID: "newbie", Platforms: []config.PlatformConfig{{ID: "telegram", Bot: "new-bot"}}}
	f := newHotAddFixture(t, []config.AgentConfig{newCfg}, secrets)
	f.d.agents.at(testAgentID).agentCfg.Platforms = []config.PlatformConfig{{ID: "telegram", Bot: "old-bot"}}

	_, err := f.starter.Start("newbie")
	if err == nil {
		t.Fatal("Start with a running agent's bot token succeeded")
	}
	if !strings.Contains(err.Error(), "telegram") || !strings.Contains(err.Error(), testAgentID) {
		t.Errorf("error %q does not name the platform and the clashing agent %q", err, testAgentID)
	}
	if f.builds.Load() != 0 || f.starts.Load() != 0 {
		t.Errorf("builds=%d starts=%d, want 0 and 0", f.builds.Load(), f.starts.Load())
	}
	if _, ok := f.d.agents.Get("newbie"); ok {
		t.Error("refused agent is registered")
	}
}

func TestHotAdd_FailureLeavesNothingRegistered(t *testing.T) {
	cases := []struct {
		name  string
		id    string
		setup func(f *hotAddFixture)
	}{
		{"setup fails", "newbie", func(f *hotAddFixture) {
			f.starter.build = func(config.AgentConfig) (*agentInstance, []string, error) {
				f.builds.Add(1)
				return nil, nil, errors.New("agent setup failed")
			}
		}},
		{"config unreadable", "newbie", func(f *hotAddFixture) {
			f.starter.load = func() (*config.Config, error) { return nil, errors.New("toml: bad") }
		}},
		{"id not in config", "ghost", func(*hotAddFixture) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newHotAddFixture(t, []config.AgentConfig{{ID: "newbie"}}, nil)
			tc.setup(f)
			if _, err := f.starter.Start(tc.id); err == nil {
				t.Fatal("Start succeeded")
			}
			if _, ok := f.d.agents.Get(tc.id); ok {
				t.Error("failed agent is registered")
			}
			if inst := newAgentResolver(f.d.agents)(tc.id); inst != nil {
				t.Error("failed agent reaches the resolver")
			}
			if n := len(newAgentLister(f.d.agents)()); n != 1 {
				t.Errorf("agent list has %d agents, want 1", n)
			}
			if f.starts.Load() != 0 {
				t.Error("connections started for a failed agent")
			}
			// The reservation is released: the id can be started later.
			if err := f.d.agents.reserve(tc.id); err != nil {
				t.Errorf("id still reserved after a failed start: %v", err)
			}
		})
	}
}

func TestHotAdd_ConcurrentAddsAndLookups(t *testing.T) {
	const n = 8
	var cfgs []config.AgentConfig
	for i := 0; i < n; i++ {
		cfgs = append(cfgs, config.AgentConfig{ID: fmt.Sprintf("hot-%d", i)})
	}
	f := newHotAddFixture(t, cfgs, nil)
	resolve := newAgentResolver(f.d.agents)
	list := newAgentLister(f.d.agents)
	resolveAgent, _ := buildResolvers(f.d)

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = list()
				_ = resolve("hot-3")
				if inst, ok := resolveAgent(""); !ok || inst.id != testAgentID {
					t.Errorf("default agent changed under a hot-add: %v", inst)
					return
				}
				for _, inst := range f.d.agents.All() {
					_ = inst.LiveConfig()
				}
			}
		}()
	}

	// Every id started twice at once: exactly one of each pair wins.
	var wins atomic.Int32
	var adders sync.WaitGroup
	for i := 0; i < n; i++ {
		for dup := 0; dup < 2; dup++ {
			adders.Add(1)
			go func(id string) {
				defer adders.Done()
				if _, err := f.starter.Start(id); err == nil {
					wins.Add(1)
				}
			}(fmt.Sprintf("hot-%d", i))
		}
	}
	adders.Wait()
	close(stop)
	readers.Wait()

	if w := wins.Load(); w != n {
		t.Errorf("successful starts = %d, want %d (one per id)", w, n)
	}
	if b := f.builds.Load(); b != n {
		t.Errorf("builds = %d, want %d (nothing built twice)", b, n)
	}
	if got := f.d.agents.Len(); got != n+1 {
		t.Errorf("registered = %d, want %d", got, n+1)
	}
	if order := f.d.agents.Order(); order[0] != testAgentID {
		t.Errorf("first agent = %s, want %s", order[0], testAgentID)
	}
}

func TestHotAdd_StartRefusedAfterClose(t *testing.T) {
	f := newHotAddFixture(t, []config.AgentConfig{{ID: "newbie"}}, nil)
	f.starter.close()
	if _, err := f.starter.Start("newbie"); !errors.Is(err, errShuttingDown) {
		t.Fatalf("Start after close: err = %v, want errShuttingDown", err)
	}
	if f.builds.Load() != 0 {
		t.Error("built after close")
	}
}

func TestMissingTelegramTokenHint(t *testing.T) {
	acfg := config.AgentConfig{ID: "newbie", Platforms: []config.PlatformConfig{{ID: "telegram", Bot: "newbie"}}}
	h := missingTelegramTokenHint(acfg, []string{"app", "telegram"}, mapSecrets{})
	if !strings.Contains(h, "/secrets set telegram.newbie <token>") || !strings.Contains(h, "/restart") {
		t.Errorf("hint = %q, want the secret command and /restart", h)
	}
	if h := missingTelegramTokenHint(acfg, []string{"app", "telegram"}, mapSecrets{"telegram.newbie": "tok"}); h != "" {
		t.Errorf("hint with the token set = %q, want none", h)
	}
	if h := missingTelegramTokenHint(acfg, []string{"app"}, mapSecrets{}); h != "" {
		t.Errorf("hint with Telegram not configured = %q, want none", h)
	}
}

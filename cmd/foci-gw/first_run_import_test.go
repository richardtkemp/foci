package main

// #3049: the first agent on a NEW install gets the one-time import from other
// AI tools with its onboarding; every other agent gets the onboarding alone.
// These tests arm agents through the gateway's own arming paths
// (handleRestartAndFirstRun at boot, armFirstRun as the hot-add calls it) and
// deliver through a real API turn (Agent.HandleMessage → prepareUserMessage →
// the mock provider), over a real session index file that a simulated
// restart closes and reopens.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
	"foci/shared/prompts"
)

// importMarker is the first line of the import instructions; its presence in
// a delivered turn means the agent got the import.
const importMarker = "[IMPORT FROM OTHER AI TOOLS]"

// importInstall is one install's durable state: a session index file and a
// sessions dir. Each "process" opens it afresh (open), so closing and
// reopening it simulates a gateway restart.
type importInstall struct {
	t       *testing.T
	dbPath  string
	sessDir string
	idx     *session.SessionIndex
}

func newImportInstall(t *testing.T) *importInstall {
	t.Helper()
	dir := t.TempDir()
	in := &importInstall{t: t, dbPath: filepath.Join(dir, "state.db"), sessDir: filepath.Join(dir, "sessions")}
	if err := os.MkdirAll(in.sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	in.open()
	return in
}

// open starts a new "process": a fresh SessionIndex over the same file.
func (in *importInstall) open() {
	in.t.Helper()
	idx, err := session.NewSessionIndex(in.dbPath)
	if err != nil {
		in.t.Fatalf("NewSessionIndex: %v", err)
	}
	in.idx = idx
	in.t.Cleanup(func() { _ = idx.Close() })
}

// restart ends the current "process" and starts the next one.
func (in *importInstall) restart() {
	in.t.Helper()
	_ = in.idx.Close()
	in.open()
}

// importAgent is one mock-backed API agent of the current process, with its
// own mock so a test can tell which agent received what.
type importAgent struct {
	inst *agentInstance
	mock *mockClient
	key  string
}

func (in *importInstall) agent(id string) *importAgent {
	in.t.Helper()
	mock := &mockClient{}
	ag := &agent.Agent{
		Client:       mock,
		Sessions:     session.NewStore(in.sessDir),
		SessionIndex: in.idx,
		Tools:        tools.NewRegistry(),
		Bootstrap:    workspace.NewBootstrap(in.t.TempDir(), nil),
		Model:        "test-model",
	}
	key := id + "/i0"
	in.idx.Upsert(session.SessionIndexEntry{SessionKey: key, FilePath: "x", SessionType: session.SessionTypeChat, Status: session.SessionStatusActive})
	return &importAgent{
		inst: &agentInstance{id: id, ag: ag, agentCfg: config.AgentConfig{ID: id}, resolved: config.NewLiveValue(&config.ResolvedAgentConfig{})},
		mock: mock,
		key:  key,
	}
}

// boot arms the agents exactly as a gateway boot does.
func (in *importInstall) boot(agents ...*importAgent) {
	in.t.Helper()
	m := make(map[string]*agentInstance, len(agents))
	order := make([]string, 0, len(agents))
	for _, a := range agents {
		m[a.inst.id] = a.inst
		order = append(order, a.inst.id)
	}
	handleRestartAndFirstRun(m, order, in.idx, &config.Config{}, context.Background(),
		stubConnMgr{}, func(string, func(platform.Connection)) {}, nil)
}

// turn delivers one user message through a real API turn and returns the
// user text the provider received.
func (a *importAgent) turn(t *testing.T, text string) string {
	t.Helper()
	if err := a.inst.ag.HandleMessage(context.Background(), a.key, []string{text}, nil); err != nil {
		t.Fatalf("%s: HandleMessage: %v", a.inst.id, err)
	}
	return a.mock.lastText()
}

func checkOnboarding(t *testing.T, id, got string, wantImport bool) {
	t.Helper()
	if !strings.HasPrefix(got, "[FIRST RUN]") {
		t.Fatalf("%s: delivered turn does not start with the onboarding:\n%s", id, got)
	}
	// The main-branch onboarding text leads the turn unchanged; the import,
	// when given, follows it directly.
	if !strings.HasPrefix(got, prompts.FirstRun()+"\n\n") {
		t.Errorf("%s: the delivered turn does not start with the full onboarding text", id)
	}
	if wantImport && !strings.HasPrefix(got, prompts.FirstRun()+"\n\n"+importMarker) {
		t.Errorf("%s: the import does not directly follow the onboarding", id)
	}
	if has := strings.Contains(got, importMarker); has != wantImport {
		t.Errorf("%s: import delivered = %v, want %v\nturn:\n%s", id, has, wantImport, got)
	}
}

// A fresh install with one agent: its onboarding carries the import.
func TestFirstRunImport_FreshInstallOneAgent(t *testing.T) {
	in := newImportInstall(t)
	a := in.agent("alpha")
	in.boot(a)
	got := a.turn(t, "hello")
	checkOnboarding(t, "alpha", got, true)
	// An API agent is told to use "ask".
	if !strings.Contains(got, "ONE `ask` call") {
		t.Errorf("API agent's import does not name the ask tool as `ask`:\n%s", got)
	}
}

// Two fresh agents at one boot, first turns at the same time: exactly one
// gets the import, both get the full onboarding. Repeated on a fresh install
// each time, so a lost race shows up (run it under GOFLAGS=-race too).
func TestFirstRunImport_TwoFreshAgentsAtOneBoot(t *testing.T) {
	for run := 0; run < 20; run++ {
		in := newImportInstall(t)
		a, b := in.agent("alpha"), in.agent("beta")
		in.boot(a, b)

		start := make(chan struct{})
		var wg sync.WaitGroup
		got := make([]string, 2)
		for i, ag := range []*importAgent{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got[i] = ag.turn(t, "hello")
			}()
		}
		close(start)
		wg.Wait()

		imports := 0
		for i, ag := range []*importAgent{a, b} {
			if !strings.HasPrefix(got[i], "[FIRST RUN]") {
				t.Fatalf("run %d: %s: no onboarding delivered:\n%s", run, ag.inst.id, got[i])
			}
			if strings.Contains(got[i], importMarker) {
				imports++
			}
		}
		if imports != 1 {
			t.Fatalf("run %d: import delivered to %d agents, want exactly 1", run, imports)
		}
	}
}

// A second agent hot-added after the first one onboarded: no import.
func TestFirstRunImport_HotAddedSecondAgent(t *testing.T) {
	in := newImportInstall(t)
	a := in.agent("alpha")
	in.boot(a)
	checkOnboarding(t, "alpha", a.turn(t, "hello"), true)

	b := in.agent("beta")
	armFirstRun(b.inst, in.idx) // as liveAgentStarter's build does
	checkOnboarding(t, "beta", b.turn(t, "hello"), false)
}

// An existing install (an agent already onboarded, no install marker) gets
// no import, not even for an agent created after the upgrade.
func TestFirstRunImport_ExistingInstallNewAgent(t *testing.T) {
	in := newImportInstall(t)
	if err := in.idx.SetAgentMetadata("old", "first_run_completed", "true"); err != nil {
		t.Fatal(err)
	}
	old, fresh := in.agent("old"), in.agent("fresh")
	in.boot(old, fresh)
	checkOnboarding(t, "fresh", fresh.turn(t, "hello"), false)

	// A later hot-added agent on the same install: still none.
	late := in.agent("late")
	armFirstRun(late.inst, in.idx)
	checkOnboarding(t, "late", late.turn(t, "hello"), false)

	// And after a restart.
	in.restart()
	later := in.agent("later")
	in.boot(later)
	checkOnboarding(t, "later", later.turn(t, "hello"), false)
}

// A restart after arming but before delivery: the import is still delivered,
// once, after the restart.
func TestFirstRunImport_RestartBeforeDelivery(t *testing.T) {
	in := newImportInstall(t)
	in.boot(in.agent("alpha")) // armed, never delivered

	in.restart()
	a := in.agent("alpha")
	in.boot(a)
	checkOnboarding(t, "alpha", a.turn(t, "hello"), true)
}

// A restart after delivery: no agent gets the import again.
func TestFirstRunImport_RestartAfterDelivery(t *testing.T) {
	in := newImportInstall(t)
	a := in.agent("alpha")
	in.boot(a)
	checkOnboarding(t, "alpha", a.turn(t, "hello"), true)

	in.restart()
	a2, b := in.agent("alpha"), in.agent("beta")
	in.boot(a2, b)
	if got := a2.turn(t, "again"); strings.Contains(got, "[FIRST RUN]") || strings.Contains(got, importMarker) {
		t.Errorf("alpha got onboarding or import again after a restart:\n%s", got)
	}
	checkOnboarding(t, "beta", b.turn(t, "hello"), false)
}

// A delegated agent is told to use foci_ask.
func TestFirstRunImport_DelegatedAskToolName(t *testing.T) {
	in := newImportInstall(t)
	add := firstRunImportAddendum(in.idx, "alpha", func() bool { return true })
	initFirstRunImportMarker(in.idx)
	got := add()
	if !strings.Contains(got, "ONE `foci_ask` call") || strings.Contains(got, "{{ask_tool}}") {
		t.Fatalf("delegated import does not name foci_ask:\n%s", got)
	}
}

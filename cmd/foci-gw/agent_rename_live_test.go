package main

// #2787: /agents rename renames an agent inside the running gateway. These
// tests drive liveAgentStarter.Rename with the real registry and readers, a
// mock-backed build, and fakes for the teardown and the disk rename (the
// engine has its own tests in internal/agentrename), so they check the
// orchestration: what stops, what starts, where, and what a failure leaves.

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"foci/internal/agentrename"
	"foci/internal/config"
)

// renameFixture is a gateway with three running agents — testAgentID, bob,
// carol, in that order — and a fake foci.toml the fake disk rename edits.
type renameFixture struct {
	*hotAddFixture
	mu        sync.Mutex
	cfgIDs    []string // the agent ids foci.toml names, in order
	stopped   []string // teardown calls
	platRen   [][2]string
	diskRuns  int
	planFail  error // the plan run fails with this
	runFail   error // the real run fails with this...
	failAfter bool  // ...after rewriting foci.toml (else before)
}

func newRenameFixture(t *testing.T) *renameFixture {
	t.Helper()
	f := &renameFixture{cfgIDs: []string{testAgentID, "bob", "carol"}}
	f.hotAddFixture = newHotAddFixture(t, nil, nil)
	s := f.starter
	s.load = func() (*config.Config, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		cfg := &config.Config{}
		for _, id := range f.cfgIDs {
			cfg.Agents = append(cfg.Agents, config.AgentConfig{ID: id})
		}
		return cfg, nil
	}
	s.teardown = func(inst *agentInstance) {
		f.mu.Lock()
		f.stopped = append(f.stopped, inst.id)
		f.mu.Unlock()
	}
	s.renamePlatforms = func(old, new string) {
		f.mu.Lock()
		f.platRen = append(f.platRen, [2]string{old, new})
		f.mu.Unlock()
	}
	s.renameDisk = func(_ context.Context, o agentrename.Options) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if o.PlanOnly {
			_, _ = io.WriteString(o.Out, "Plan:\n  1. back up\n")
			return f.planFail
		}
		f.diskRuns++
		if f.runFail != nil && !f.failAfter {
			return f.runFail
		}
		if i := slices.Index(f.cfgIDs, o.Old); i >= 0 {
			f.cfgIDs[i] = o.New
		}
		if f.runFail != nil {
			return f.runFail
		}
		_, _ = io.WriteString(o.Out, "Checklist:\n  - Backup: /x\n")
		return nil
	}
	for _, id := range []string{"bob", "carol"} {
		if _, err := s.Start(id); err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
	}
	return f
}

func (f *renameFixture) order() []string { return f.d.agents.Order() }

func TestLiveRename_StopsOnlyOldAndStartsNewInItsPlace(t *testing.T) {
	f := newRenameFixture(t)
	first, carol := f.d.agents.at(testAgentID), f.d.agents.at("carol")

	if _, err := f.starter.Rename(context.Background(), renameRequest{Old: "bob", New: "rob"}, io.Discard); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if !slices.Equal(f.stopped, []string{"bob"}) {
		t.Errorf("stopped %v, want only bob", f.stopped)
	}
	if got := f.order(); !slices.Equal(got, []string{testAgentID, "rob", "carol"}) {
		t.Errorf("agent order = %v, want rob in bob's place", got)
	}
	if f.d.agents.at(testAgentID) != first || f.d.agents.at("carol") != carol {
		t.Error("an agent other than bob was replaced")
	}
	if inst := newAgentResolver(f.d.agents)("rob"); inst == nil || inst.id != "rob" {
		t.Errorf("resolver(rob) = %v", inst)
	}
	if inst := newAgentResolver(f.d.agents)("bob"); inst != nil {
		t.Errorf("resolver(bob) still finds %q", inst.id)
	}
	if !slices.Equal(f.platRen, [][2]string{{"bob", "rob"}}) {
		t.Errorf("platform rename calls = %v", f.platRen)
	}
	if got := gwAgentRenames.resolve("bob"); got != "rob" {
		t.Errorf("startup-config id bob resolves to %q, want rob", got)
	}
}

// The first agent is the default target (HTTP, askgw, the shared facet
// pool): renaming it keeps the new id first.
func TestLiveRename_DefaultAgentStaysDefault(t *testing.T) {
	f := newRenameFixture(t)
	if _, err := f.starter.Rename(context.Background(), renameRequest{Old: testAgentID, New: "prime"}, io.Discard); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if d, ok := f.d.agents.Default(); !ok || d.id != "prime" {
		t.Errorf("default agent = %v, want prime", d)
	}
}

// A refused plan (bad id, new id in use, a foci.toml that would not load)
// stops nothing and runs no step.
func TestLiveRename_RefusedPlanStopsNothing(t *testing.T) {
	f := newRenameFixture(t)
	f.planFail = errors.New("the new id \"rob\" is already used")
	_, err := f.starter.Rename(context.Background(), renameRequest{Old: "bob", New: "rob"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("err = %v", err)
	}
	if len(f.stopped) != 0 || f.diskRuns != 0 {
		t.Errorf("a refused plan stopped %v and ran the disk rename %d time(s)", f.stopped, f.diskRuns)
	}
	if _, ok := f.d.agents.Get("bob"); !ok {
		t.Error("bob is not running after a refused plan")
	}
}

// A failed step before foci.toml is rewritten: bob is started again, in its
// place, and the error says so and how to resume.
func TestLiveRename_FailureBeforeConfigRestartsOld(t *testing.T) {
	f := newRenameFixture(t)
	f.runFail = errors.New("database state.db: disk I/O error")
	_, err := f.starter.Rename(context.Background(), renameRequest{Old: "bob", New: "rob"}, io.Discard)
	if err == nil {
		t.Fatal("a failed disk rename reported success")
	}
	for _, want := range []string{"disk I/O error", `"bob" runs again`, "/agents rename bob rob"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if got := f.order(); !slices.Equal(got, []string{testAgentID, "bob", "carol"}) {
		t.Errorf("agent order = %v, want bob back in its place", got)
	}
	if _, ok := f.d.agents.Get("rob"); ok {
		t.Error("rob was started although the rename failed")
	}
}

// A failed step after foci.toml names the new id: neither id can be started
// safely, so nothing is; the error says to finish with the same command,
// which then resumes and starts the new id.
func TestLiveRename_FailureAfterConfigResumes(t *testing.T) {
	f := newRenameFixture(t)
	f.runFail, f.failAfter = errors.New("crontab: install failed"), true
	_, err := f.starter.Rename(context.Background(), renameRequest{Old: "bob", New: "rob"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "neither agent runs") || !strings.Contains(err.Error(), "/agents rename bob rob") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := f.d.agents.Get("bob"); ok {
		t.Error("bob was restarted although foci.toml no longer names it")
	}
	f.runFail = nil
	if _, err := f.starter.Rename(context.Background(), renameRequest{Old: "bob", New: "rob"}, io.Discard); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, ok := f.d.agents.Get("rob"); !ok {
		t.Error("the resumed rename did not start rob")
	}
	if !slices.Equal(f.stopped, []string{"bob"}) {
		t.Errorf("stopped %v, want bob once", f.stopped)
	}
}

func TestLiveRename_RunningNewIDIsRefused(t *testing.T) {
	f := newRenameFixture(t)
	_, err := f.starter.Rename(context.Background(), renameRequest{Old: "bob", New: "carol"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v", err)
	}
	if len(f.stopped) != 0 {
		t.Errorf("stopped %v", f.stopped)
	}
}

func TestLiveRename_RefusedAfterClose(t *testing.T) {
	f := newRenameFixture(t)
	f.starter.close()
	if _, err := f.starter.Rename(context.Background(), renameRequest{Old: "bob", New: "rob"}, io.Discard); !errors.Is(err, errShuttingDown) {
		t.Fatalf("err = %v, want errShuttingDown", err)
	}
}

func TestAgentRenames_ResolveFollowsChainsAndReversal(t *testing.T) {
	var r agentRenames
	r.add("a", "b")
	r.add("b", "c")
	if got := r.resolve("a"); got != "c" {
		t.Errorf("a → %q, want c", got)
	}
	r.add("c", "a") // renamed back
	if got := r.resolve("a"); got != "a" {
		t.Errorf("after the rename back, a → %q, want a", got)
	}
	if got := r.resolve("zed"); got != "zed" {
		t.Errorf("unrelated id → %q", got)
	}
}

// A new agent reusing a renamed agent's old id is that agent: askgw (and
// every other gwAgentRenames reader) must resolve the id to it, not to the
// agent it was renamed to.
func TestLiveRename_ReclaimedOldIDResolvesToTheNewAgent(t *testing.T) {
	f := newRenameFixture(t)
	if _, err := f.starter.Rename(context.Background(), renameRequest{Old: "bob", New: "rob"}, io.Discard); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got := gwAgentRenames.resolve("bob"); got != "rob" {
		t.Fatalf("after the rename bob resolves to %q, want rob", got)
	}
	f.mu.Lock()
	f.cfgIDs = append(f.cfgIDs, "bob") // /agents new bob
	f.mu.Unlock()
	if _, err := f.starter.Start("bob"); err != nil {
		t.Fatalf("start the new bob: %v", err)
	}
	if got := gwAgentRenames.resolve("bob"); got != "bob" {
		t.Errorf("a new agent bob resolves to %q, want bob", got)
	}
	if got := gwAgentRenames.resolve("rob"); got != "rob" {
		t.Errorf("rob resolves to %q", got)
	}
}

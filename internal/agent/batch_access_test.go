package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/tools"
)

// bridgeRecordingBackend records, at each Start, the foci shell functions its
// exec bridge exports (the BASH_ENV file the bridge writes), so a test can see
// which tools a batch session could reach.
type bridgeRecordingBackend struct {
	batchTurnBackend
	funcs []string // one entry per Start: the BASH_ENV file's text, "" = no bridge
}

func (b *bridgeRecordingBackend) Start(ctx context.Context, opts delegator.StartOptions) error {
	text := ""
	if path := opts.Env["BASH_ENV"]; path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text = string(data)
	}
	b.optsMu.Lock()
	b.funcs = append(b.funcs, text)
	b.optsMu.Unlock()
	return b.batchTurnBackend.Start(ctx, opts)
}

// accessTestRegistry holds one tool from each class the access levels treat
// differently: an explore-allowlisted tool, a messaging tool, close_session
// and the spawn family.
func accessTestRegistry() *tools.Registry {
	reg := tools.NewRegistry()
	for _, name := range []string{"memory_search", "send_to_chat", "close_session", "spawn", "spawn_status"} {
		reg.Register(&tools.Tool{Name: name, ExecExport: true, Description: name + ".", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)})
	}
	return reg
}

// newAccessTestAgent is newBatchTestAgent with a Spec that declares the given
// batch capabilities and an exec registry wired for bridges.
func newAccessTestAgent(t *testing.T, be delegator.Delegator, caps map[delegator.Capability]delegator.Support) *Agent {
	t.Helper()
	a := newBatchTestAgent(t, be)
	a.DelegatedManager.Spec = delegator.Spec{LedgerKey: accounting.BackendCCStream, DisplayName: "Test", Caps: caps}
	a.DelegatedManager.StartOpts.ExecRegistry = accessTestRegistry()
	return a
}

var bothBatchCaps = map[delegator.Capability]delegator.Support{
	delegator.CapIsolatedBatch: delegator.Yes(),
	delegator.CapReadOnlyBatch: delegator.Yes(),
}

// TestRunBatch_RejectsUnsupportedRestrictedAccess proves a restricted batch on
// a backend that does not declare the matching capability is refused before
// launch, rather than run with the agent's full tools: an explore spawn on
// such a backend would otherwise write and execute freely.
func TestRunBatch_RejectsUnsupportedRestrictedAccess(t *testing.T) {
	t.Parallel()
	for _, access := range []delegator.BatchAccess{delegator.BatchAccessIsolated, delegator.BatchAccessReadOnly} {
		t.Run(access.String(), func(t *testing.T) {
			be := &batchTurnBackend{}
			be.completeTurns()
			a := newAccessTestAgent(t, be, map[delegator.Capability]delegator.Support{
				delegator.CapIsolatedBatch: delegator.No("test"),
				delegator.CapReadOnlyBatch: delegator.No("test"),
			})
			_, err := a.DelegatedManager.RunBatch(context.Background(), delegator.BatchRequest{
				Prompt: "p", Access: access, Purpose: delegator.BatchPurposeSpawnExplore,
			})
			if err == nil {
				t.Fatalf("RunBatch ran a %s batch on a backend that cannot restrict it", access)
			}
			if n := len(be.startedOpts()); n != 0 {
				t.Fatalf("backend started %d times for a refused batch", n)
			}
		})
	}
}

// TestRunBatch_RejectsAgentPromptWithSystemPrompt proves the two ways of
// choosing a batch's system prompt cannot both be set: one would silently
// win.
func TestRunBatch_RejectsAgentPromptWithSystemPrompt(t *testing.T) {
	t.Parallel()
	be := &batchTurnBackend{}
	be.completeTurns()
	a := newBatchTestAgent(t, be)
	if _, err := a.DelegatedManager.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt: "p", SystemPrompt: "X", AgentPrompt: true, Access: delegator.BatchAccessParent, Purpose: delegator.BatchPurposeSpawnCharacter,
	}); err == nil {
		t.Fatal("RunBatch accepted both AgentPrompt and SystemPrompt")
	}
}

// TestRunBatch_AccessAndPromptReachLaunch proves every batch launches
// unattended (SkipPermissions, so its asks are decided by the rules, never
// prompted) with its BatchAccess unchanged, and that AgentPrompt launches with
// the agent's own composed prompt while a plain batch replaces it with the
// caller's (empty = the backend's default).
func TestRunBatch_AccessAndPromptReachLaunch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		access      delegator.BatchAccess
		agentPrompt bool
		system      string
		wantPrompt  string
	}{
		{delegator.BatchAccessDefault, false, "CALLER", "CALLER"},
		{delegator.BatchAccessParent, true, "", "AGENT PROMPT"},
		{delegator.BatchAccessIsolated, false, "", ""},
		{delegator.BatchAccessReadOnly, false, "EXPLORE", "EXPLORE"},
	}
	for _, c := range cases {
		t.Run(c.access.String(), func(t *testing.T) {
			be := &batchTurnBackend{}
			be.completeTurns()
			a := newAccessTestAgent(t, be, bothBatchCaps)
			a.DelegatedManager.StartOpts.SystemPromptFunc = func(string) string { return "AGENT PROMPT" }
			if _, err := a.DelegatedManager.RunBatch(context.Background(), delegator.BatchRequest{
				Prompt: "p", SystemPrompt: c.system, AgentPrompt: c.agentPrompt, Access: c.access,
				OwnerSessionKey: "helen/c1", Purpose: delegator.BatchPurposeSpawnCharacter,
			}); err != nil {
				t.Fatalf("RunBatch: %v", err)
			}
			opts := be.startedOpts()
			if len(opts) != 1 {
				t.Fatalf("started %d times, want 1", len(opts))
			}
			if !opts[0].SkipPermissions || opts[0].BatchAccess != c.access {
				t.Errorf("launched with skip=%v access %s, want skip=true access %s", opts[0].SkipPermissions, opts[0].BatchAccess, c.access)
			}
			if opts[0].SystemPrompt != c.wantPrompt {
				t.Errorf("launched with system prompt %q, want %q", opts[0].SystemPrompt, c.wantPrompt)
			}
		})
	}
}

// TestRunBatch_BridgeToolsFollowAccess proves a batch session's exec bridge
// exports only what its access allows (tools.RegistryForAccess): the whole
// registry for foci's own batches; all but close_session and the spawn family for a
// parent-access batch; the explore allowlist read-only; and no bridge at all
// for an isolated batch.
func TestRunBatch_BridgeToolsFollowAccess(t *testing.T) {
	t.Parallel()
	all := []string{"foci_memory_search", "foci_send_to_chat", "foci_close_session", "foci_spawn", "foci_spawn_status"}
	cases := []struct {
		access delegator.BatchAccess
		want   []string // exported functions; nil = no bridge
	}{
		{delegator.BatchAccessDefault, all},
		{delegator.BatchAccessParent, []string{"foci_memory_search", "foci_send_to_chat"}},
		{delegator.BatchAccessReadOnly, []string{"foci_memory_search"}},
		{delegator.BatchAccessIsolated, nil},
	}
	for _, c := range cases {
		t.Run(c.access.String(), func(t *testing.T) {
			be := &bridgeRecordingBackend{}
			be.completeTurns()
			a := newAccessTestAgent(t, be, bothBatchCaps)
			if _, err := a.DelegatedManager.RunBatch(context.Background(), delegator.BatchRequest{
				Prompt: "p", Access: c.access, OwnerSessionKey: "helen/c1", Purpose: delegator.BatchPurposeSpawnExplore,
			}); err != nil {
				t.Fatalf("RunBatch: %v", err)
			}
			if len(be.funcs) != 1 {
				t.Fatalf("started %d times, want 1", len(be.funcs))
			}
			text := be.funcs[0]
			if c.want == nil {
				if text != "" {
					t.Fatalf("isolated batch got an exec bridge:\n%s", text)
				}
				return
			}
			for _, fn := range all {
				has := strings.Contains(text, fn+"()")
				want := false
				for _, w := range c.want {
					want = want || w == fn
				}
				if has != want {
					t.Errorf("bridge exports %s = %v, want %v", fn, has, want)
				}
			}
		})
	}
}

// TestCharacterSpawnBranch_LaunchesAsCharacter proves a background character
// spawn's branch (#2643) launches as a direct character spawn does — the
// agent's own composed prompt, parent access, unattended, on the requested
// model — and counts as a batch session for the unattended-turn check, so
// codex (which reads no SkipPermissions) keeps its asks out of a chat too.
func TestCharacterSpawnBranch_LaunchesAsCharacter(t *testing.T) {
	t.Parallel()
	be := &batchTurnBackend{}
	be.completeTurns()
	a := newAccessTestAgent(t, be, bothBatchCaps)
	a.DelegatedManager.StartOpts.SystemPromptFunc = func(string) string { return "AGENT PROMPT" }
	const bk = "helen/c1/b1700000000"
	if a.DelegatedManager.isBatchSession(bk) {
		t.Fatal("an unprepared branch counts as a batch session")
	}
	a.PrepareCharacterSpawnBranch(bk, "zai/glm-4.6")
	if !a.DelegatedManager.isBatchSession(bk) {
		t.Fatal("a character spawn branch does not count as a batch session (its turns would prompt)")
	}
	if _, err := a.DelegatedManager.Get(context.Background(), bk); err != nil {
		t.Fatalf("Get: %v", err)
	}
	opts := be.startedOpts()
	if len(opts) != 1 {
		t.Fatalf("started %d times, want 1", len(opts))
	}
	o := opts[0]
	if !o.SkipPermissions || o.BatchAccess != delegator.BatchAccessParent || o.SystemPrompt != "AGENT PROMPT" || o.Model != "zai/glm-4.6" {
		t.Errorf("launched with skip=%v access=%s prompt=%q model=%q; want skip, parent, the agent's prompt, zai/glm-4.6",
			o.SkipPermissions, o.BatchAccess, o.SystemPrompt, o.Model)
	}
	var nilMgr *DelegatedManager
	if nilMgr.isBatchSession(bk) {
		t.Error("a nil manager (API agent) reports a batch session")
	}
}

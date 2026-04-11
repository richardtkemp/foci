package ccstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// ---------------------------------------------------------------------------
// Settings file install / uninstall
// ---------------------------------------------------------------------------

// writeSettings is a test helper that serialises a settings.local.json
// fixture to a temp path.
func writeSettings(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readTop parses a settings.local.json file back into the top-level raw-
// message map so tests can inspect individual keys.
func readTop(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return top
}

// readHooks decodes the "hooks" sub-tree of a settings.local.json file.
func readHooks(t *testing.T, path string) hooksConfig {
	t.Helper()
	top := readTop(t, path)
	raw, ok := top["hooks"]
	if !ok {
		return hooksConfig{}
	}
	var h hooksConfig
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("parse hooks: %v", err)
	}
	return h
}

// TestEnsureFociEntry_AddsWhenAbsent proves appendFociEntry creates a new
// matcher entry for foci's command when nothing matching is present.
func TestEnsureFociEntry_AddsWhenAbsent(t *testing.T) {
	spec := fociHookSpec("/usr/local/bin/foci-cc-hook")
	matchers := appendFociEntry(nil, spec)

	if len(matchers) != 1 {
		t.Fatalf("matchers = %d, want 1", len(matchers))
	}
	if matchers[0].Matcher != "*" {
		t.Errorf("matcher = %q, want *", matchers[0].Matcher)
	}
	if len(matchers[0].Hooks) != 1 || matchers[0].Hooks[0].Command != "/usr/local/bin/foci-cc-hook" {
		t.Errorf("hooks = %+v", matchers[0].Hooks)
	}
}

// TestEnsureFociEntry_Idempotent proves running install twice leaves exactly
// one foci entry — important for crash-recovery scenarios where a dead
// foci's entries linger and a new foci starts up in the same workdir.
func TestEnsureFociEntry_Idempotent(t *testing.T) {
	spec := fociHookSpec("/bin/foci-cc-hook")
	once := appendFociEntry(nil, spec)
	twice := appendFociEntry(once, spec)

	if len(twice) != 1 {
		t.Errorf("after double install matchers = %d, want 1", len(twice))
	}
}

// TestEnsureFociEntry_PreservesUserHooks proves existing non-foci hooks are
// left untouched when foci adds its own entry — the user's hooks and foci's
// hook both fire.
func TestEnsureFociEntry_PreservesUserHooks(t *testing.T) {
	userMatcher := hookMatcher{
		Matcher: "Write|Edit",
		Hooks: []hookSpec{
			{Type: "command", Command: "/home/user/bin/format.sh"},
		},
	}
	merged := appendFociEntry([]hookMatcher{userMatcher}, fociHookSpec("/bin/foci-cc-hook"))

	if len(merged) != 2 {
		t.Fatalf("matchers = %d, want 2 (user + foci)", len(merged))
	}
	if merged[0].Hooks[0].Command != "/home/user/bin/format.sh" {
		t.Errorf("user hook displaced: %+v", merged[0])
	}
	if merged[1].Hooks[0].Command != "/bin/foci-cc-hook" {
		t.Errorf("foci hook missing: %+v", merged[1])
	}
}

// TestRemoveFociEntries_KeepsUserHooks proves uninstall drops only foci's
// command entries; user hooks under the same or adjacent matchers stay.
func TestRemoveFociEntries_KeepsUserHooks(t *testing.T) {
	input := []hookMatcher{
		{
			Matcher: "*",
			Hooks: []hookSpec{
				{Type: "command", Command: "/bin/foci-cc-hook"},
				{Type: "command", Command: "/home/user/other-hook.sh"},
			},
		},
	}
	out := removeFociEntries(input, "/bin/foci-cc-hook")

	if len(out) != 1 {
		t.Fatalf("matchers = %d, want 1", len(out))
	}
	if len(out[0].Hooks) != 1 || out[0].Hooks[0].Command != "/home/user/other-hook.sh" {
		t.Errorf("remaining hooks = %+v", out[0].Hooks)
	}
}

// TestRemoveFociEntries_PrunesEmptyMatcher proves matchers that have no
// remaining hooks after uninstall are dropped so the config doesn't
// accumulate empty shells.
func TestRemoveFociEntries_PrunesEmptyMatcher(t *testing.T) {
	input := []hookMatcher{
		{
			Matcher: "*",
			Hooks: []hookSpec{
				{Type: "command", Command: "/bin/foci-cc-hook"},
			},
		},
	}
	out := removeFociEntries(input, "/bin/foci-cc-hook")

	if len(out) != 0 {
		t.Errorf("matchers = %d, want 0 (empty matcher should be pruned)", len(out))
	}
}

// TestInstallHooks_CreatesSettingsFile proves installing into an empty
// workdir writes a minimal settings.local.json with foci's two entries
// and nothing else.
func TestInstallHooks_CreatesSettingsFile(t *testing.T) {
	workDir := t.TempDir()
	settingsPath := filepath.Join(workDir, ".claude", "settings.local.json")

	// Directly exercise the merge helpers (resolveHookBinary requires an
	// actual binary sibling, which unit tests don't have).
	top, err := loadSettings(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	hooks := extractHooks(top)
	spec := fociHookSpec("/bin/foci-cc-hook")
	hooks[eventPostToolUse] = appendFociEntry(hooks[eventPostToolUse], spec)
	hooks[eventPostToolUseFailure] = appendFociEntry(hooks[eventPostToolUseFailure], spec)
	if err := writeHooks(settingsPath, top, hooks); err != nil {
		t.Fatal(err)
	}

	got := readHooks(t, settingsPath)
	if _, ok := got[eventPostToolUse]; !ok {
		t.Error("PostToolUse entry not written")
	}
	if _, ok := got[eventPostToolUseFailure]; !ok {
		t.Error("PostToolUseFailure entry not written")
	}
}

// TestInstallHooks_MergesWithExisting proves that a pre-existing
// settings.local.json with user hooks retains them after foci installs its
// own entries, and that top-level keys unrelated to hooks are preserved.
func TestInstallHooks_MergesWithExisting(t *testing.T) {
	workDir := t.TempDir()
	settingsPath := filepath.Join(workDir, ".claude", "settings.local.json")

	writeSettings(t, settingsPath, `{
	  "hooks": {
	    "PostToolUse": [
	      { "matcher": "Write", "hooks": [{ "type": "command", "command": "/home/user/format.sh" }] }
	    ]
	  },
	  "permissionMode": "default"
	}`)

	top, err := loadSettings(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	hooks := extractHooks(top)
	spec := fociHookSpec("/bin/foci-cc-hook")
	hooks[eventPostToolUse] = appendFociEntry(hooks[eventPostToolUse], spec)
	hooks[eventPostToolUseFailure] = appendFociEntry(hooks[eventPostToolUseFailure], spec)
	if err := writeHooks(settingsPath, top, hooks); err != nil {
		t.Fatal(err)
	}

	// User's PostToolUse hook should still be there.
	gotHooks := readHooks(t, settingsPath)
	pte := gotHooks[eventPostToolUse]
	if len(pte) != 2 {
		t.Fatalf("PostToolUse entries = %d, want 2 (user + foci)", len(pte))
	}
	foundUser := false
	foundFoci := false
	for _, m := range pte {
		for _, h := range m.Hooks {
			if h.Command == "/home/user/format.sh" {
				foundUser = true
			}
			if h.Command == "/bin/foci-cc-hook" {
				foundFoci = true
			}
		}
	}
	if !foundUser || !foundFoci {
		t.Errorf("user=%v foci=%v", foundUser, foundFoci)
	}

	// Top-level permissionMode must survive the round-trip.
	gotTop := readTop(t, settingsPath)
	if _, ok := gotTop["permissionMode"]; !ok {
		t.Error("permissionMode not preserved across merge")
	}
}

// TestUninstallHooks_RemovesEntries proves install-then-uninstall restores
// the settings file to its pre-install state when the user had no hooks.
func TestUninstallHooks_RemovesEntries(t *testing.T) {
	workDir := t.TempDir()
	settingsPath := filepath.Join(workDir, ".claude", "settings.local.json")

	// Install.
	top, _ := loadSettings(settingsPath)
	hooks := extractHooks(top)
	spec := fociHookSpec("/bin/foci-cc-hook")
	hooks[eventPostToolUse] = appendFociEntry(hooks[eventPostToolUse], spec)
	hooks[eventPostToolUseFailure] = appendFociEntry(hooks[eventPostToolUseFailure], spec)
	if err := writeHooks(settingsPath, top, hooks); err != nil {
		t.Fatal(err)
	}

	// Uninstall.
	top, _ = loadSettings(settingsPath)
	hooks = extractHooks(top)
	hooks[eventPostToolUse] = removeFociEntries(hooks[eventPostToolUse], "/bin/foci-cc-hook")
	hooks[eventPostToolUseFailure] = removeFociEntries(hooks[eventPostToolUseFailure], "/bin/foci-cc-hook")
	if len(hooks[eventPostToolUse]) == 0 {
		delete(hooks, eventPostToolUse)
	}
	if len(hooks[eventPostToolUseFailure]) == 0 {
		delete(hooks, eventPostToolUseFailure)
	}
	if err := writeHooks(settingsPath, top, hooks); err != nil {
		t.Fatal(err)
	}

	// File should be gone (empty top-level after pruning).
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Errorf("settings.local.json still exists after uninstall: %v", err)
	}
}

// TestUninstallHooks_PreservesUserHooks proves uninstalling leaves user
// hooks intact when they coexisted with foci's entries.
func TestUninstallHooks_PreservesUserHooks(t *testing.T) {
	workDir := t.TempDir()
	settingsPath := filepath.Join(workDir, ".claude", "settings.local.json")

	writeSettings(t, settingsPath, `{
	  "hooks": {
	    "PostToolUse": [
	      { "matcher": "Write", "hooks": [{ "type": "command", "command": "/home/user/format.sh" }] }
	    ]
	  }
	}`)

	// Install.
	top, _ := loadSettings(settingsPath)
	hooks := extractHooks(top)
	spec := fociHookSpec("/bin/foci-cc-hook")
	hooks[eventPostToolUse] = appendFociEntry(hooks[eventPostToolUse], spec)
	hooks[eventPostToolUseFailure] = appendFociEntry(hooks[eventPostToolUseFailure], spec)
	_ = writeHooks(settingsPath, top, hooks)

	// Uninstall.
	top, _ = loadSettings(settingsPath)
	hooks = extractHooks(top)
	hooks[eventPostToolUse] = removeFociEntries(hooks[eventPostToolUse], "/bin/foci-cc-hook")
	hooks[eventPostToolUseFailure] = removeFociEntries(hooks[eventPostToolUseFailure], "/bin/foci-cc-hook")
	if len(hooks[eventPostToolUse]) == 0 {
		delete(hooks, eventPostToolUse)
	}
	if len(hooks[eventPostToolUseFailure]) == 0 {
		delete(hooks, eventPostToolUseFailure)
	}
	_ = writeHooks(settingsPath, top, hooks)

	// User's entry should still be there.
	gotHooks := readHooks(t, settingsPath)
	pte := gotHooks[eventPostToolUse]
	if len(pte) != 1 || pte[0].Hooks[0].Command != "/home/user/format.sh" {
		t.Errorf("user hook not preserved: %+v", pte)
	}
}

// ---------------------------------------------------------------------------
// handleHookResponse dispatch
// ---------------------------------------------------------------------------

// TestHandleHookResponse_PostToolUse proves a well-formed hook_response
// envelope for a PostToolUse event dispatches to OnToolEnd with is_error=false.
// The backend's install ID matches the payload so the multi-backend filter
// lets the event through.
func TestHandleHookResponse_PostToolUse(t *testing.T) {
	b := &Backend{hookInstallID: "install-a"}

	type captured struct {
		id, name, output string
		isErr            bool
	}
	var got []captured
	handler := &delegator.EventHandler{
		OnToolEnd: func(id, name, output string, isError bool) {
			got = append(got, captured{id, name, output, isError})
		},
	}
	b.beginTurn(handler)

	stdout, _ := json.Marshal(hookScriptOutput{
		HookEvent:    "PostToolUse",
		InstallID:    "install-a",
		ToolUseID:    "toolu_1",
		ToolName:     "Read",
		ToolResponse: "file contents",
		IsError:      false,
	})
	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse",
		Stdout:    string(stdout),
		ExitCode:  0,
		Outcome:   "success",
	})
	b.handleHookResponse(env)

	if len(got) != 1 {
		t.Fatalf("OnToolEnd calls = %d, want 1", len(got))
	}
	c := got[0]
	if c.id != "toolu_1" || c.name != "Read" || c.output != "file contents" || c.isErr {
		t.Errorf("captured = %+v", c)
	}
}

// TestHandleHookResponse_PostToolUseFailure proves failure envelopes carry
// the error message (not tool_response) into OnToolEnd with is_error=true.
func TestHandleHookResponse_PostToolUseFailure(t *testing.T) {
	b := &Backend{hookInstallID: "install-b"}
	var captured struct {
		id, name, output string
		isErr            bool
	}
	handler := &delegator.EventHandler{
		OnToolEnd: func(id, name, output string, isError bool) {
			captured.id = id
			captured.name = name
			captured.output = output
			captured.isErr = isError
		},
	}
	b.beginTurn(handler)

	stdout, _ := json.Marshal(hookScriptOutput{
		HookEvent: "PostToolUseFailure",
		InstallID: "install-b",
		ToolUseID: "toolu_2",
		ToolName:  "Write",
		Error:     "Permission denied",
		IsError:   true,
	})
	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUseFailure",
		Stdout:    string(stdout),
	})
	b.handleHookResponse(env)

	if !captured.isErr {
		t.Error("isError = false, want true for PostToolUseFailure")
	}
	if captured.output != "Permission denied" {
		t.Errorf("output = %q, want Permission denied", captured.output)
	}
}

// TestHandleHookResponse_FiltersForeignInstallID proves the multi-backend
// filter drops events whose install_id doesn't match this backend's — each
// backend only acts on hook_response events from its own installed entry.
// This is what keeps two foci backends sharing a workdir from crossing
// wires.
func TestHandleHookResponse_FiltersForeignInstallID(t *testing.T) {
	b := &Backend{hookInstallID: "install-us"}
	fired := false
	handler := &delegator.EventHandler{
		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
	}
	b.beginTurn(handler)

	stdout, _ := json.Marshal(hookScriptOutput{
		HookEvent:    "PostToolUse",
		InstallID:    "install-someone-else",
		ToolUseID:    "toolu_x",
		ToolName:     "Read",
		ToolResponse: "not ours",
	})
	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse",
		Stdout:    string(stdout),
	})
	b.handleHookResponse(env)

	if fired {
		t.Error("OnToolEnd fired for foreign install_id")
	}
}

// TestInstallHooks_MultiBackend proves two backends installing into the
// same settings.local.json produce two independent entries (each with its
// own unique install ID in the command string), and that uninstalling
// one backend leaves the other's entry untouched.
func TestInstallHooks_MultiBackend(t *testing.T) {
	workDir := t.TempDir()
	settingsPath := filepath.Join(workDir, ".claude", "settings.local.json")

	hookPath := "/bin/foci-cc-hook"
	idA := "aaaaaaaa"
	idB := "bbbbbbbb"
	cmdA := buildHookCommand(hookPath, idA)
	cmdB := buildHookCommand(hookPath, idB)

	// Backend A installs.
	top, _ := loadSettings(settingsPath)
	hooks := extractHooks(top)
	hooks[eventPostToolUse] = appendFociEntry(hooks[eventPostToolUse], fociHookSpec(cmdA))
	hooks[eventPostToolUseFailure] = appendFociEntry(hooks[eventPostToolUseFailure], fociHookSpec(cmdA))
	if err := writeHooks(settingsPath, top, hooks); err != nil {
		t.Fatal(err)
	}

	// Backend B installs with a different install ID.
	top, _ = loadSettings(settingsPath)
	hooks = extractHooks(top)
	hooks[eventPostToolUse] = appendFociEntry(hooks[eventPostToolUse], fociHookSpec(cmdB))
	hooks[eventPostToolUseFailure] = appendFociEntry(hooks[eventPostToolUseFailure], fociHookSpec(cmdB))
	if err := writeHooks(settingsPath, top, hooks); err != nil {
		t.Fatal(err)
	}

	// Both entries should now be present.
	got := readHooks(t, settingsPath)
	if len(got[eventPostToolUse]) != 2 {
		t.Fatalf("PostToolUse entries = %d, want 2 (A + B)", len(got[eventPostToolUse]))
	}

	// Backend A uninstalls. B's entry must survive.
	top, _ = loadSettings(settingsPath)
	hooks = extractHooks(top)
	hooks[eventPostToolUse] = removeFociEntries(hooks[eventPostToolUse], cmdA)
	hooks[eventPostToolUseFailure] = removeFociEntries(hooks[eventPostToolUseFailure], cmdA)
	if err := writeHooks(settingsPath, top, hooks); err != nil {
		t.Fatal(err)
	}

	// B's entry still there.
	got = readHooks(t, settingsPath)
	if len(got[eventPostToolUse]) != 1 {
		t.Fatalf("PostToolUse entries after A uninstall = %d, want 1 (B)", len(got[eventPostToolUse]))
	}
	if got[eventPostToolUse][0].Hooks[0].Command != cmdB {
		t.Errorf("surviving entry = %q, want %q", got[eventPostToolUse][0].Hooks[0].Command, cmdB)
	}
}

// TestNewInstallID_Unique proves independent calls produce distinct IDs.
// A collision would mean two backends in the same workdir can't
// distinguish their own hook_response events from each other's.
func TestNewInstallID_Unique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newInstallID()
		if id == "" {
			t.Fatal("empty install ID")
		}
		if seen[id] {
			t.Fatalf("duplicate install ID after %d iterations: %s", i, id)
		}
		seen[id] = true
	}
}

// TestLockSettingsFile_SerializesConcurrent proves the package-level mutex
// keyed by absolute path prevents concurrent read-modify-write races on the
// same settings file. Two goroutines racing to add their entry should end
// up with both entries in the file rather than one clobbering the other.
func TestLockSettingsFile_SerializesConcurrent(t *testing.T) {
	workDir := t.TempDir()
	settingsPath := filepath.Join(workDir, ".claude", "settings.local.json")

	cmdA := buildHookCommand("/bin/foci-cc-hook", "a")
	cmdB := buildHookCommand("/bin/foci-cc-hook", "b")

	var wg sync.WaitGroup
	install := func(cmd string) {
		defer wg.Done()
		mu := lockSettingsFile(settingsPath)
		mu.Lock()
		defer mu.Unlock()
		top, _ := loadSettings(settingsPath)
		hooks := extractHooks(top)
		hooks[eventPostToolUse] = appendFociEntry(hooks[eventPostToolUse], fociHookSpec(cmd))
		_ = writeHooks(settingsPath, top, hooks)
	}

	wg.Add(2)
	go install(cmdA)
	go install(cmdB)
	wg.Wait()

	got := readHooks(t, settingsPath)
	if len(got[eventPostToolUse]) != 2 {
		t.Fatalf("entries = %d, want 2 (both install calls should have landed)", len(got[eventPostToolUse]))
	}
}

// TestHandleHookResponse_SkipsSubagent proves hook events with a non-empty
// agent_id are dropped before dispatch — they belong to the sub-agent's
// transcript, not the parent turn.
func TestHandleHookResponse_SkipsSubagent(t *testing.T) {
	b := &Backend{}
	fired := false
	handler := &delegator.EventHandler{
		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
	}
	b.beginTurn(handler)

	stdout, _ := json.Marshal(hookScriptOutput{
		HookEvent:    "PostToolUse",
		ToolUseID:    "toolu_sub",
		ToolName:     "Read",
		ToolResponse: "nested result",
		AgentID:      "agent-child-7",
	})
	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse",
		Stdout:    string(stdout),
	})
	b.handleHookResponse(env)

	if fired {
		t.Error("OnToolEnd fired for sub-agent hook event")
	}
}

// TestHandleHookResponse_SkipsUnknownHookEvent proves events that aren't
// PostToolUse or PostToolUseFailure are silently ignored — user-configured
// PreToolUse hooks or other lifecycle events shouldn't fire OnToolEnd.
func TestHandleHookResponse_SkipsUnknownHookEvent(t *testing.T) {
	b := &Backend{}
	fired := false
	handler := &delegator.EventHandler{
		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
	}
	b.beginTurn(handler)

	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PreToolUse",
		Stdout:    `{"tool_use_id":"x","tool_name":"y"}`,
	})
	b.handleHookResponse(env)

	if fired {
		t.Error("OnToolEnd fired for PreToolUse event")
	}
}

// TestHandleHookResponse_MalformedStdoutGracefulSkip proves malformed JSON
// in the hook script's stdout doesn't crash — we log at debug and drop the
// event, keeping the rest of the turn flowing.
func TestHandleHookResponse_MalformedStdoutGracefulSkip(t *testing.T) {
	b := &Backend{}
	fired := false
	handler := &delegator.EventHandler{
		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
	}
	b.beginTurn(handler)

	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse",
		Stdout:    "not valid json {{{",
	})
	b.handleHookResponse(env)

	if fired {
		t.Error("OnToolEnd fired despite malformed stdout")
	}
}

// TestHandleHookResponse_EmptyStdoutSilent proves hook_response messages
// with empty stdout (possible when the helper binary fails silently) are
// ignored rather than triggering a spurious OnToolEnd dispatch.
func TestHandleHookResponse_EmptyStdoutSilent(t *testing.T) {
	b := &Backend{}
	fired := false
	handler := &delegator.EventHandler{
		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
	}
	b.beginTurn(handler)

	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse",
		Stdout:    "",
	})
	b.handleHookResponse(env)

	if fired {
		t.Error("OnToolEnd fired for empty stdout")
	}
}

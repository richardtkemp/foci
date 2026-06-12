package cctmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// startRespond returns a fakeTmux respond function scripting the fresh-start
// sequence: no existing session, successful create, pane PID lookup.
func startRespond(panePID string) func(args []string, stdin string) (string, error) {
	return func(args []string, _ string) (string, error) {
		switch args[0] {
		case "has-session":
			return "", errors.New("no server running")
		case "list-panes":
			return panePID + "\n", nil
		}
		return "", nil
	}
}

// detachPane drops the Backend's pane reference under lock so Close skips the
// tmux teardown path (graceful /exit + kill) while still stopping the watcher.
func detachPane(t *testing.T, b *Backend) {
	t.Helper()
	b.mu.Lock()
	b.pane = nil
	b.mu.Unlock()
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestStart_BuildsClaudeCommand proves Start assembles the full claude
// invocation from StartOptions and backend config: system prompt written to a
// file and referenced via flag, model/resume flags, permission flags from
// config, env exports, window naming from the label, and pane geometry.
func TestStart_BuildsClaudeCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wd, "character"), 0o755); err != nil {
		t.Fatal(err)
	}

	f := &fakeTmux{respond: startRespond("4242")}
	b := &Backend{
		cfg:      map[string]any{"skip_permissions": true, "allowed_tools": "Bash,Read"},
		tmuxExec: f.exec,
	}

	opts := delegator.StartOptions{
		AgentID:         "main",
		Label:           "lbl",
		WorkDir:         wd,
		SystemPrompt:    "YOU ARE FOCI",
		Model:           "opus",
		ResumeSessionID: "uuid-1",
		TmuxCols:        200,
		TmuxRows:        50,
		Env:             map[string]string{"FOCI_SOCK": "/tmp/foci.sock"},
	}
	if err := b.Start(context.Background(), opts); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// System prompt must be persisted to the workspace file.
	promptFile := filepath.Join(wd, "character", ".full-prompt")
	data, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("prompt file not written: %v", err)
	}
	if string(data) != "YOU ARE FOCI" {
		t.Errorf("prompt file content = %q", data)
	}

	creates := f.callsFor("new-session")
	if len(creates) != 1 {
		t.Fatalf("got %d new-session calls, want 1", len(creates))
	}
	args := creates[0]
	for _, want := range [][]string{
		{"-d", "-s", "cc-lbl"},
		{"-x", "200", "-y", "50"},
		{"-c", wd},
	} {
		if !hasSubsequence(args, want) {
			t.Errorf("new-session args %v missing %v", args, want)
		}
	}

	// The command is shell-quoted for "sh -l -c", so quotes nest — assert
	// on the raw payloads rather than exact quoting.
	shellCmd := args[len(args)-1]
	for _, want := range []string{
		"--system-prompt-file", promptFile,
		"--model", "opus",
		"--resume", "uuid-1",
		"--dangerously-skip-permissions",
		"--allowedTools", "Bash,Read",
		"export ", "FOCI_SOCK=/tmp/foci.sock",
	} {
		if !strings.Contains(shellCmd, want) {
			t.Errorf("shell command missing %q\ncmd: %s", want, shellCmd)
		}
	}

	if b.pane.pid != 4242 {
		t.Errorf("pane pid = %d, want 4242", b.pane.pid)
	}
	if b.agentID != "main" || b.workDir != wd {
		t.Errorf("backend identity = (%q, %q), want (main, %s)", b.agentID, b.workDir, wd)
	}
}

// TestStart_LabelFallsBackToAgentID proves the tmux window is named after the
// agent ID when no label is supplied, and that optional flags are omitted
// when their options are empty.
func TestStart_LabelFallsBackToAgentID(t *testing.T) {
	f := &fakeTmux{respond: startRespond("7")}
	b := &Backend{cfg: map[string]any{}, tmuxExec: f.exec}

	opts := delegator.StartOptions{AgentID: "main", WorkDir: t.TempDir()}
	if err := b.Start(context.Background(), opts); err != nil {
		t.Fatalf("Start: %v", err)
	}

	args := f.callsFor("new-session")[0]
	if !hasSubsequence(args, []string{"-s", "cc-main"}) {
		t.Errorf("window should be cc-main, args = %v", args)
	}
	shellCmd := args[len(args)-1]
	for _, banned := range []string{"--system-prompt-file", "--model", "--resume", "--dangerously-skip-permissions", "--allowedTools"} {
		if strings.Contains(shellCmd, banned) {
			t.Errorf("shell command should not contain %q: %s", banned, shellCmd)
		}
	}
}

// TestStart_SystemPromptWriteError proves Start fails fast — before any tmux
// call — when the system prompt file cannot be written.
func TestStart_SystemPromptWriteError(t *testing.T) {
	f := &fakeTmux{}
	b := &Backend{cfg: map[string]any{}, tmuxExec: f.exec}

	// WorkDir has no character/ subdirectory, so the WriteFile fails.
	opts := delegator.StartOptions{AgentID: "a", WorkDir: t.TempDir(), SystemPrompt: "x"}
	err := b.Start(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "write system prompt file") {
		t.Fatalf("err = %v, want prompt-file write error", err)
	}
	if len(f.allCalls()) != 0 {
		t.Errorf("tmux should not be invoked, got %v", f.allCalls())
	}
}

// TestStart_CreateError proves a tmux new-session failure surfaces as a
// "create tmux pane" error.
func TestStart_CreateError(t *testing.T) {
	f := &fakeTmux{respond: func(args []string, _ string) (string, error) {
		return "", errors.New("tmux broken")
	}}
	b := &Backend{cfg: map[string]any{}, tmuxExec: f.exec}

	err := b.Start(context.Background(), delegator.StartOptions{AgentID: "a", WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "create tmux pane") {
		t.Fatalf("err = %v, want create error", err)
	}
}

// TestStart_ReadPanePIDError proves Start errors when the pane PID cannot be
// read after a successful create.
func TestStart_ReadPanePIDError(t *testing.T) {
	f := &fakeTmux{respond: func(args []string, _ string) (string, error) {
		switch args[0] {
		case "has-session", "list-panes":
			return "", errors.New("fail")
		}
		return "", nil
	}}
	b := &Backend{cfg: map[string]any{}, tmuxExec: f.exec}

	err := b.Start(context.Background(), delegator.StartOptions{AgentID: "a", WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "read pane PID") {
		t.Fatalf("err = %v, want read pane PID error", err)
	}
}

// TestStart_RecoversExistingSession proves crash recovery: when the tmux
// session already exists, Start adopts its pane PID, re-injects env vars via
// set-environment, and does NOT create a new session.
func TestStart_RecoversExistingSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // keep discoverSession away from the real home

	f := &fakeTmux{respond: func(args []string, _ string) (string, error) {
		if args[0] == "list-panes" {
			return "777\n", nil
		}
		return "", nil // has-session succeeds → session alive
	}}
	b := &Backend{cfg: map[string]any{}, tmuxExec: f.exec}

	opts := delegator.StartOptions{
		AgentID: "main",
		WorkDir: t.TempDir(),
		Env:     map[string]string{"BASH_ENV": "/tmp/bridge.sh"},
	}
	if err := b.Start(context.Background(), opts); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if len(f.callsFor("new-session")) != 0 {
		t.Error("recovery must not create a new session")
	}
	if b.pane.pid != 777 {
		t.Errorf("pane pid = %d, want 777 (adopted)", b.pane.pid)
	}
	envCalls := f.callsFor("set-environment")
	if len(envCalls) != 1 || !hasSubsequence(envCalls[0], []string{"BASH_ENV", "/tmp/bridge.sh"}) {
		t.Errorf("set-environment calls = %v", envCalls)
	}
}

// TestStart_DeadRecoveredPaneIsKilledAndRecreated proves that when the tmux
// session exists but its pane PID cannot be read (dead pane), Start kills the
// stale session and creates a fresh one.
func TestStart_DeadRecoveredPaneIsKilledAndRecreated(t *testing.T) {
	listPanesCalls := 0
	f := &fakeTmux{}
	f.respond = func(args []string, _ string) (string, error) {
		switch args[0] {
		case "has-session":
			return "", nil // stale session exists
		case "list-panes":
			listPanesCalls++
			if listPanesCalls == 1 {
				return "", errors.New("no panes") // dead pane on recovery probe
			}
			return "88\n", nil // healthy after recreate
		}
		return "", nil
	}
	b := &Backend{cfg: map[string]any{}, tmuxExec: f.exec}

	if err := b.Start(context.Background(), delegator.StartOptions{AgentID: "a", WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if len(f.callsFor("kill-session")) != 1 {
		t.Error("stale session should be killed")
	}
	if len(f.callsFor("new-session")) != 1 {
		t.Error("a fresh session should be created")
	}
	if b.pane.pid != 88 {
		t.Errorf("pane pid = %d, want 88", b.pane.pid)
	}
}

// ---------------------------------------------------------------------------
// startWatcher / discoverSession
// ---------------------------------------------------------------------------

// TestStartWatcher_WiresHandlerAndCatchesUp proves startWatcher consumes the
// recorded pre-send offset, installs the persistent handler (text delivery via
// replyFunc, typing restart, per-turn completion), performs the initial
// catch-up read for entries written before the watcher existed, and keeps
// delivering via the live watch loop afterwards.
func TestStartWatcher_WiresHandlerAndCatchesUp(t *testing.T) {
	// An entry already in the file before the watcher starts (the gap
	// between recordPreSendOffset and watcher creation).
	path := writeSessionFile(t, endTurnLine("early reply"))

	b := &Backend{preSendOffset: 0}

	replies := make(chan string, 4)
	var typing []bool
	turnDone := make(chan *delegator.TurnResult, 2)

	b.replyMu.Lock()
	b.replyFunc = func(text string) { replies <- text }
	b.typingFunc = func(v bool) { typing = append(typing, v) }
	b.replyMu.Unlock()
	b.turnCompleteMu.Lock()
	b.turnCompleteFn = func(r *delegator.TurnResult) { turnDone <- r }
	b.turnCompleteMu.Unlock()

	if err := b.startWatcher(path); err != nil {
		t.Fatalf("startWatcher: %v", err)
	}
	defer detachPane(t, b)

	// Catch-up read happens synchronously inside startWatcher.
	select {
	case got := <-replies:
		if got != "early reply" {
			t.Errorf("reply = %q, want %q", got, "early reply")
		}
	default:
		t.Fatal("catch-up read did not deliver the pre-existing entry")
	}
	select {
	case r := <-turnDone:
		if r.Text != "early reply" {
			t.Errorf("turn text = %q", r.Text)
		}
	default:
		t.Fatal("catch-up read did not fire turn completion")
	}
	if len(typing) == 0 || typing[0] != true {
		t.Errorf("typing calls = %v, want re-established (true) on text", typing)
	}

	if b.preSendOffset != -1 {
		t.Errorf("preSendOffset = %d, want -1 (consumed)", b.preSendOffset)
	}
	if got := b.SessionFilePath(); got != path {
		t.Errorf("SessionFilePath = %q, want %q", got, path)
	}

	// Live path: a new turn callback and a fresh appended entry flow
	// through the long-lived watch loop.
	b.turnCompleteMu.Lock()
	b.turnCompleteFn = func(r *delegator.TurnResult) { turnDone <- r }
	b.turnCompleteMu.Unlock()
	appendLine(t, path, endTurnLine("live reply"))

	select {
	case r := <-turnDone:
		if r.Text != "live reply" {
			t.Errorf("live turn text = %q", r.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch loop did not deliver the live entry")
	}
}

// TestStartWatcher_AgentStatusRoutesToReply proves the AgentTracker's status
// messages (sub-agent spawn notifications in the JSONL) are forwarded to the
// user through replyFunc.
func TestStartWatcher_AgentStatusRoutesToReply(t *testing.T) {
	agentSpawn := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"ag1","name":"Agent","input":{"description":"explore code"}}]}}` + "\n"
	path := writeSessionFile(t, agentSpawn)

	b := &Backend{preSendOffset: 0}
	replies := make(chan string, 4)
	b.replyMu.Lock()
	b.replyFunc = func(text string) { replies <- text }
	b.replyMu.Unlock()

	if err := b.startWatcher(path); err != nil {
		t.Fatalf("startWatcher: %v", err)
	}
	defer detachPane(t, b)

	select {
	case got := <-replies:
		if !strings.Contains(got, "explore code") {
			t.Errorf("status = %q, want agent description", got)
		}
	default:
		t.Fatal("agent spawn status was not forwarded to replyFunc")
	}
}

// TestStartWatcher_MissingFile proves startWatcher propagates watcher
// initialisation failure when the session file does not exist.
func TestStartWatcher_MissingFile(t *testing.T) {
	b := &Backend{preSendOffset: -1}
	err := b.startWatcher(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err == nil || !strings.Contains(err.Error(), "init watcher") {
		t.Fatalf("err = %v, want init watcher error", err)
	}
}

// TestDiscoverSession_Success proves the recovery-path discovery: a PID file
// under $HOME mapping the pane PID to a session, plus the session JSONL on
// disk, yields a stored session ID and a running watcher.
func TestDiscoverSession_Success(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wd := "/work/dir"

	writePIDFile(t, home, 555, pidEntry{PID: 555, SessionID: "sid-1", CWD: wd})
	jsonlDir := filepath.Join(home, ccProjectsDir, "-work-dir")
	if err := os.MkdirAll(jsonlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jsonlDir, "sid-1.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	b := &Backend{preSendOffset: -1}
	b.pane = &tmuxPane{pid: 555}
	b.workDir = wd

	b.discoverSession()

	if b.sessionID != "sid-1" {
		t.Errorf("sessionID = %q, want sid-1", b.sessionID)
	}
	if b.watcher == nil {
		t.Fatal("watcher should be running after discovery")
	}
	detachPane(t, b)
}

// TestDiscoverSession_WatcherInitFails proves discovery defers (leaves no
// session ID) when the PID file resolves but the session JSONL is missing.
func TestDiscoverSession_WatcherInitFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writePIDFile(t, home, 556, pidEntry{PID: 556, SessionID: "sid-2", CWD: "/x"})
	// No JSONL file created → startWatcher fails.

	b := &Backend{preSendOffset: -1}
	b.pane = &tmuxPane{pid: 556}
	b.workDir = "/x"

	b.discoverSession()

	if b.sessionID != "" {
		t.Errorf("sessionID = %q, want empty (deferred)", b.sessionID)
	}
	if b.watcher != nil {
		t.Error("watcher should not be set on failure")
	}
}

// ---------------------------------------------------------------------------
// ensureWatcher
// ---------------------------------------------------------------------------

// TestEnsureWatcher_AlreadyRunning proves ensureWatcher is a fast no-op when
// the watcher is already up.
func TestEnsureWatcher_AlreadyRunning(t *testing.T) {
	b := &Backend{watcher: &sessionWatcher{}}
	if err := b.ensureWatcher(context.Background()); err != nil {
		t.Fatalf("ensureWatcher: %v", err)
	}
}

// TestEnsureWatcher_PaneDead proves discovery aborts with a process-exited
// error as soon as the tmux session disappears (e.g. bad --resume UUID),
// instead of burning the full 30s discovery window.
func TestEnsureWatcher_PaneDead(t *testing.T) {
	f := &fakeTmux{respond: func([]string, string) (string, error) {
		return "", errors.New("no server") // has-session fails → pane dead
	}}
	b := &Backend{}
	b.pane = &tmuxPane{windowName: "cc-x", exec: f.exec, pid: -1}

	err := b.ensureWatcher(context.Background())
	if err == nil || !strings.Contains(err.Error(), "claude process exited") {
		t.Fatalf("err = %v, want process-exited error", err)
	}
	b.mu.Lock()
	starting := b.watcherStarting
	b.mu.Unlock()
	if starting {
		t.Error("watcherStarting should be reset after failure")
	}
}

// TestEnsureWatcher_TimesOutWithLastError proves the discovery loop reports
// the most recent failure cause when the deadline expires while the pane is
// alive but no claude child/session can be found.
func TestEnsureWatcher_TimesOutWithLastError(t *testing.T) {
	f := &fakeTmux{} // has-session succeeds → pane alive
	b := &Backend{}
	b.pane = &tmuxPane{windowName: "cc-x", exec: f.exec, pid: -1} // PID -1 → no child ever

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := b.ensureWatcher(ctx)
	if err == nil || !strings.Contains(err.Error(), "timeout waiting for claude session") {
		t.Fatalf("err = %v, want discovery timeout", err)
	}
	if !strings.Contains(err.Error(), "no child process") {
		t.Errorf("err = %v, should carry the last discovery failure", err)
	}
}

// TestEnsureWatcher_WaitsForConcurrentDiscovery proves a second caller blocks
// while another goroutine holds the discovery slot and returns nil once that
// discovery finishes.
func TestEnsureWatcher_WaitsForConcurrentDiscovery(t *testing.T) {
	b := &Backend{watcherStarting: true}

	go func() {
		time.Sleep(50 * time.Millisecond)
		b.mu.Lock()
		b.watcher = &sessionWatcher{}
		b.watcherStarting = false
		b.mu.Unlock()
	}()

	if err := b.ensureWatcher(context.Background()); err != nil {
		t.Fatalf("ensureWatcher: %v", err)
	}
}

// TestEnsureWatcher_ConcurrentWaitHonoursContext proves the wait-for-other-
// discoverer loop gives up with the context error when cancelled.
func TestEnsureWatcher_ConcurrentWaitHonoursContext(t *testing.T) {
	b := &Backend{watcherStarting: true} // never resolves

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	err := b.ensureWatcher(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline", err)
	}
}

// ---------------------------------------------------------------------------
// checkPermissionPrompt / Close — driven through the fake pane
// ---------------------------------------------------------------------------

// permissionPromptPane is a realistic CC permission prompt as captured from
// the tmux pane.
const permissionPromptPane = `───────────────────
  Edit file
  src/main.go

  Do you want to proceed?

  ❯ 1. Yes
    2. Yes, allow all edits during this session (shift+tab)
    3. No

  Esc to cancel
`

// TestCheckPermissionPrompt_FullPipeline proves the real detection state
// machine end to end through pane capture: no prompt → prompt detected and
// dispatched with structured choices → identical prompt deduped → prompt
// disappearance fires onPermCleared exactly once.
func TestCheckPermissionPrompt_FullPipeline(t *testing.T) {
	pane, f := newFakePane("cc-w")
	paneContent := "booting..."
	f.respond = func(args []string, _ string) (string, error) {
		return paneContent, nil
	}

	b := &Backend{}
	b.pane = pane

	type dispatched struct {
		summary string
		choices int
	}
	var prompts []dispatched
	cleared := 0
	b.SetPermissionPromptFunc(func(_, _, summary string, choices []delegator.PromptChoice) {
		prompts = append(prompts, dispatched{summary, len(choices)})
	})
	b.SetOnPromptsCleared(func() { cleared++ })

	// 1. No prompt on screen: nothing fires.
	b.checkPermissionPrompt()
	if len(prompts) != 0 || cleared != 0 {
		t.Fatalf("idle pane fired prompts=%v cleared=%d", prompts, cleared)
	}

	// 2. Prompt appears: dispatched once with summary and all choices.
	paneContent = permissionPromptPane
	b.checkPermissionPrompt()
	if len(prompts) != 1 {
		t.Fatalf("prompt dispatched %d times, want 1", len(prompts))
	}
	if prompts[0].summary != "Edit file src/main.go" || prompts[0].choices != 3 {
		t.Errorf("dispatched = %+v", prompts[0])
	}

	// 3. Same prompt still on screen: deduped.
	b.checkPermissionPrompt()
	if len(prompts) != 1 {
		t.Errorf("identical prompt re-dispatched (%d times)", len(prompts))
	}

	// 4. Prompt disappears (user answered): cleared fires once.
	paneContent = "tool running..."
	b.checkPermissionPrompt()
	if cleared != 1 {
		t.Fatalf("cleared = %d, want 1", cleared)
	}

	// 5. Still no prompt: cleared does not fire again.
	b.checkPermissionPrompt()
	if cleared != 1 {
		t.Errorf("cleared = %d after second idle check, want 1", cleared)
	}
}

// TestCheckPermissionPrompt_PlainTextFallback proves that without a
// structured prompt callback, the prompt is forwarded as plain text through
// replyFunc with reply instructions.
func TestCheckPermissionPrompt_PlainTextFallback(t *testing.T) {
	pane, f := newFakePane("cc-w")
	f.respond = func([]string, string) (string, error) {
		return permissionPromptPane, nil
	}
	b := &Backend{}
	b.pane = pane

	var reply string
	b.replyMu.Lock()
	b.replyFunc = func(text string) { reply = text }
	b.replyMu.Unlock()

	b.checkPermissionPrompt()

	if !strings.Contains(reply, "Do you want to proceed?") || !strings.Contains(reply, "Reply with your choice") {
		t.Errorf("fallback reply = %q", reply)
	}
}

// TestCheckPermissionPrompt_CaptureFailure proves a pane capture error is
// swallowed without state changes or callback fires.
func TestCheckPermissionPrompt_CaptureFailure(t *testing.T) {
	pane, f := newFakePane("cc-w")
	f.respond = func([]string, string) (string, error) {
		return "", errors.New("pane gone")
	}
	b := &Backend{}
	b.pane = pane
	b.SetOnPromptsCleared(func() { t.Error("cleared fired on capture failure") })

	b.checkPermissionPrompt() // must not panic or fire callbacks
}

// TestClose_TearsDownLivePane proves Close attempts a graceful /exit and,
// when the session is still alive, force-kills it via Ctrl-C + kill-session,
// then drops the pane and watcher references.
func TestClose_TearsDownLivePane(t *testing.T) {
	shortEnterDelays(t)
	pane, f := newFakePane("cc-w") // has-session succeeds → still alive after /exit
	b := &Backend{}
	b.pane = pane

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := textSends(f); len(got) != 1 || got[0] != "/exit" {
		t.Errorf("graceful exit sends = %v, want [/exit]", got)
	}
	if len(f.callsFor("kill-session")) != 1 {
		t.Error("live session should be force-killed")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pane != nil || b.watcher != nil {
		t.Error("pane and watcher should be nil after Close")
	}
}

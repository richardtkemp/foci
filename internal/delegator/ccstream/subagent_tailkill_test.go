package ccstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestPostToolUse_NeverStopsASubagentTail pins the invariant the #1934 fix
// establishes: the Agent PostToolUse hook must not stop a transcript tail,
// whatever that tail is labelled.
//
// Measured on CC 2.1.261 (timing.sh): a BACKGROUND Agent PostToolUse fires
// +0.03s — at launch, while the subagent runs on for seconds or minutes. A
// FOREGROUND one fires ~30ms AFTER the real end, so nothing is lost by letting
// task_notification:completed be the only stop for both.
//
// Armed FOREGROUND deliberately: that is what production does today for a
// default-background subagent, because run_in_background is absent from the
// tool input and was read as false. The tail must survive regardless, so this
// test keeps guarding even once the labelling is fixed.
func TestPostToolUse_NeverStopsASubagentTail(t *testing.T) {
	withFastTail(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	const (
		toolUseID = "toolu_survives"
		agentID   = "a0000000000000001"
	)
	b := &Backend{workDir: "/home/foci/clutch", hookInstallID: "install-a"}
	b.sessionID = "6bd15e3c-0000-0000-0000-000000000000"

	path := b.subagentFilePath(agentID, ".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(assistantLine("BEFORE-HOOK")), 0o644); err != nil {
		t.Fatal(err)
	}

	// lines counts what the tail actually read, independent of whether its text
	// is forwarded — so this asserts the TAIL survived, not the text routing.
	linesRead := func() int64 {
		m := b.subagentTails()
		m.mu.Lock()
		defer m.mu.Unlock()
		if t := m.tails[toolUseID]; t != nil {
			return t.lines.Load()
		}
		return -1 // tail gone
	}

	// Production arms this for a default-background subagent (the bug).
	b.subagentTails().expectForeground(toolUseID)

	raw, _ := json.Marshal(TaskEvent{
		Type: "system", Subtype: "task_started",
		TaskID: agentID, ToolUseID: toolUseID,
	})
	b.OnSystem("task_started", raw)
	waitFor(t, func() bool { return linesRead() == 1 })

	// The Agent PostToolUse hook — for a background subagent this fires at
	// LAUNCH, with the whole run still ahead of it.
	stdout, _ := json.Marshal(hookScriptOutput{
		HookEvent: "PostToolUse", InstallID: "install-a",
		ToolUseID: toolUseID, ToolName: "Agent",
	})
	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse", Stdout: string(stdout), ExitCode: 0, Outcome: "success",
	})
	b.handleHookResponse(env)

	// Everything the subagent writes AFTER that hook must still be collected.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(assistantLine("AFTER-HOOK"))
	f.Close()

	waitFor(t, func() bool { return linesRead() == 2 })
	if n := linesRead(); n != 2 {
		t.Fatalf("lines read after the hook = %d, want 2 (-1 means the tail was stopped)", n)
	}
	b.subagentTails().finalize(toolUseID)
}

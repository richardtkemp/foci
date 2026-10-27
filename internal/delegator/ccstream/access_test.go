package ccstream

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/procx"
)

// launchArgs starts a real Backend through Start with the binary pointed at a
// recorder script, and returns the arguments that process was given, one per
// element — the launch flags as Claude Code would see them.
//
// The backend's own warnings (no hook binary under test, the recorder's EOF)
// go to a discarding warn hook, cleared once the process is gone: left in the
// log package's startup buffer they would be replayed into the next test that
// captures warnings.
func launchArgs(t *testing.T, cfg map[string]any, batch bool, access delegator.BatchAccess) []string {
	t.Helper()
	log.SetWarnHook(func(log.Level, string, string) {})
	dir := t.TempDir()
	out := filepath.Join(dir, "args.txt")
	script := filepath.Join(dir, "fake-claude")
	// The recorder writes to a scratch file and renames it into place: the
	// rename is atomic, so args.txt appears only once every argument is in
	// it. Writing args.txt directly let the poll below read it mid-write and
	// return a truncated list (#2593).
	body := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + out + ".tmp' && mv '" + out + ".tmp' '" + out + "'\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write recorder script: %v", err)
	}
	cfg["binary"] = script
	b, err := newFromConfig(cfg)
	if err != nil {
		t.Fatalf("newFromConfig: %v", err)
	}
	be := b.(*Backend)
	if err := be.Start(context.Background(), delegator.StartOptions{WorkDir: dir, AgentID: "argtest", SkipPermissions: batch, BatchAccess: access}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		for deadline := time.Now().Add(10 * time.Second); be.IsRunning() && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
		}
		_ = be.Close()
		log.SetWarnHook(nil)
	})
	var data []byte
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err = os.ReadFile(out); err == nil && len(data) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(data) == 0 {
		t.Fatal("recorder script produced no args — subprocess never ran")
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// flagValue returns the value following flag in args, or "" if absent.
func flagValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// TestStart_AccessLaunchArgs proves each kind of session launches Claude
// Code with the flags that give it its access, read back from a spawned
// process: an ordinary session gets the agent's rules only; foci's own batch
// runs in auto mode (unattendedPermissionArgs); a parent-access batch (spawn
// character) launches exactly as the agent's sessions do — no auto mode, the
// agent's rules; a restricted batch gets only its tool list (pre-approved
// under dontAsk, no MCP), never the agent's rules, auto mode or the
// operator's --dangerously-skip-permissions, and isolated adds --restricted.
func TestStart_AccessLaunchArgs(t *testing.T) {
	const agentRules = "Bash(git:*),Write(/tmp/**)"
	cases := []struct {
		name         string
		cfgSkip      bool
		batch        bool
		access       delegator.BatchAccess
		skip         bool
		restricted   bool
		mode         string
		tools        string // "" = no --tools flag
		allowedTools string
	}{
		{"ordinary", false, false, delegator.BatchAccessDefault, false, false, "", "", agentRules},
		{"foci batch", false, true, delegator.BatchAccessDefault, false, false, "auto", "", agentRules},
		{"parent batch", false, true, delegator.BatchAccessParent, false, false, "", "", agentRules},
		{"parent batch, operator skip", true, true, delegator.BatchAccessParent, true, false, "", "", agentRules},
		{"isolated batch", false, true, delegator.BatchAccessIsolated, false, true, "dontAsk", "Read,Write,Edit,Glob,Grep,WebSearch,WebFetch", "Read,Write,Edit,Glob,Grep,WebSearch,WebFetch"},
		{"read-only batch", false, true, delegator.BatchAccessReadOnly, false, false, "dontAsk", "Read,Glob,Grep,WebSearch,WebFetch", "Read,Glob,Grep,WebSearch,WebFetch"},
		{"read-only batch, operator skip", true, true, delegator.BatchAccessReadOnly, false, false, "dontAsk", "Read,Glob,Grep,WebSearch,WebFetch", "Read,Glob,Grep,WebSearch,WebFetch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := map[string]any{"allowed_tools": agentRules}
			if c.cfgSkip {
				cfg["skip_permissions"] = true
			}
			args := launchArgs(t, cfg, c.batch, c.access)
			if got := slices.Contains(args, "--dangerously-skip-permissions"); got != c.skip {
				t.Errorf("--dangerously-skip-permissions = %v, want %v (args %q)", got, c.skip, args)
			}
			if got := slices.Contains(args, "--restricted"); got != c.restricted {
				t.Errorf("--restricted = %v, want %v", got, c.restricted)
			}
			if got := flagValue(args, "--permission-mode"); got != c.mode {
				t.Errorf("--permission-mode = %q, want %q", got, c.mode)
			}
			if got := flagValue(args, "--tools"); got != c.tools {
				t.Errorf("--tools = %q, want %q", got, c.tools)
			}
			if got := flagValue(args, "--allowedTools"); got != c.allowedTools {
				t.Errorf("--allowedTools = %q, want %q", got, c.allowedTools)
			}
			if n := strings.Count(strings.Join(args, "\n"), "--allowedTools"); n > 1 {
				t.Errorf("--allowedTools passed %d times", n)
			}
			if got := slices.Contains(args, "--strict-mcp-config"); got != (c.tools != "") {
				t.Errorf("--strict-mcp-config = %v, want %v", got, c.tools != "")
			}
		})
	}
}

// TestReadOnlyBatchTools_NothingWritesOrRuns proves the read-only tool list
// holds no Claude Code tool that writes a file or runs code, and the isolated
// list no tool that runs code (its writes are confined by --restricted).
func TestReadOnlyBatchTools_NothingWritesOrRuns(t *testing.T) {
	t.Parallel()
	writes := []string{"Write", "Edit", "MultiEdit", "NotebookEdit"}
	runs := []string{"Bash", "PowerShell", "Agent", "Task", "Skill", "BashOutput", "KillShell", "Monitor"}
	for _, tool := range readOnlyBatchTools {
		if slices.Contains(writes, tool) || slices.Contains(runs, tool) {
			t.Errorf("read-only batch tools include %s", tool)
		}
	}
	for _, tool := range isolatedBatchTools {
		if slices.Contains(runs, tool) {
			t.Errorf("isolated batch tools include %s", tool)
		}
	}
}

// TestLive_RestrictedBatchRefusesWritesAndCommands runs the real claude CLI
// with the read-only and isolated batch launch flags (unattendedPermissionArgs,
// with the operator's skip_permissions set, which must not loosen them) and
// asks it to
// write files and run a command outside its directory; none of them may
// appear. It needs a logged-in claude on PATH and spends a few cents, so it
// runs only with FOCI_LIVE_CLAUDE=1.
func TestLive_RestrictedBatchRefusesWritesAndCommands(t *testing.T) {
	if os.Getenv("FOCI_LIVE_CLAUDE") != "1" {
		t.Skip("set FOCI_LIVE_CLAUDE=1 to run against the real claude CLI")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not on PATH")
	}
	for _, access := range []delegator.BatchAccess{delegator.BatchAccessReadOnly, delegator.BatchAccessIsolated} {
		t.Run(access.String(), func(t *testing.T) {
			work := t.TempDir()
			outside := t.TempDir()
			made := filepath.Join(work, "made.txt")
			escaped := filepath.Join(outside, "escaped.txt")
			ran := filepath.Join(outside, "ran.txt")
			prompt := "Do all three, using any tool you have, then report what happened: " +
				"1) create " + made + " containing x; 2) create " + escaped + " containing x; " +
				"3) run the shell command: touch " + ran
			// The prompt goes on stdin: accessArgs ends in variadic flags
			// (--allowedTools), which would swallow a trailing argument.
			args := append([]string{"-p", "--model", "haiku"}, unattendedPermissionArgs(true, true, access)...)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cmd := procx.Spawn(ctx, procx.Operator, bin, args...)
			cmd.Dir = work
			cmd.Stdin = strings.NewReader(prompt)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("claude: %v\n%s", err, out)
			}
			for _, p := range []string{escaped, ran} {
				if _, err := os.Stat(p); err == nil {
					t.Errorf("%s batch created %s\nclaude said:\n%s", access, p, out)
				}
			}
			_, err = os.Stat(made)
			if wrote := err == nil; wrote != (access == delegator.BatchAccessIsolated) {
				t.Errorf("%s batch wrote inside its directory = %v\nclaude said:\n%s", access, wrote, out)
			}
		})
	}
}

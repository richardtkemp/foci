package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// #2685: the shell call a direct spawn runs in is found from the exec-bridge
// caller's ancestors, and matched to the backend's tracked command.

func TestShellCallMatch(t *testing.T) {
	t.Parallel()
	// Claude Code's own wrapping of a background Bash call, as read from
	// /proc on CC 2.1.314: eval '<command>', each ' written as '"'"'.
	ccLine := normalizeShellText(`/usr/bin/bash -c source /home/foci/.claude/shell-snapshots/snapshot-bash-1.sh 2>/dev/null || true && eval 'foci_spawn --context explore "it'"'"'s a '"'"'quoted'"'"' \"test\""; make -C /x land' && pwd -P >| /tmp/claude-1fbf-cwd`)
	cmd := `foci_spawn --context explore "it's a 'quoted' \"test\""; make -C /x land`
	cases := []struct {
		name, command, shell string
		want                 bool
	}{
		{"the call itself", cmd, ccLine, true},
		{"a truncated command (FullCommand's cut)", cmd[:20] + "\n…", ccLine, true},
		{"another command", "sleep 100", ccLine, false},
		{"no caller shell", cmd, "", false},
		{"an empty command", "", ccLine, false},
	}
	for _, c := range cases {
		if got := ShellCallMatch(c.command, c.shell) > 0; got != c.want {
			t.Errorf("%s: match = %v, want %v", c.name, got, c.want)
		}
	}
	if long, short := ShellCallMatch(cmd, ccLine), ShellCallMatch(`foci_spawn --context explore`, ccLine); long <= short || short == 0 {
		t.Errorf("match lengths: full %d, prefix %d; want the full command to score higher", long, short)
	}
}

// startShellTree starts a real process tree: this test process (standing in
// for the gateway) runs an outer shell (the backend), which runs the shell
// call (marked SHELL-CALL-MARK), which runs the caller (CALLER-ARGS). It
// returns the caller's pid.
func startShellTree(t *testing.T) int {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	caller := filepath.Join(dir, "caller.sh")
	if err := os.WriteFile(caller, []byte("echo $$ > "+pidFile+"\nsleep 30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Each level ends with "; :" so a shell forks instead of exec'ing its last command.
	outer := exec.Command("bash", "-c", "bash -c 'sh "+caller+" CALLER-ARGS; : SHELL-CALL-MARK'; : BACKEND-MARK")
	outer.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // so cleanup kills the whole tree
	if err := outer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-outer.Process.Pid, syscall.SIGKILL); _ = outer.Wait() })
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(b), "\n") {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			return pid
		}
	}
	t.Fatal("the caller never wrote its pid")
	return 0
}

// callerShell(caller) holds the shell call's command line, and neither the
// caller's own arguments nor the backend's.
func TestCallerShell_RealProcessTree(t *testing.T) {
	t.Parallel()
	got := callerShell(startShellTree(t))
	if !strings.Contains(got, "SHELL-CALL-MARK") {
		t.Errorf("callerShell = %q, want the shell call's command line", got)
	}
	if strings.Contains(got, "BACKEND-MARK") {
		t.Errorf("callerShell = %q, holds the backend's (the gateway's child's) command line", got)
	}
	if strings.Count(got, "CALLER-ARGS") != 1 {
		// Once, inside the shell call's own line; the caller's line is left out.
		t.Errorf("callerShell = %q, want CALLER-ARGS only from the shell call's line", got)
	}
	if strings.ContainsAny(got, `'"`) {
		t.Errorf("callerShell = %q, still has quotes", got)
	}
}

func TestCallerShell_Unknown(t *testing.T) {
	t.Parallel()
	if got := callerShell(0); got != "" {
		t.Errorf("callerShell(0) = %q", got)
	}
	// A child of the gateway itself (an API agent's exec) has no shell call
	// above it that the gateway did not start.
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	if got := callerShell(child.Process.Pid); got != "" {
		t.Errorf("callerShell(a child of the gateway) = %q, want empty", got)
	}
}

// The caller pid reaches runDirect's entry, as CallerShell.
func TestDirectSpawn_EntryCarriesCallerShell(t *testing.T) {
	t.Parallel()
	s, obs, _ := newObservedDirectSpawner("dobs5")
	ctx := WithCallerPID(WithSessionKey(context.Background(), "dobs5/c1"), startShellTree(t))
	if _, err := s.runDirect(ctx, s, spawnModeRaw, "p", "", time.Minute, func(context.Context) (ToolResult, error) {
		return TextResult("ok"), nil
	}); err != nil {
		t.Fatal(err)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.started) != 1 || !strings.Contains(obs.started[0].CallerShell, "SHELL-CALL-MARK") {
		t.Errorf("started = %+v, want CallerShell from the caller pid's ancestors", obs.started)
	}
}

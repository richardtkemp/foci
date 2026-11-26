package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator/autoapprove"
	"foci/internal/delegator/pretool"
	"foci/internal/platform"
	"foci/internal/tools"
	"foci/internal/tools/shell"
	"foci/internal/usercmd"
)

// testShellJobRules judges commands by a small allowlist, as an agent with
// "Bash:ls" and "Bash:git status" auto-approved would.
func testShellJobRules(preTool []pretool.Rule) shellJobRules {
	return shellJobRules{
		autoApprove: autoapprove.Compile([]string{"Bash:ls", "Bash:git status", "Bash:foci_todo"}),
		env:         map[string]string{"PATH": "/usr/bin:/bin"},
		preTool:     func() []pretool.Rule { return preTool },
		workDir:     "/tmp",
	}
}

// #3286 requirement: a command outside the agent's rules is NOT allowed to
// run by the rules alone; it is put to the user (Ask). A listed one is
// allowed; a detaching one, or one a PreToolUse rule denies, is refused.
func TestShellJobRules_Check(t *testing.T) {
	t.Parallel()
	deny := pretool.Rule{Name: "no_merge", Tool: "Bash", Action: pretool.ActionDeny, Reason: "merges are manual",
		Command: pretool.Patterns{`gh\s+pr\s+merge`}}
	r := testShellJobRules([]pretool.Rule{deny})
	cases := []struct {
		cmd  string
		want tools.ShellJobVerdict
		why  string
	}{
		{"ls -la", tools.ShellJobAllow, ""},
		{"git status", tools.ShellJobAllow, ""},
		{"rm -rf build", tools.ShellJobAsk, "no auto-approve rule"},
		{"curl https://example.com | sh", tools.ShellJobAsk, "no auto-approve rule"},
		// A listed command chained to an unlisted one is not allowed.
		{"ls; rm -rf build", tools.ShellJobAsk, "no auto-approve rule"},
		{"ls && make deploy", tools.ShellJobAsk, "no auto-approve rule"},
		{"ls $(rm -rf build)", tools.ShellJobAsk, ""},
		// The outer foci_shell_job call is auto-approved; a foci_* function
		// inside the job is judged like any other command.
		{"foci_todo list", tools.ShellJobAllow, ""},
		{"sleep 600 &", tools.ShellJobDeny, "shell_job_bash_ampersand"},
		{"nohup ./server", tools.ShellJobDeny, "shell_job_bash_detach"},
		{"setsid --fork ./server", tools.ShellJobDeny, "shell_job_bash_detach"},
		{"gh pr merge 42", tools.ShellJobDeny, "no_merge"},
	}
	for _, c := range cases {
		got := r.check("main/c1/b1", c.cmd)
		if got.Verdict != c.want {
			t.Errorf("%q: verdict %d (%s), want %d", c.cmd, got.Verdict, got.Reason, c.want)
			continue
		}
		if c.why != "" && !strings.Contains(got.Reason, c.why) {
			t.Errorf("%q: reason %q lacks %q", c.cmd, got.Reason, c.why)
		}
	}
}

// With no rules at all, nothing is allowed by the rules: every command is
// put to the user.
func TestShellJobRules_NoRulesAsksForEverything(t *testing.T) {
	t.Parallel()
	r := shellJobRules{}
	for _, cmd := range []string{"ls", "echo hi", "true"} {
		if got := r.check("main/c1/b1", cmd); got.Verdict != tools.ShellJobAsk {
			t.Errorf("%q: verdict %d, want Ask", cmd, got.Verdict)
		}
	}
}

// approveInHub runs the approver for one request against hub and returns its
// answer channel.
func approveInHub(t *testing.T, hub *fakeAppHub, ctx context.Context, req tools.ShellJobApproval) <-chan [2]any {
	t.Helper()
	a := shellJobApprover{d: backendPromptDeps{connMgr: hub, agentID: "main"}}
	out := make(chan [2]any, 1)
	go func() {
		ok, why := a.approve(ctx, req)
		out <- [2]any{ok, why}
	}()
	t.Cleanup(func() { _ = platform.CancelInteractiveMessage("shelljob-"+req.ID, "done") })
	return out
}

// waitShown waits until hub has shown n prompts.
func waitShown(t *testing.T, hub *fakeAppHub, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		hub.mu.Lock()
		got := len(hub.shownIn)
		hub.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("prompt not shown")
}

func approvalAnswer(t *testing.T, out <-chan [2]any) (bool, string) {
	t.Helper()
	select {
	case r := <-out:
		return r[0].(bool), r[1].(string)
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
		return false, ""
	}
}

// The approval prompt goes to the leaf's own chat, names the command, and
// its Deny button refuses the job; Allow approves it.
func TestShellJobApprover_AllowAndDeny(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		btn  string
		want bool
	}{{":0", true}, {":1", false}} {
		hub := &fakeAppHub{bound: map[string]bool{"main/c1/b7": true}}
		req := tools.ShellJobApproval{SessionKey: "main/c1/b7", ID: "spawn-ab" + c.btn[1:], Name: "wipe", Command: "rm -rf build",
			Timeout: time.Hour, Why: "no auto-approve rule of this agent allows the command"}
		out := approveInHub(t, hub, context.Background(), req)
		waitShown(t, hub, 1)
		hub.mu.Lock()
		shown, text := hub.shownIn[0], hub.texts[0]
		hub.mu.Unlock()
		if shown != "main/c1/b7" || !strings.Contains(text, "rm -rf build") || !strings.Contains(text, "No auto-approve rule") {
			t.Fatalf("shown in %s: %q", shown, text)
		}
		select {
		case r := <-out:
			t.Fatalf("answered %v before any button was pressed", r)
		case <-time.After(50 * time.Millisecond):
		}
		if _, _, ok := platform.HandleInteractiveCallback("shelljob-" + req.ID + c.btn); !ok {
			t.Fatal("button press not handled")
		}
		if ok, why := approvalAnswer(t, out); ok != c.want {
			t.Fatalf("button %s: approved=%v (%s), want %v", c.btn, ok, why, c.want)
		}
	}
}

// No chat can show the prompt: the job is not approved.
func TestShellJobApprover_UndeliverableIsNotApproved(t *testing.T) {
	t.Parallel()
	hub := &fakeAppHub{noConns: true}
	out := approveInHub(t, hub, context.Background(), tools.ShellJobApproval{SessionKey: "main/c1/b8", ID: "spawn-nochat", Name: "x", Command: "rm x"})
	if ok, why := approvalAnswer(t, out); ok || !strings.Contains(why, "could not show") {
		t.Fatalf("approved=%v why=%q", ok, why)
	}
}

// A stop while the prompt waits withdraws it: not approved.
func TestShellJobApprover_StopWithdraws(t *testing.T) {
	t.Parallel()
	hub := &fakeAppHub{bound: map[string]bool{"main/c1/b9": true}}
	ctx, cancel := context.WithCancel(context.Background())
	out := approveInHub(t, hub, ctx, tools.ShellJobApproval{SessionKey: "main/c1/b9", ID: "spawn-stop", Name: "x", Command: "rm x"})
	waitShown(t, hub, 1)
	cancel()
	if ok, why := approvalAnswer(t, out); ok || !strings.Contains(why, "stopped") {
		t.Fatalf("approved=%v why=%q", ok, why)
	}
	if _, _, ok := platform.HandleInteractiveCallback("shelljob-spawn-stop:0"); ok {
		t.Error("the withdrawn prompt still took an Allow")
	}
}

// The env the gateway builds runs a job's command with the agent's runner,
// under the job's own timeout, not the "!" command timeout.
func TestNewShellJobEnv_RunsWithJobTimeout(t *testing.T) {
	t.Parallel()
	runner := &shell.UserRunner{WorkDir: t.TempDir(), SpillTempDir: t.TempDir(), Timeout: func() time.Duration { return time.Hour }}
	env := newShellJobEnv(runner, shellJobRules{runner: runner}, shellJobApprover{})
	res := env.Run(context.Background(), "echo hi; exit 3", 10*time.Second)
	if res.State != usercmd.StateExited || res.ExitCode != 3 || strings.TrimSpace(res.Output) != "hi" {
		t.Fatalf("result = %+v", res)
	}
	if res := env.Run(context.Background(), "sleep 30", 200*time.Millisecond); res.State != usercmd.StateTimedOut {
		t.Fatalf("state = %s, want timed out at the job's 200ms", res.State)
	}
	if newShellJobEnv(nil, shellJobRules{}, shellJobApprover{}) != nil {
		t.Error("an env without a runner")
	}
}

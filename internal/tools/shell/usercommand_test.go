package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"foci/internal/procx"
	"foci/internal/secrets"
	"foci/internal/tools"
	"foci/internal/usercmd"
)

const testSecretValue = "sk-test-2812-very-secret"

func userRunner(t *testing.T, timeout time.Duration, threshold int64) *UserRunner {
	t.Helper()
	return &UserRunner{
		WorkDir:        t.TempDir(),
		SpillThreshold: func() int64 { return threshold },
		SpillTempDir:   t.TempDir(),
		Timeout:        func() time.Duration { return timeout },
	}
}

func userRunnerWithSecret(t *testing.T, timeout time.Duration, threshold int64) *UserRunner {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.toml")
	if err := os.WriteFile(path, []byte("[brave]\napi_key = \""+testSecretValue+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := secrets.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r := userRunner(t, timeout, threshold)
	r.Store = store
	return r
}

func TestUserRunner_RunsInWorkspaceAndReportsExit(t *testing.T) {
	t.Parallel()
	r := userRunner(t, 10*time.Second, 15000)
	res := r.Run(context.Background(), "pwd; echo hi")
	if res.State != usercmd.StateExited || res.ExitCode != 0 {
		t.Fatalf("state = %s code = %d", res.State, res.ExitCode)
	}
	lines := strings.Split(strings.TrimSpace(res.Output), "\n")
	if len(lines) != 2 || lines[1] != "hi" {
		t.Fatalf("output = %q", res.Output)
	}
	want, _ := filepath.EvalSymlinks(r.WorkDir)
	if got, _ := filepath.EvalSymlinks(lines[0]); got != want {
		t.Errorf("cwd = %q, want the workspace %q", got, want)
	}
	if res.File != "" {
		t.Errorf("File = %q under the limit", res.File)
	}
}

func TestUserRunner_MultiLineScriptAndExitCode(t *testing.T) {
	t.Parallel()
	res := userRunner(t, 10*time.Second, 15000).Run(context.Background(), "echo a\necho b\nexit 3")
	if res.Output != "a\nb\n" {
		t.Errorf("output = %q", res.Output)
	}
	if res.State != usercmd.StateExited || res.ExitCode != 3 {
		t.Errorf("state = %s code = %d, want exited 3", res.State, res.ExitCode)
	}
}

// Dick's rulings: no path blocking, sleep permitted. The shell tool's own refusal
// of both stays (TestExec* in shell_test.go are unedited).
func TestUserRunner_SleepAndBlockedPathRun(t *testing.T) {
	t.Parallel()
	r := userRunnerWithSecret(t, 10*time.Second, 15000)
	blocked := filepath.Join(r.WorkDir, "blocked.txt")
	if err := os.WriteFile(blocked, []byte("readable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.Store.AddBlockedPaths([]string{blocked})
	if !r.Store.IsBlockedCommand("cat " + blocked) {
		t.Fatal("control: the store does not block the path")
	}
	script := "sleep 0.1; echo done; cat " + blocked
	if err := r.Check(script); err != nil {
		t.Fatalf("Check refused: %v", err)
	}
	res := r.Run(context.Background(), script)
	if !strings.Contains(res.Output, "done") || !strings.Contains(res.Output, "readable") {
		t.Errorf("output = %q", res.Output)
	}
}

func TestUserRunner_RefusesSecretTemplate(t *testing.T) {
	t.Parallel()
	r := userRunnerWithSecret(t, 10*time.Second, 15000)
	if err := r.Check("echo {{secret:brave.api_key}}"); err == nil {
		t.Fatal("a {{secret:}} template was not refused")
	}
	if err := r.Check("echo plain"); err != nil {
		t.Fatalf("a plain script was refused: %v", err)
	}
}

func TestUserRunner_ScrubsCommandAndOutput(t *testing.T) {
	t.Parallel()
	r := userRunnerWithSecret(t, 10*time.Second, 15000)
	res := r.Run(context.Background(), fmt.Sprintf(`printf '%%s' %q`, testSecretValue))
	if strings.Contains(res.Output, testSecretValue) || strings.Contains(res.Command, testSecretValue) {
		t.Fatalf("secret survived: output=%q command=%q", res.Output, res.Command)
	}
	if !strings.Contains(res.Output, "[REDACTED]") {
		t.Errorf("output = %q, want the scrubbed form", res.Output)
	}
}

func TestUserRunner_ScrubsSavedFile(t *testing.T) {
	t.Parallel()
	r := userRunnerWithSecret(t, 10*time.Second, 1000)
	res := r.Run(context.Background(), fmt.Sprintf(`seq 1 5000; printf '%%s\n' %q; seq 5001 6000`, testSecretValue))
	if res.File == "" {
		t.Fatal("output did not spill")
	}
	data, err := os.ReadFile(res.File)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(testSecretValue)) {
		t.Fatal("the saved file holds the secret")
	}
	if !bytes.Contains(data, []byte("[REDACTED]")) || !bytes.Contains(data, []byte("\n6000\n")) {
		t.Errorf("saved file lost content (%d bytes)", len(data))
	}
	if strings.Contains(res.Output, "\n6000\n") {
		t.Errorf("Output is meant to be only the start, got %d bytes", len(res.Output))
	}
}

// A value split across two read chunks is still scrubbed, and nothing else moves.
func TestScrubStream_SecretAcrossChunkBoundary(t *testing.T) {
	t.Parallel()
	const secret = "SECRET-VALUE"
	redact := func(s string) string { return strings.ReplaceAll(s, secret, "[R]") }
	for _, at := range []int{scrubChunkBytes - 5, scrubChunkBytes, scrubChunkBytes - scrubOverlapBytes - 3, 2*scrubChunkBytes - 1} {
		in := strings.Repeat("a", at) + secret + strings.Repeat("b", 100)
		var out bytes.Buffer
		if err := scrubStream(strings.NewReader(in), &out, redact); err != nil {
			t.Fatal(err)
		}
		if want := strings.Repeat("a", at) + "[R]" + strings.Repeat("b", 100); out.String() != want {
			t.Errorf("secret at %d: got %d bytes, secret left=%v", at, out.Len(), strings.Contains(out.String(), secret))
		}
	}
}

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func readPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				return pid
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", path)
	return 0
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d is still alive", pid)
}

func TestUserRunner_TimeoutKillsGroupAndKeepsOutput(t *testing.T) {
	t.Parallel()
	r := userRunner(t, 300*time.Millisecond, 15000)
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	res := r.Run(context.Background(), fmt.Sprintf("echo before; sleep 30 & echo $! > %s; wait; echo late", pidFile))
	if res.State != usercmd.StateTimedOut {
		t.Fatalf("state = %s (%s), want timed out", res.State, res.Output)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
	if !strings.Contains(res.Output, "before") || strings.Contains(res.Output, "late") {
		t.Errorf("output = %q", res.Output)
	}
	if !strings.Contains(res.Status(), "timed out after 300ms") {
		t.Errorf("status = %q", res.Status())
	}
	waitDead(t, readPid(t, pidFile))
}

func TestUserRunner_CancelKillsGroupAndKeepsOutput(t *testing.T) {
	t.Parallel()
	r := userRunner(t, time.Minute, 15000)
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan usercmd.Result, 1)
	go func() {
		done <- r.Run(ctx, fmt.Sprintf("echo before; sleep 30 & echo $! > %s; wait; echo after", pidFile))
	}()
	pid := readPid(t, pidFile)
	if !pidAlive(pid) {
		t.Fatal("control: the child is not running")
	}
	cancel()
	select {
	case res := <-done:
		if res.State != usercmd.StateCancelled {
			t.Errorf("state = %s", res.State)
		}
		if !strings.Contains(res.Output, "before") || strings.Contains(res.Output, "after") {
			t.Errorf("output = %q", res.Output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not end the command within 5 s")
	}
	waitDead(t, pid)
}

// writeExec creates dir/name with the given mode, running a harmless script.
func writeExec(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho ran-"+name+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUserRunnerRunnable(t *testing.T) {
	binDir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	writeExec(t, binDir, "on-path-2878", 0o755)
	writeExec(t, binDir, "plain-on-path-2878", 0o644)

	r := userRunner(t, 5*time.Second, 1<<20)
	abs := writeExec(t, t.TempDir(), "abs-2878", 0o755)
	writeExec(t, filepath.Join(r.WorkDir, "scripts"), "rel-2878", 0o755)
	writeExec(t, r.WorkDir, "in-cwd-2878", 0o755)
	writeExec(t, home, "in-home-2878", 0o755)
	subdir := filepath.Join(r.WorkDir, "a-dir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		word string
		want bool
	}{
		{"on-path-2878", true},
		{"plain-on-path-2878", false}, // on PATH, not executable
		{"no-such-command-2878", false},
		{abs, true},
		{abs + ".missing", false},
		{"./scripts/rel-2878", true}, // relative to the workspace, not the gateway's cwd
		{"scripts/rel-2878", true},
		{"./in-cwd-2878", true},
		{"in-cwd-2878", false}, // a shell does not look in its cwd for a bare word
		{"~/in-home-2878", true},
		{"./a-dir", false}, // a directory
		{"", false},
		{"nul\x00byte", false},
	} {
		if got := r.Runnable(tc.word); got != tc.want {
			t.Errorf("Runnable(%q) = %v, want %v", tc.word, got, tc.want)
		}
	}
}

// The check must agree with what actually happens when the word is run: a word it
// calls runnable starts, and one it refuses gets the shell's "not found" (127) or
// "cannot execute" (126).
func TestUserRunnerRunnable_AgreesWithRun(t *testing.T) {
	binDir := t.TempDir()
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	writeExec(t, binDir, "on-path-2878", 0o755)
	r := userRunner(t, 10*time.Second, 1<<20)
	writeExec(t, filepath.Join(r.WorkDir, "scripts"), "rel-2878", 0o755)
	writeExec(t, r.WorkDir, "in-cwd-2878", 0o755)
	writeExec(t, r.WorkDir, "plain-2878", 0o644)

	for _, word := range []string{"on-path-2878", "./scripts/rel-2878", "./in-cwd-2878", "in-cwd-2878", "./plain-2878", "no-such-command-2878"} {
		res := r.Run(context.Background(), word)
		ran := res.State == usercmd.StateExited && res.ExitCode == 0 && strings.Contains(res.Output, "ran-")
		if refused := res.ExitCode == 126 || res.ExitCode == 127; !ran && !refused {
			t.Errorf("%q: unexpected result %+v", word, res)
		}
		if got := r.Runnable(word); got != ran {
			t.Errorf("Runnable(%q) = %v, but running it gave exit %d: %q", word, got, res.ExitCode, res.Output)
		}
	}
}

// Dick's ruling: the PATH is the one a "!" command gets, the operator PATH, which
// can differ from the gateway's own. A word found only on the operator PATH is
// runnable and does run; so a check of the gateway's PATH would fail this.
func TestUserRunnerRunnable_UsesTheCommandsPath(t *testing.T) {
	binDir := t.TempDir()
	writeExec(t, binDir, "operator-only-2878", 0o755)
	procx.SetOperatorEnv(map[string]string{"PATH": binDir + ":" + os.Getenv("PATH")})
	t.Cleanup(func() { procx.SetOperatorEnv(nil) })

	r := userRunner(t, 10*time.Second, 1<<20)
	if !r.Runnable("operator-only-2878") {
		t.Error("a word on the operator PATH is not runnable")
	}
	res := r.Run(context.Background(), "operator-only-2878")
	if res.ExitCode != 0 || !strings.Contains(res.Output, "ran-operator-only-2878") {
		t.Errorf("the command itself did not find it: %+v", res)
	}
}

// #2882: a word the shell itself runs counts, and the answer must match what running
// a block that starts with it does. Each script is a real use of its first word.
func TestUserRunnerRunnable_ShellWordsAgreeWithRun(t *testing.T) {
	r := userRunner(t, 10*time.Second, 1<<20)
	for word, script := range map[string]string{
		"cd":     "cd / && pwd",
		"export": "export A=1; echo $A",
		"echo":   "echo hi",
		":":      ": && echo hi",
		".":      ". /dev/null && echo hi",
		"source": "source /dev/null && echo hi",
		"[":      "[ 1 = 1 ] && echo hi",
		"if":     "if true; then echo hi; fi",
		"for":    "for i in hi; do echo $i; done",
		"while":  "while false; do :; done; echo hi",
		"case":   "case x in x) echo hi;; esac",
		"{":      "{ echo hi; }",
		"(":      "( echo hi )",
		"!":      "! false && echo hi",
		"[[":     "[[ 1 == 1 ]] && echo hi",
		"time":   "time echo hi",
	} {
		if !r.Runnable(word) {
			t.Errorf("Runnable(%q) = false, want true", word)
		}
		res := r.Run(context.Background(), script)
		if res.State != usercmd.StateExited || res.ExitCode != 0 {
			t.Errorf("%q does not run: %+v", script, res)
		}
	}

	// A script starting with one of these fails whatever follows.
	for word, script := range map[string]string{
		"then":     "then echo hi",
		"fi":       "fi",
		"done":     "done",
		"}":        "}",
		"esac":     "esac",
		"local":    "local x=1 || exit 9",
		"return":   "return 0 || exit 9",
		"break":    "break 2>&1 | grep -q meaningful && exit 9",
		"continue": "continue 2>&1 | grep -q meaningful && exit 9",
		"logout":   "logout || exit 9",
		"fg":       "fg 1 || exit 9",
		"bg":       "bg 1 || exit 9",
		"suspend":  "suspend || exit 9",
		"compopt":  "compopt -o nospace || exit 9",
	} {
		if r.Runnable(word) {
			t.Errorf("Runnable(%q) = true, want false", word)
		}
		res := r.Run(context.Background(), script)
		if res.State == usercmd.StateExited && res.ExitCode == 0 {
			t.Errorf("%q ran, so a button would have been right: %+v", script, res)
		}
	}

	for _, word := range []string{"FOO=1", "#", "$CMD", "no-such-command-2882", "if;"} {
		if r.Runnable(word) {
			t.Errorf("Runnable(%q) = true, want false", word)
		}
	}
}

// #2882: a block can start with a comment or an assignment. The "!" trigger keeps
// each as a script, and the shell runs the command after it.
func TestUserRunner_LeadingCommentAndAssignmentRun(t *testing.T) {
	r := userRunner(t, 10*time.Second, 1<<20)
	for _, tc := range []struct{ typed, want string }{
		{"!# a note\necho hi", "hi\n"},
		{"!#!/bin/bash\necho hi", "hi\n"},
		{"!FOO=hi env", "FOO=hi"},
		{"!FOO='a b' BAR=$(echo c) env", "FOO=a b"},
		{"!FOO=hi\necho $FOO", "hi\n"},
		{"!! false", ""},
	} {
		script, ok := usercmd.Parse(tc.typed)
		if !ok {
			t.Errorf("%q is not a command", tc.typed)
			continue
		}
		res := r.Run(context.Background(), script)
		if res.State != usercmd.StateExited || res.ExitCode != 0 || !strings.Contains(res.Output, tc.want) {
			t.Errorf("%q: %+v", tc.typed, res)
		}
	}
}

// foci-call is the binary the generated foci_* functions call; a "!" command finds it
// on the operator PATH.
func buildFociCall(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out, err := exec.Command("go", "build", "-buildvcs=false", "-o", filepath.Join(dir, "foci-call"), "foci/cmd/foci-call").CombinedOutput()
	if err != nil {
		t.Fatalf("build foci-call: %v\n%s", err, out)
	}
	return dir
}

// #2882: the foci_* functions must exist in the shell a "!" command gets, and reach
// the tool. This drives the real path: UserRunner, generated function, foci-call,
// bridge, tool. The answer of Runnable matches: an available tool's function counts,
// a tool with a closed live gate or no export does not, nor does any name with no
// registry.
func TestUserRunner_FociFunctionsRunAndCount(t *testing.T) {
	t.Setenv("PATH", buildFociCall(t)+":"+os.Getenv("PATH"))
	registry := tools.NewRegistry()
	echoTool := func(name string) *tools.Tool {
		return &tools.Tool{
			Name:        name,
			Description: "Test tool.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			ExecExport:  true,
			Execute: func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Text: "reached " + name + " in " + tools.SessionKeyFromContext(ctx)}, nil
			},
		}
	}
	registry.Register(echoTool("probe2882"))
	closed := echoTool("closed2882")
	closed.Available = func() bool { return false }
	registry.Register(closed)
	hidden := echoTool("hidden2882")
	hidden.ExecExport = false
	registry.Register(hidden)

	r := userRunner(t, 15*time.Second, 1<<20)
	r.Registry = registry
	ctx := tools.WithSessionKey(context.Background(), "agent/c1")
	res := r.Run(ctx, "# a note\nFOO=1 foci_probe2882")
	if res.State != usercmd.StateExited || res.ExitCode != 0 || !strings.Contains(res.Output, "reached probe2882 in agent/c1") {
		t.Fatalf("foci_probe2882 did not reach its tool: %+v", res)
	}
	for word, want := range map[string]bool{
		"foci_probe2882":  true,
		"foci_closed2882": false,
		"foci_hidden2882": false,
		"foci_nope2882":   false,
	} {
		if got := r.Runnable(word); got != want {
			t.Errorf("Runnable(%q) = %v, want %v", word, got, want)
		}
	}
	r.Registry = nil
	if r.Runnable("foci_probe2882") {
		t.Error("with no registry there is no function")
	}
}

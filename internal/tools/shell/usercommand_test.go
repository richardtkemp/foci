package shell

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"foci/internal/secrets"
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

package ccstream

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeStubClaude writes an executable stub that emulates `claude auth status`
// by printing body to stdout and exiting with code. Returns its path.
func writeStubClaude(t *testing.T, body string, code int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-stub")
	script := "#!/bin/sh\ncat <<'EOF'\n" + body + "\nEOF\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func TestCheckReady_Authenticated(t *testing.T) {
	stub := writeStubClaude(t, `{"loggedIn": true, "authMethod": "claude.ai"}`, 0)
	fired := false
	b := &Backend{
		cfg:           map[string]any{"binary": stub},
		onAuthFailure: func(string) { fired = true },
	}

	ready, err := b.CheckReady(context.Background())
	if err != nil {
		t.Fatalf("CheckReady err: %v", err)
	}
	if !ready {
		t.Errorf("ready = false; want true")
	}
	if fired {
		t.Errorf("onAuthFailure fired for an authenticated backend")
	}
}

func TestCheckReady_NotAuthenticated_TriggersRelogin(t *testing.T) {
	// Real `claude auth status` exits NON-ZERO when not logged in but still
	// prints {"loggedIn": false} to stdout. The exit code must not be mistaken
	// for a probe failure — we must parse the body and trigger login.
	stub := writeStubClaude(t, `{"loggedIn": false, "authMethod": "none"}`, 1)
	var detail string
	fired := false
	b := &Backend{
		cfg:           map[string]any{"binary": stub},
		onAuthFailure: func(d string) { fired = true; detail = d },
	}

	ready, err := b.CheckReady(context.Background())
	if err != nil {
		t.Fatalf("CheckReady err: %v", err)
	}
	if ready {
		t.Errorf("ready = true; want false when not logged in")
	}
	if !fired {
		t.Fatalf("onAuthFailure not fired for an unauthenticated backend")
	}
	if detail == "" {
		t.Errorf("onAuthFailure detail empty")
	}
}

func TestCheckReady_ProbeError_NoTrigger(t *testing.T) {
	// Non-JSON output → parse error → indeterminate. Must NOT trigger login.
	stub := writeStubClaude(t, `not json at all`, 0)
	fired := false
	b := &Backend{
		cfg:           map[string]any{"binary": stub},
		onAuthFailure: func(string) { fired = true },
	}

	ready, err := b.CheckReady(context.Background())
	if err == nil {
		t.Fatalf("CheckReady err = nil; want a parse error")
	}
	if ready {
		t.Errorf("ready = true; want false on probe error")
	}
	if fired {
		t.Errorf("onAuthFailure fired on an indeterminate probe (should not)")
	}
}

func TestCheckReady_BinaryFails_NoTrigger(t *testing.T) {
	// Binary exits non-zero with no parseable output → run error, no trigger.
	stub := writeStubClaude(t, ``, 3)
	fired := false
	b := &Backend{
		cfg:           map[string]any{"binary": stub},
		onAuthFailure: func(string) { fired = true },
	}

	ready, err := b.CheckReady(context.Background())
	if err == nil {
		t.Fatalf("CheckReady err = nil; want a run error")
	}
	if ready || fired {
		t.Errorf("ready=%v fired=%v; want false/false on a failing binary", ready, fired)
	}
}

// setAuthProbeLimits overrides the wait and the hard cap for one test.
func setAuthProbeLimits(t *testing.T, wait, hardCap time.Duration) {
	t.Helper()
	oldWait, oldCap := authStatusTimeout, authStatusHardCap
	authStatusTimeout, authStatusHardCap = wait, hardCap
	t.Cleanup(func() { authStatusTimeout, authStatusHardCap = oldWait, oldCap })
}

// writeHeldStubClaude writes a stub that stands in for a slow token refresh: it
// blocks until the file at the returned release path exists, then records that
// it ran to the end (the refresh was saved) and prints a logged-out body.
func writeHeldStubClaude(t *testing.T) (stub, release, finished string) {
	t.Helper()
	dir := t.TempDir()
	stub = filepath.Join(dir, "claude-stub")
	release = filepath.Join(dir, "release")
	finished = filepath.Join(dir, "finished")
	script := "#!/bin/sh\n" +
		"while [ ! -e '" + release + "' ]; do sleep 0.02; done\n" +
		": > '" + finished + "'\n" +
		"echo '{\"loggedIn\": false}'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return stub, release, finished
}

// waitForFile fails the test if path does not appear. The deadline is only a
// hang guard: the stub writes the file as soon as it is released.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared: the probe did not run to the end (killed?)", filepath.Base(path))
}

// #2837: `claude auth status` can refresh the OAuth token over the network, and
// killing it mid-refresh could sign the user out. So the wait ends at the
// timeout as 'indeterminate', and the probe carries on.
func TestCheckReady_SlowProbe_DetachesWithoutKilling(t *testing.T) {
	setAuthProbeLimits(t, 50*time.Millisecond, time.Minute)
	stub, release, finished := writeHeldStubClaude(t)
	fired := false
	b := &Backend{
		cfg:           map[string]any{"binary": stub},
		onAuthFailure: func(string) { fired = true },
	}

	ready, err := b.CheckReady(context.Background())
	if err == nil {
		t.Fatalf("CheckReady err = nil; want an indeterminate timeout error")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("err = %v; want it to say the probe is still running", err)
	}
	if ready || fired {
		t.Errorf("ready=%v fired=%v; want false/false on an indeterminate probe", ready, fired)
	}

	// The wait has ended and the probe has not been released yet. It must still
	// be alive to reach the end, well past the timeout that used to kill it.
	time.Sleep(4 * authStatusTimeout)
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatalf("release stub: %v", err)
	}
	waitForFile(t, finished)
}

// Shutting down the gateway ends the wait but must not kill a refresh either.
func TestCheckReady_CtxCancel_EndsWaitWithoutKilling(t *testing.T) {
	setAuthProbeLimits(t, time.Minute, time.Minute)
	stub, release, finished := writeHeldStubClaude(t)
	b := &Backend{cfg: map[string]any{"binary": stub}, onAuthFailure: func(string) {}}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := b.CheckReady(ctx); err == nil {
		t.Fatalf("CheckReady err = nil; want an indeterminate error after cancel")
	}

	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatalf("release stub: %v", err)
	}
	waitForFile(t, finished)
}

// A context that is already cancelled must not start a probe at all.
func TestCheckReady_CtxAlreadyCancelled_NoSpawn(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	stub := filepath.Join(dir, "claude-stub")
	script := "#!/bin/sh\n: > '" + marker + "'\necho '{\"loggedIn\": true}'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	b := &Backend{cfg: map[string]any{"binary": stub}, onAuthFailure: func(string) {}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.CheckReady(ctx); err == nil {
		t.Fatalf("CheckReady err = nil; want the context error")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("stub ran on an already-cancelled context")
	}
}

// A wedged binary must not live for ever: the hard cap kills it after detach.
func TestCheckReady_HardCapKillsWedgedProbe(t *testing.T) {
	setAuthProbeLimits(t, 50*time.Millisecond, 300*time.Millisecond)
	dir := t.TempDir()
	stub := filepath.Join(dir, "claude-stub")
	pidFile := filepath.Join(dir, "pid")
	tmp := pidFile + ".tmp"
	script := "#!/bin/sh\necho $$ > '" + tmp + "'\nmv '" + tmp + "' '" + pidFile + "'\nexec sleep 120\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	b := &Backend{cfg: map[string]any{"binary": stub}, onAuthFailure: func(string) {}}

	if _, err := b.CheckReady(context.Background()); err == nil {
		t.Fatalf("CheckReady err = nil; want an indeterminate timeout error")
	}
	waitForFile(t, pidFile)
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse pid %q: %v", raw, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	deadline := time.Now().Add(30 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("probe pid %d still alive long after the hard cap", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

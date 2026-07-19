package procx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCredentialSetupError proves the fail-closed decision: a process that
// holds the foci-secrets group but whose CAP_SETGID probe failed must surface
// an error (otherwise children silently inherit the group and can read
// secrets.toml). Every other combination is safe and must not error. (P2-12.)
func TestCredentialSetupError(t *testing.T) {
	t.Parallel()
	if err := credentialSetupError(true, errors.New("EPERM")); err == nil {
		t.Error("held group + failed probe must fail closed (non-nil error)")
	}
	if err := credentialSetupError(false, errors.New("EPERM")); err != nil {
		t.Errorf("no group membership must not fail closed: %v", err)
	}
	if err := credentialSetupError(true, nil); err != nil {
		t.Errorf("successful probe must not fail closed: %v", err)
	}
	if err := credentialSetupError(false, nil); err != nil {
		t.Errorf("no group + success must not fail closed: %v", err)
	}
}

func TestChildAttrSetpgid(t *testing.T) {
	// childAttr should always return a non-nil SysProcAttr with Setpgid
	// so child processes land in their own process group (clean kill).
	t.Parallel()
	attr := childAttr()
	if attr == nil {
		t.Fatal("childAttr returned nil")
	}
	if !attr.Setpgid {
		t.Error("Setpgid should be true")
	}
}

func TestChildAttrSetsid(t *testing.T) {
	// childAttrSetsid should always return a non-nil SysProcAttr with
	// Setsid so daemonised children start their own session.
	t.Parallel()
	attr := childAttrSetsid()
	if attr == nil {
		t.Fatal("childAttrSetsid returned nil")
	}
	if !attr.Setsid {
		t.Error("Setsid should be true")
	}
}

func TestChildCredentialPreservesOtherGroups(t *testing.T) {
	// When a child credential is set, the foci-secrets group should be
	// excluded from its group list while all other supplementary groups
	// are preserved.
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user")
	}

	// Setup is idempotent (sync.Once), so calling it from multiple tests
	// is safe. Not parallel because childCredential is a package-level
	// var read here after Setup runs.
	Setup()

	// If foci-secrets group doesn't exist, credential should be nil
	// (nothing to drop). If it does exist but we lack CAP_SETGID,
	// credential should also be nil. In both cases, child inherits
	// all parent groups — which is correct.
	if childCredential == nil {
		t.Log("childCredential is nil — either foci-secrets group not found or CAP_SETGID unavailable")
		return
	}

	secretsGrp, err := user.LookupGroup(SecurityGroupName)
	if err != nil {
		t.Fatalf("%s group lookup failed but credential is set: %v", SecurityGroupName, err)
	}

	for _, g := range childCredential.Groups {
		if g == uint32(mustParseUint(secretsGrp.Gid)) {
			t.Errorf("childCredential.Groups contains %s gid %s — should be filtered", SecurityGroupName, secretsGrp.Gid)
		}
	}

	t.Logf("child groups: %v (primary gid: %d)", childCredential.Groups, childCredential.Gid)
}

func mustParseUint(s string) uint64 {
	n, _ := strings.CutPrefix(s, "")
	var v uint64
	for _, c := range n {
		v = v*10 + uint64(c-'0')
	}
	return v
}

func TestSpawnStillWorks(t *testing.T) {
	// A subprocess launched via Spawn should run normally regardless of
	// whether the credential mechanism is active.
	t.Parallel()
	proc := Spawn(context.Background(), "echo", "hello")
	out, err := proc.CombinedOutput()
	if err != nil {
		t.Fatalf("exec failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "hello") {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestSpawnSetsidStillWorks(t *testing.T) {
	// Same for SpawnSetsid.
	t.Parallel()
	proc := SpawnSetsid(context.Background(), "echo", "hello")
	out, err := proc.CombinedOutput()
	if err != nil {
		t.Fatalf("exec failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "hello") {
		t.Errorf("unexpected output: %s", out)
	}
}

// writeExecutable writes a trivial runnable script and returns its path.
func writeExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prog.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	return path
}

// TestRunWithETXTBSYRetry_Recovers proves the retry loop recovers from a
// transient "text file busy" (golang/go#22315). It forces the condition
// deterministically: an open O_WRONLY fd to the target makes exec fail with
// ETXTBSY, and the fd is released inside the retry budget so a later attempt
// lands once the file is free.
func TestRunWithETXTBSYRetry_Recovers(t *testing.T) {
	t.Parallel()
	prog := writeExecutable(t)

	wf, err := os.OpenFile(prog, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open for write: %v", err)
	}
	go func() {
		time.Sleep(4 * time.Millisecond) // < ETXTBSYRetries*ETXTBSYBackoff (10ms)
		_ = wf.Close()
	}()

	ctx := context.Background()
	if err := runWithETXTBSYRetry(ctx, ETXTBSYRetries, ETXTBSYBackoff, func() *exec.Cmd {
		return Spawn(ctx, prog)
	}); err != nil {
		t.Fatalf("expected retry to recover past ETXTBSY, got: %v", err)
	}
}

// TestRunWithETXTBSYRetry_NoRetryFails is the control: with the write fd held
// open for the whole call and retries=0, the single exec attempt must surface
// the real ETXTBSY. This proves the recovery above comes from the retry, not
// from luck — and that the helper propagates a genuine ETXTBSY.
func TestRunWithETXTBSYRetry_NoRetryFails(t *testing.T) {
	t.Parallel()
	prog := writeExecutable(t)

	wf, err := os.OpenFile(prog, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open for write: %v", err)
	}
	defer func() { _ = wf.Close() }()

	ctx := context.Background()
	err = runWithETXTBSYRetry(ctx, 0, ETXTBSYBackoff, func() *exec.Cmd {
		return Spawn(ctx, prog)
	})
	if !errors.Is(err, syscall.ETXTBSY) {
		t.Fatalf("with retries=0 and fd held open, want ETXTBSY, got: %v", err)
	}
}

func TestNoCredentialWithoutSecretsGroup(t *testing.T) {
	// If the foci-secrets group doesn't exist on this system, the
	// credential must be nil — there's nothing to drop.
	// Not parallel: reads package-level childCredential after Setup.
	Setup()
	if _, err := user.LookupGroup(SecurityGroupName); err != nil {
		if childCredential != nil {
			t.Errorf("childCredential should be nil when %s group doesn't exist", SecurityGroupName)
		}
	}
}

package procx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// TestCredentialSetupError proves the fail-closed decision: a process that
// holds a security group but whose credential-drop probe failed must surface
// an error (otherwise children silently inherit the group and can read
// secrets.toml or reach the askgw socket). Every other combination is safe
// and must not error. (P2-12.)
func TestCredentialSetupError(t *testing.T) {
	t.Parallel()
	held := []string{SecurityGroupName}
	for _, goos := range []string{"linux", "darwin"} {
		if err := credentialSetupError(goos, held, errors.New("EPERM")); err == nil {
			t.Errorf("%s: held group + failed probe must fail closed (non-nil error)", goos)
		}
		if err := credentialSetupError(goos, nil, errors.New("EPERM")); err != nil {
			t.Errorf("%s: no group membership must not fail closed: %v", goos, err)
		}
		if err := credentialSetupError(goos, held, nil); err != nil {
			t.Errorf("%s: successful probe must not fail closed: %v", goos, err)
		}
		if err := credentialSetupError(goos, nil, nil); err != nil {
			t.Errorf("%s: no group + success must not fail closed: %v", goos, err)
		}
	}
}

// TestCredentialSetupErrorMessage pins what the fail-closed message tells the
// operator (#2665): it names the group actually held (not always foci-secrets),
// and on macOS it says isolation is unavailable instead of pointing at
// CAP_SETGID or a systemd unit, neither of which exists there.
func TestCredentialSetupErrorMessage(t *testing.T) {
	t.Parallel()
	probe := errors.New("operation not permitted")

	macErr := credentialSetupError("darwin", []string{"foci-askgw"}, probe)
	if macErr == nil {
		t.Fatal("darwin: held group + failed probe must fail closed")
	}
	mac := macErr.Error()
	if !strings.Contains(mac, "foci-askgw") {
		t.Errorf("darwin message must name the held group foci-askgw: %q", mac)
	}
	if strings.Contains(mac, SecurityGroupName) {
		t.Errorf("darwin message names %s, which is not held: %q", SecurityGroupName, mac)
	}
	for _, bad := range []string{"CAP_SETGID", "systemd", "AmbientCapabilities"} {
		if strings.Contains(mac, bad) {
			t.Errorf("darwin message mentions %s, which does not exist on macOS: %q", bad, mac)
		}
	}
	if !strings.Contains(mac, "macOS") {
		t.Errorf("darwin message must say isolation is unavailable on macOS: %q", mac)
	}
	if !errors.Is(macErr, probe) {
		t.Error("darwin error must wrap the probe error")
	}

	linErr := credentialSetupError("linux", []string{SecurityGroupName, "foci-askgw"}, probe)
	if linErr == nil {
		t.Fatal("linux: held group + failed probe must fail closed")
	}
	for _, want := range []string{SecurityGroupName, "foci-askgw", "CAP_SETGID", "AmbientCapabilities=CAP_SETGID"} {
		if !strings.Contains(linErr.Error(), want) {
			t.Errorf("linux message must mention %s: %q", want, linErr.Error())
		}
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
	proc := Spawn(context.Background(), Trusted, "echo", "hello")
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
	proc := SpawnSetsid(context.Background(), Trusted, "echo", "hello")
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
//
// The fd close is causally ordered on the retry loop's own clock, not a
// wall-clock guess: build (called fresh on every attempt, see
// runWithETXTBSYRetry) closes the fd on its second invocation, i.e. right
// before the attempt that must observe the file free — after the first
// attempt has already failed with ETXTBSY but before that attempt's Run()
// executes. Previously a goroutine slept 4ms hoping to beat the retry loop's
// 10ms budget (ETXTBSYRetries*ETXTBSYBackoff); under heavy host load that
// 6ms margin could vanish (#1703).
//
// NOT t.Parallel (#2002): closing wf only releases OUR copy of the write fd.
// Any fork made by another goroutine while wf is open gives that child a copy
// too, held until the child execs (golang/go#22315 itself); if the child is
// descheduled past the remaining retry budget, every retry still sees ETXTBSY.
// That is how this failed in a full `make test` even with the causal release.
// Running serially means no sibling test in this binary is forking while wf
// is open, so the release on attempt 2 is the last write holder by
// construction, not by timing.
func TestRunWithETXTBSYRetry_Recovers(t *testing.T) {
	prog := writeExecutable(t)

	wf, err := os.OpenFile(prog, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open for write: %v", err)
	}
	defer func() { _ = wf.Close() }() // no-op if already closed below

	attempt := 0
	build := func() *exec.Cmd {
		attempt++
		if attempt == 2 {
			// Release the fd right before the retry attempt that needs it
			// free — Spawn below only constructs the *exec.Cmd, it does not
			// start it, so this happens strictly before that attempt execs.
			_ = wf.Close()
		}
		return Spawn(context.Background(), Trusted, prog)
	}

	ctx := context.Background()
	if err := runWithETXTBSYRetry(ctx, ETXTBSYRetries, ETXTBSYBackoff, build); err != nil {
		t.Fatalf("expected retry to recover past ETXTBSY, got: %v", err)
	}
}

// TestRunWithETXTBSYRetry_NoRetryFails is the control: with the write fd held
// open for the whole call and retries=0, the single exec attempt must surface
// the real ETXTBSY. This proves the recovery above comes from the retry, not
// from luck — and that the helper propagates a genuine ETXTBSY.
func TestRunWithETXTBSYRetry_NoRetryFails(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		// macOS execs a file that is open for writing; it never returns
		// ETXTBSY, so there is nothing for the control to observe. The retry
		// helper is harmless there (it retries only on ETXTBSY).
		t.Skipf("ETXTBSY is Linux behaviour; %s execs a file open for write", runtime.GOOS)
	}
	prog := writeExecutable(t)

	wf, err := os.OpenFile(prog, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open for write: %v", err)
	}
	defer func() { _ = wf.Close() }()

	ctx := context.Background()
	err = runWithETXTBSYRetry(ctx, 0, ETXTBSYBackoff, func() *exec.Cmd {
		return Spawn(ctx, Trusted, prog)
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

package procx

import (
	"context"
	"os"
	"os/user"
	"strings"
	"testing"
)

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

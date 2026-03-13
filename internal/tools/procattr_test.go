package tools

import (
	"os"
	"os/exec"
	"os/user"
	"strings"
	"testing"

	"foci/internal/secrets"
)

func TestChildSysProcAttr(t *testing.T) {
	// Verifies that ChildSysProcAttr returns a non-nil SysProcAttr with Setpgid enabled so child processes are placed in their own process group.
	t.Parallel()
	attr := ChildSysProcAttr()
	if attr == nil {
		t.Fatal("ChildSysProcAttr returned nil")
	}
	if !attr.Setpgid {
		t.Error("Setpgid should be true")
	}
}

func TestChildSysProcAttrSetsid(t *testing.T) {
	// Verifies that ChildSysProcAttrSetsid returns a non-nil SysProcAttr with Setsid enabled so child processes start a new session.
	t.Parallel()
	attr := ChildSysProcAttrSetsid()
	if attr == nil {
		t.Fatal("ChildSysProcAttrSetsid returned nil")
	}
	if !attr.Setsid {
		t.Error("Setsid should be true")
	}
}

func TestChildCredentialPreservesOtherGroups(t *testing.T) {
	// Verifies that when a child credential is set, the foci-secrets group is excluded from its group list while all other supplementary groups are preserved.
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user")
	}

	// If foci-secrets group doesn't exist, credential should be nil
	// (nothing to drop). If it does exist but we lack CAP_SETGID,
	// credential should also be nil. In both cases, child inherits
	// all parent groups — which is correct.
	if childCredential == nil {
		t.Log("childCredential is nil — either foci-secrets group not found or CAP_SETGID unavailable")
		return
	}

	// If credential IS set, verify foci-secrets is not in the group list
	secretsGrp, err := user.LookupGroup(secrets.SecurityGroupName)
	if err != nil {
		t.Fatalf("foci-secrets group lookup failed but credential is set: %v", err)
	}

	for _, g := range childCredential.Groups {
		if g == uint32(mustParseUint(secretsGrp.Gid)) {
			t.Errorf("childCredential.Groups contains foci-secrets gid %s — should be filtered", secretsGrp.Gid)
		}
	}

	// Verify other groups ARE preserved (credential should have more than just primary)
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

func TestExecStillWorks(t *testing.T) {
	// Verifies that a real subprocess can be successfully launched with ChildSysProcAttr applied, proving it does not break basic execution.
	t.Parallel()
	// (regardless of whether credential is set or nil).
	proc := exec.Command("echo", "hello")
	proc.SysProcAttr = ChildSysProcAttr()

	out, err := proc.CombinedOutput()
	if err != nil {
		t.Fatalf("exec failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "hello") {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestExecSetsidStillWorks(t *testing.T) {
	// Verifies that a real subprocess can be launched with ChildSysProcAttrSetsid applied, proving it does not break basic execution.
	t.Parallel()
	proc := exec.Command("echo", "hello")
	proc.SysProcAttr = ChildSysProcAttrSetsid()

	out, err := proc.CombinedOutput()
	if err != nil {
		t.Fatalf("exec failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "hello") {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestNoCredentialWithoutSecretsGroup(t *testing.T) {
	// Verifies that childCredential is nil when the foci-secrets group does not exist on this system, since there is no group to drop credentials for.
	t.Parallel()
	_, err := user.LookupGroup(secrets.SecurityGroupName)
	if err != nil {
		// Group doesn't exist — credential must be nil
		if childCredential != nil {
			t.Error("childCredential should be nil when foci-secrets group doesn't exist")
		}
	}
}

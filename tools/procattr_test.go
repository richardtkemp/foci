package tools

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestChildSysProcAttr(t *testing.T) {
	attr := ChildSysProcAttr()
	if attr == nil {
		t.Fatal("ChildSysProcAttr returned nil")
	}
	if !attr.Setpgid {
		t.Error("Setpgid should be true")
	}
}

func TestChildSysProcAttrSetsid(t *testing.T) {
	attr := ChildSysProcAttrSetsid()
	if attr == nil {
		t.Fatal("ChildSysProcAttrSetsid returned nil")
	}
	if !attr.Setsid {
		t.Error("Setsid should be true")
	}
}

func TestChildCredentialProbe(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user")
	}

	// In test environment (no CAP_SETGID), childCredential should be nil
	// because the probe fails. In production with CAP_SETGID, it would be set.
	if childCredential != nil {
		// If we do have it (e.g. running with capabilities), verify it's correct
		if childCredential.Uid != uint32(os.Getuid()) {
			t.Errorf("Uid = %d, want %d", childCredential.Uid, os.Getuid())
		}
		if childCredential.Gid != uint32(os.Getgid()) {
			t.Errorf("Gid = %d, want %d", childCredential.Gid, os.Getgid())
		}
		if len(childCredential.Groups) != 1 || childCredential.Groups[0] != uint32(os.Getgid()) {
			t.Errorf("Groups = %v, want [%d]", childCredential.Groups, os.Getgid())
		}
	} else {
		t.Log("childCredential is nil (no CAP_SETGID) — expected in test environment")
	}
}

func TestExecStillWorks(t *testing.T) {
	// Verify that exec commands still work with the SysProcAttr
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

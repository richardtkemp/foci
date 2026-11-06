package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/secrets"
	"foci/internal/usercmd"
)

// #2882: the question the run button rests on. A "!" command of a delegated agent,
// built by the production wiring (buildExecRegistry, setupUserShell), has the foci_*
// functions, reaches the tool, and the button's answer agrees with that.
func TestUserCommand_FociFunctionRunsOnProductionWiring(t *testing.T) {
	binDir := t.TempDir()
	out, err := exec.Command("go", "build", "-buildvcs=false", "-o", filepath.Join(binDir, "foci-call"), "foci/cmd/foci-call").CombinedOutput()
	if err != nil {
		t.Fatalf("build foci-call: %v\n%s", err, out)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	p := minimalSetupParams(t, "agent2882")
	p.acfg.Workspace = t.TempDir()
	secretsPath := filepath.Join(t.TempDir(), "secrets.toml")
	if err := os.WriteFile(secretsPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p.store, err = secrets.Load(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	ag := &agent.Agent{}
	registry := buildExecRegistry(p, stubWakeFn, nil, func() *agent.Agent { return ag })
	setupUserShell(ag, p, registry, 20*time.Second)

	if !ag.UserCommandRunnable("foci_whoami") {
		t.Error("foci_whoami gets no button")
	}
	if ag.UserCommandRunnable("foci_no_such_tool") {
		t.Error("an unknown foci_ word gets a button")
	}
	res := ag.RunUserCommand(context.Background(), "agent2882/c7", "# who\nFOO=1 foci_whoami", nil)
	if res.State != usercmd.StateExited || res.ExitCode != 0 || !strings.Contains(res.Output, "agent2882/c7") {
		t.Fatalf("foci_whoami did not run in the session: %+v", res)
	}
}

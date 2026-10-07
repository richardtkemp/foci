package main

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"foci/internal/delegator/autoapprove"
	"foci/internal/tools"
)

// TestBuildExecRegistryExportsWhoami pins the whoami tool's presence on the
// exec path: it must register (as foci_whoami, hence auto-approved via
// FociShellRulesFor) and be EXECUTABLE through the real wiring even when the
// lazy agent getter is a literal nil and sessionIndex is nil — the
// minimalSetupParams shape — reporting the missing lookups as none/unknown
// instead of panicking (#1135).
func TestBuildExecRegistryExportsWhoami(t *testing.T) {
	t.Parallel()

	p := minimalSetupParams(t, "test")
	registry := buildExecRegistry(p, stubWakeFn, nil, nil)

	found := false
	for _, n := range registry.ExportedNames() {
		if n == "foci_whoami" {
			found = true
		}
	}
	if !found {
		t.Errorf("ExportedNames() = %v, want to include foci_whoami", registry.ExportedNames())
	}

	tool := registry.Get("whoami")
	if tool == nil {
		t.Fatal("registry missing whoami tool")
	}
	res, err := tool.Execute(tools.WithSessionKey(context.Background(), "test/c123"), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("whoami through nil-dep wiring panicked or errored: %v", err)
	}
	want := "agent_id: test\n" +
		"session_key: test/c123\n" +
		"chat_id: 123\n" +
		"platform: unknown\n" + // nil sessionIndex
		"backend: api\n" + // empty configured backend
		"transport: api\n" + // IsDelegated() false
		"model: unknown\n" // nil agLazy
	if res.Text != want {
		t.Errorf("whoami =\n%s\nwant\n%s", res.Text, want)
	}
}

// TestBuildExecRegistryWhoamiAutoApproved proves the #2223 regression lock:
// because the exec registry exports foci_whoami, FociShellRulesFor derives a
// Bash:foci_whoami auto-approve rule for it, and the compiled rules approve
// the bare `foci_whoami` Bash command through MatchWithEnv — the command a
// delegated backend would ask about. Characterisation: this already holds on
// the current code; the test pins it so removing the export (or the rule
// derivation) cannot pass silently.
func TestBuildExecRegistryWhoamiAutoApproved(t *testing.T) {
	t.Parallel()

	registry := buildExecRegistry(minimalSetupParams(t, "test"), stubWakeFn, nil, nil)
	rules := autoapprove.FociShellRulesFor(registry.ExportedNames())
	if !slices.Contains(rules, "Bash:foci_whoami") {
		t.Errorf("FociShellRulesFor(ExportedNames()) = %v, want to contain Bash:foci_whoami", rules)
	}

	ok, veto := autoapprove.MatchWithEnv(autoapprove.Compile(rules), "Bash",
		json.RawMessage(`{"command":"foci_whoami"}`), nil)
	if !ok {
		t.Errorf("compiled foci rules do not auto-approve the Bash command foci_whoami (veto: %s)", veto)
	}
}

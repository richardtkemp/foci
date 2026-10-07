package main

import (
	"context"
	"encoding/json"
	"testing"

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

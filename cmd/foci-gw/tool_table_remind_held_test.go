package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/memory"
)

// TestRemindToolTable_WiresSessionHeld pins the production wiring (#2233): the
// tool table's remind row passes WithSessionHeld, so a wake listed through the
// REAL delegated registration reads "fired, held behind background work"
// exactly while the agent's SystemInputHeld says so for the wake's session —
// the predicate is consulted per call, not captured at build time. An unwired
// predicate compiles and runs fine but silently degrades every held wake to
// "waiting to run", which is what this guards against.
func TestRemindToolTable_WiresSessionHeld(t *testing.T) {
	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })

	p := minimalSetupParams(t, "test")
	p.reminderStore = rs

	ag := &agent.Agent{}
	const sk = "test/main"
	ag.SetInFlightDeliveringForTest(sk, true) // an adopted delivering run holds the session
	t.Cleanup(func() { ag.SetInFlightDeliveringForTest(sk, false) })

	registry := buildExecRegistry(p, stubWakeFn, nil, func() *agent.Agent { return ag })
	tool := registry.Get("remind")
	if tool == nil {
		t.Fatal("remind tool not registered with reminderStore + wakeFn set")
	}

	id, err := rs.AddWake("test", sk, "check the build", "1h")
	if err != nil {
		t.Fatalf("AddWake: %v", err)
	}
	rs.MarkWakeFired(id, sk)

	res, err := tool.Execute(context.Background(), json.RawMessage(`{"list":true}`))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(res.Text, "fired, held behind background work") {
		t.Fatalf("list = %q, want the held wording — the table's WithSessionHeld predicate is unwired or never consulted", res.Text)
	}

	// The hold clears: the same fired wake now reads "waiting to run".
	ag.SetInFlightDeliveringForTest(sk, false)
	res, err = tool.Execute(context.Background(), json.RawMessage(`{"list":true}`))
	if err != nil {
		t.Fatalf("list after clear: %v", err)
	}
	if !strings.Contains(res.Text, "fired, waiting to run") {
		t.Fatalf("list = %q, want the waiting wording once the session is no longer held", res.Text)
	}
}

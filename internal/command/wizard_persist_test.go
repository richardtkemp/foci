package command

import (
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/session"
)

func newTestIndex(t *testing.T) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

// The core restart story: a mid-flow agentWizard is checkpointed on every
// mutation, and a FRESH registry (a new process) restores it at the same step
// with all collected values — the next answer continues the flow seamlessly.
func TestWizardPersistence_SurvivesRestart(t *testing.T) {
	idx := newTestIndex(t)
	deps := testDeps([]AgentInfo{{ID: "existing"}}, nil)
	cc := CommandContext{AgentNewDeps: &deps}

	r1 := NewRegistry()
	r1.EnableWizardPersistence(idx, "ag")
	w := newAgentWizard(deps)
	r1.SetWizard("ag/conv-1", w)

	// Advance two steps: name, then backend.
	if _, _, ok := r1.HandleMessage("ag/conv-1", "Greek Tutor"); !ok {
		t.Fatal("name step should be handled")
	}
	if _, _, ok := r1.HandleMessage("ag/conv-1", "claude-code"); !ok {
		t.Fatal("backend step should be handled")
	}

	// "Restart": a fresh registry restores from the same index.
	r2 := NewRegistry()
	r2.EnableWizardPersistence(idx, "ag")
	r2.RestoreWizards(cc)

	if !r2.WizardActive("ag/conv-1") {
		t.Fatal("wizard should be active after restore")
	}
	if r2.WizardGen("ag/conv-1") == 0 {
		t.Error("restored wizard must have a non-zero gen")
	}
	// The restored wizard is at the model step with the collected values intact:
	// answering model + charMode completes creation with the pre-restart name.
	var captured *agentWizard
	r2.wizardMu.Lock()
	rw := r2.wizards["ag/conv-1"].handler.(*agentWizard)
	r2.wizardMu.Unlock()
	rw.createFn = func(wiz *agentWizard) (string, error) { captured = wiz; return "Created!", nil }

	if resp, _, _ := r2.HandleMessage("ag/conv-1", "opus"); !strings.Contains(resp, "Character files") {
		t.Fatalf("model answer should reach the charMode step, got %q", resp)
	}
	resp, _, _ := r2.HandleMessage("ag/conv-1", "defaults")
	if resp != "Created!" {
		t.Fatalf("final step resp = %q", resp)
	}
	if captured == nil || captured.id != "greek-tutor" || captured.backend != "claude-code" {
		t.Errorf("restored wizard lost collected state: %+v", captured)
	}
	if r2.WizardActive("ag/conv-1") {
		t.Error("wizard should clear on completion")
	}
	// Completion also clears the persisted entry — a third restart restores nothing.
	r3 := NewRegistry()
	r3.EnableWizardPersistence(idx, "ag")
	r3.RestoreWizards(cc)
	if r3.WizardActive("ag/conv-1") {
		t.Error("completed wizard must not resurrect")
	}
}

// Cancellation (the /cancel intercept) removes the persisted entry too.
func TestWizardPersistence_CancelClears(t *testing.T) {
	idx := newTestIndex(t)
	deps := testDeps(nil, nil)
	r1 := NewRegistry()
	r1.EnableWizardPersistence(idx, "ag")
	r1.SetWizard("s", newAgentWizard(deps))
	if _, _, ok := r1.HandleMessage("s", "/cancel"); !ok {
		t.Fatal("cancel should be handled")
	}

	r2 := NewRegistry()
	r2.EnableWizardPersistence(idx, "ag")
	r2.RestoreWizards(CommandContext{AgentNewDeps: &deps})
	if r2.WizardActive("s") {
		t.Error("cancelled wizard must not restore")
	}
}

// Wizards persist independently per scope, each restoring with its own
// generation.
func TestWizardPersistence_PerScope(t *testing.T) {
	idx := newTestIndex(t)
	deps := testDeps(nil, nil)
	r1 := NewRegistry()
	r1.EnableWizardPersistence(idx, "ag")
	r1.SetWizard("s1", newAgentWizard(deps))
	r1.SetWizard("s2", newAgentWizard(deps))

	r2 := NewRegistry()
	r2.EnableWizardPersistence(idx, "ag")
	r2.RestoreWizards(CommandContext{AgentNewDeps: &deps})
	if !r2.WizardActive("s1") || !r2.WizardActive("s2") {
		t.Error("both scopes should restore")
	}
	if r2.WizardGen("s1") == r2.WizardGen("s2") {
		t.Error("each restored wizard needs its own generation")
	}
}

// A wizard whose kind can't be rebuilt (deps missing in the restoring process)
// is dropped — and the DROP is durable: restore re-persists the cleaned set
// (same self-heal as the ask tool), so a later restart doesn't resurrect it.
func TestWizardPersistence_MissingDepsDropDurably(t *testing.T) {
	idx := newTestIndex(t)
	deps := testDeps(nil, nil)
	r1 := NewRegistry()
	r1.EnableWizardPersistence(idx, "ag")
	r1.SetWizard("s1", newAgentWizard(deps))

	r2 := NewRegistry()
	r2.EnableWizardPersistence(idx, "ag")
	r2.RestoreWizards(CommandContext{}) // no AgentNewDeps — unrestorable
	if r2.WizardActive("s1") {
		t.Error("wizard without deps must be dropped, not half-restored")
	}

	r3 := NewRegistry()
	r3.EnableWizardPersistence(idx, "ag")
	r3.RestoreWizards(CommandContext{AgentNewDeps: &deps})
	if r3.WizardActive("s1") {
		t.Error("a dropped wizard must not resurrect on the next restart")
	}
}

// The config-set wizard re-derives its func-bearing field/target from deps at
// restore; a field that no longer exists drops the wizard instead of leaving
// it half-initialised.
func TestWizardPersistence_ConfigSetRederivesField(t *testing.T) {
	snapshotThenRestore := func(t *testing.T, lookupOK bool) (*Registry, string) {
		t.Helper()
		idx := newTestIndex(t)
		var capturedValue string
		deps := testConfigSetDeps(func(_ string, _ config.SetTarget, value string) (string, error) {
			capturedValue = value
			return "", nil
		})
		r1 := NewRegistry()
		r1.EnableWizardPersistence(idx, "ag")
		r1.SetWizard("s", newConfigSetWizard(deps))
		if _, _, ok := r1.HandleMessage("s", "agent_loop"); !ok {
			t.Fatal("section step should be handled")
		}
		if resp, _, ok := r1.HandleMessage("s", "max_output_tokens"); !ok || !strings.Contains(resp, "New value") {
			t.Fatalf("key step should reach the value prompt, got %q", resp)
		}

		restoreDeps := deps
		if !lookupOK {
			restoreDeps.LookupFn = func(string) (config.ConfigField, bool) {
				return config.ConfigField{}, false
			}
		}
		r2 := NewRegistry()
		r2.EnableWizardPersistence(idx, "ag")
		r2.RestoreWizards(CommandContext{ConfigSetDeps: &restoreDeps})
		resp, _, _ := r2.HandleMessage("s", "32768")
		_ = capturedValue
		return r2, resp
	}

	r, resp := snapshotThenRestore(t, true)
	if !strings.Contains(resp, "Set agent_loop.max_output_tokens") {
		t.Errorf("restored config-set wizard should complete the set, got %q", resp)
	}
	if r.WizardActive("s") {
		t.Error("wizard should clear on completion")
	}

	r, _ = snapshotThenRestore(t, false)
	if r.WizardActive("s") {
		t.Error("a wizard whose field vanished must be dropped at restore")
	}
}

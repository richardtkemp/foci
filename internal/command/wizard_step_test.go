package command

import (
	"strings"
	"testing"
)

// Verifies the Registry's wizard introspection accessors under scoping:
// WizardActive tracks SetWizard/ClearWizard per scope, WizardGen identifies
// one wizard's lifetime (changes on replacement, 0 when none is active), and
// WizardPendingStep proxies the provider.
func TestRegistryWizardAccessors(t *testing.T) {
	r := NewRegistry()
	if r.WizardActive("s1") {
		t.Error("fresh registry should have no active wizard")
	}
	if r.WizardPendingStep("s1") != nil {
		t.Error("no wizard ⇒ no pending step")
	}
	if r.WizardGen("s1") != 0 {
		t.Error("no wizard ⇒ gen 0")
	}

	w := newAgentWizard(testDeps(nil, nil))
	r.SetWizard("s1", w)
	if !r.WizardActive("s1") {
		t.Error("wizard should be active after SetWizard")
	}
	gen1 := r.WizardGen("s1")
	if gen1 == 0 {
		t.Error("active wizard must have a non-zero gen")
	}

	// Replacement in the same scope mints a fresh generation.
	r.SetWizard("s1", newAgentWizard(testDeps(nil, nil)))
	if g := r.WizardGen("s1"); g == gen1 || g == 0 {
		t.Errorf("gen after replace = %d, want a fresh non-zero gen (was %d)", g, gen1)
	}

	// Clearing (cancel/done) deactivates the scope.
	r.ClearWizard("s1")
	if r.WizardActive("s1") {
		t.Error("wizard should be inactive after ClearWizard")
	}
	if r.WizardGen("s1") != 0 {
		t.Error("cleared scope ⇒ gen 0")
	}
}

// Verifies scope isolation: two sessions run independent wizards concurrently,
// each session's input advances only its own wizard, and clearing one leaves
// the other untouched. (Pre-scoping, a second wizard silently replaced the
// first and any chat could advance it.)
func TestRegistryWizardScopeIsolation(t *testing.T) {
	r := NewRegistry()
	wa := newAgentWizard(testDeps(nil, nil))
	wb := newAgentWizard(testDeps(nil, nil))
	r.SetWizard("agent/telegram-1", wa)
	r.SetWizard("agent/app-2", wb)

	// Input on scope A advances only wizard A.
	if _, _, ok := r.HandleMessage("agent/telegram-1", "Tutor A"); !ok {
		t.Fatal("scope A wizard should handle its message")
	}
	if wa.step != stepBackend {
		t.Errorf("wizard A step = %d, want %d (advanced)", wa.step, stepBackend)
	}
	if wb.step != stepName {
		t.Errorf("wizard B step = %d, want %d (untouched by A's traffic)", wb.step, stepName)
	}

	// A scope with no wizard is not intercepted.
	if _, _, ok := r.HandleMessage("agent/other-3", "hello"); ok {
		t.Error("a scope without a wizard must not intercept")
	}

	// Cancelling scope B leaves A live.
	if resp, _, ok := r.HandleMessage("agent/app-2", "/cancel"); !ok || resp != "Wizard cancelled." {
		t.Errorf("cancel on scope B: resp=%q ok=%v", resp, ok)
	}
	if r.WizardActive("agent/app-2") {
		t.Error("scope B should be cleared")
	}
	if !r.WizardActive("agent/telegram-1") {
		t.Error("scope A must survive scope B's cancel")
	}
}

// Verifies WizardPendingStep proxies through HandleMessage's wizard: a wizard
// without a structured current step yields nil (free-text fallback), and the
// structured steps carry options whose labels are valid Handle inputs.
func TestAgentWizardPendingStep(t *testing.T) {
	deps := testDeps([]AgentInfo{{ID: "existing"}}, nil)
	deps.AvailableBackends = []string{"claude-code", "claude-code-tmux"}
	w := newAgentWizard(deps)
	w.createFn = func(wiz *agentWizard) (string, error) { return "Created!", nil }

	// Step 0 (name) is free text — no structured step.
	if got := w.PendingStep(); got != nil {
		t.Errorf("name step: PendingStep = %+v, want nil", got)
	}

	// Advance to backend: structured, one option per backend + api.
	if _, done := w.Handle("Greek Tutor"); done {
		t.Fatal("name step should not finish the wizard")
	}
	q := w.PendingStep()
	if q == nil {
		t.Fatal("backend step: PendingStep = nil, want structured question")
	}
	if q.Header != "Backend" {
		t.Errorf("backend header = %q", q.Header)
	}
	labels := make([]string, len(q.Options))
	for i, o := range q.Options {
		labels[i] = o.Label
	}
	want := []string{"claude-code", "claude-code-tmux", "api"}
	if strings.Join(labels, ",") != strings.Join(want, ",") {
		t.Errorf("backend labels = %v, want %v", labels, want)
	}
	// Every label must be a valid Handle input (the app path feeds the picked
	// label straight back into Handle).
	if _, done := w.Handle(q.Options[0].Label); done {
		t.Fatal("backend pick should advance, not finish")
	}

	// Model step is free text again.
	if got := w.PendingStep(); got != nil {
		t.Errorf("model step: PendingStep = %+v, want nil", got)
	}
	if _, done := w.Handle("opus"); done {
		t.Fatal("model step should not finish the wizard")
	}

	// Character-files step: structured; picking a label finishes the wizard.
	q = w.PendingStep()
	if q == nil {
		t.Fatal("charMode step: PendingStep = nil, want structured question")
	}
	if q.Header != "Character files" {
		t.Errorf("charMode header = %q", q.Header)
	}
	resp, done := w.Handle(q.Options[0].Label)
	if !done || resp != "Created!" {
		t.Errorf("charMode pick: resp=%q done=%v, want Created!/true", resp, done)
	}
}

// Verifies the pre-flight warning raised at the name step is carried into the
// structured backend question (the app path renders PendingStep, not the plain
// prompt text, so the warning must live in both).
func TestAgentWizardPendingStepPreflight(t *testing.T) {
	deps := testDeps(nil, func(string) []string { return []string{"user missing"} })
	w := newAgentWizard(deps)
	if _, done := w.Handle("Tutor"); done {
		t.Fatal("name step should not finish")
	}
	q := w.PendingStep()
	if q == nil {
		t.Fatal("backend step: PendingStep = nil")
	}
	if !strings.Contains(q.Question, "user missing") {
		t.Errorf("backend question %q missing pre-flight warning", q.Question)
	}
}

// Compile-time check: agentWizard implements the optional provider interface.
var _ WizardStepProvider = (*agentWizard)(nil)

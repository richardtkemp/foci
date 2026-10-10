package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/question"
)

// scriptedStepWizard is a wizard double with ONE structured step: the first
// Handle consumes it (the wizard then sits on a free-text step, like
// androidWizard's confirmEnable → host), so a further press of the same token
// must be rejected. handles records every Handle call so tests can prove
// rejection paths never reached the wizard.
type scriptedStepWizard struct {
	handles  []string
	step     *question.Question // current structured step; Handle moves past it
	response string
	done     bool // Handle's done result
}

func (w *scriptedStepWizard) Handle(text string) (string, bool) {
	w.handles = append(w.handles, text)
	w.step = nil
	return w.response, w.done
}

func (w *scriptedStepWizard) PendingStep() *question.Question { return w.step }

// Compile-time check: the double satisfies the provider interface the choice
// path requires.
var _ WizardStepProvider = (*scriptedStepWizard)(nil)

// newChoiceRegistry activates a scriptedStepWizard asking a canned two-option
// question under the given scope.
func newChoiceRegistry(scope string) (*Registry, *scriptedStepWizard) {
	r := NewRegistry()
	w := &scriptedStepWizard{
		step: &question.Question{
			Header:   "Test",
			Question: "Pick one:",
			Options:  []question.Option{{Label: "alpha"}, {Label: "beta"}},
		},
		response: "moved on",
	}
	r.SetWizard(scope, w)
	return r, w
}

// Verifies WizardStepToken is a deterministic identity of the step: equal
// steps give equal 16-hex-char tokens, and any change to the header, question
// text, option labels, or option ORDER changes the token. The length bound
// keeps chat callback data inside Telegram's 64-byte limit.
func TestWizardStepToken_StableAndSensitive(t *testing.T) {
	base := &question.Question{
		Header:   "H",
		Question: "Q?",
		Options:  []question.Option{{Label: "a"}, {Label: "b"}},
	}
	dup := &question.Question{
		Header:   "H",
		Question: "Q?",
		Options:  []question.Option{{Label: "a"}, {Label: "b"}},
	}
	tok := WizardStepToken(base)
	if tok != WizardStepToken(dup) {
		t.Errorf("equal steps gave different tokens: %s vs %s", tok, WizardStepToken(dup))
	}
	if len(tok) != 16 || strings.Trim(tok, "0123456789abcdef") != "" {
		t.Errorf("token %q is not 16 hex characters", tok)
	}

	variants := map[string]*question.Question{
		"header":    {Header: "H2", Question: "Q?", Options: base.Options},
		"question":  {Header: "H", Question: "Q2?", Options: base.Options},
		"label":     {Header: "H", Question: "Q?", Options: []question.Option{{Label: "a"}, {Label: "c"}}},
		"order":     {Header: "H", Question: "Q?", Options: []question.Option{{Label: "b"}, {Label: "a"}}},
		"optcount":  {Header: "H", Question: "Q?", Options: []question.Option{{Label: "a"}}},
		"nooptions": {Header: "H", Question: "Q?"},
	}
	for name, q := range variants {
		if WizardStepToken(q) == tok {
			t.Errorf("token insensitive to %s change", name)
		}
	}
}

// Verifies a press with the step's own token and a valid option index behaves
// exactly like typing that option's label: Handle receives the label, the
// wizard's reply comes back, and the wizard stays registered (not done).
func TestHandleWizardChoice_AdvancesOnMatchingToken(t *testing.T) {
	r, w := newChoiceRegistry("s")
	tok := WizardStepToken(w.PendingStep())

	resp, _, handled := r.HandleWizardChoice("s", tok, 1)
	if !handled {
		t.Fatal("matching token + valid index must be handled")
	}
	if resp != "moved on" {
		t.Errorf("response = %q, want the wizard's reply", resp)
	}
	if len(w.handles) != 1 || w.handles[0] != "beta" {
		t.Errorf("Handle saw %v, want [beta] (the pressed option's label)", w.handles)
	}
	if !r.WizardActive("s") {
		t.Error("wizard must stay active after a non-done step")
	}
}

// Verifies every rejection path — wrong token, out-of-range index, inactive
// scope, and a second press of an already-answered step's token — returns
// handled == false WITHOUT calling Handle, so nothing can leak into the
// wizard's next step.
func TestHandleWizardChoice_RejectsStaleOrInvalid(t *testing.T) {
	t.Run("wrong token", func(t *testing.T) {
		r, w := newChoiceRegistry("s")
		if _, _, handled := r.HandleWizardChoice("s", "0000000000000000", 0); handled {
			t.Error("wrong token must not be handled")
		}
		if len(w.handles) != 0 {
			t.Errorf("Handle called with %v on a rejected press", w.handles)
		}
		if !r.WizardActive("s") {
			t.Error("rejected press must leave the wizard untouched")
		}
	})

	t.Run("out-of-range index", func(t *testing.T) {
		r, w := newChoiceRegistry("s")
		tok := WizardStepToken(w.PendingStep())
		for _, choice := range []int{2, 7, -2} {
			if _, _, handled := r.HandleWizardChoice("s", tok, choice); handled {
				t.Errorf("choice %d must not be handled", choice)
			}
		}
		if len(w.handles) != 0 {
			t.Errorf("Handle called with %v on rejected indexes", w.handles)
		}
	})

	t.Run("inactive scope", func(t *testing.T) {
		r, w := newChoiceRegistry("s")
		tok := WizardStepToken(w.PendingStep())
		if _, _, handled := r.HandleWizardChoice("other", tok, 0); handled {
			t.Error("a scope without a wizard must not be handled")
		}
		if len(w.handles) != 0 {
			t.Errorf("Handle called with %v from another scope's press", w.handles)
		}
	})

	t.Run("second press after the step advanced", func(t *testing.T) {
		r, w := newChoiceRegistry("s")
		tok := WizardStepToken(w.PendingStep())
		if _, _, handled := r.HandleWizardChoice("s", tok, 0); !handled {
			t.Fatal("first press must be handled")
		}
		if _, _, handled := r.HandleWizardChoice("s", tok, 1); handled {
			t.Error("a second press of the answered step's token must not be handled")
		}
		if len(w.handles) != 1 {
			t.Errorf("Handle saw %v, want exactly one call", w.handles)
		}
	})

	t.Run("wizard without structured steps", func(t *testing.T) {
		r := NewRegistry()
		r.SetWizard("s", plainWizard{})
		if _, _, handled := r.HandleWizardChoice("s", "0123456789abcdef", 0); handled {
			t.Error("a wizard without PendingStep must not be handled")
		}
	})
}

// plainWizard is a WizardHandler with no structured steps.
type plainWizard struct{}

func (plainWizard) Handle(string) (string, bool) { return "", false }

// Verifies the Cancel button (choice -1) takes the wizard's cancel path: the
// cancel reply, the wizard cleared, and Handle never called with a bogus label.
func TestHandleWizardChoice_CancelButtonCancels(t *testing.T) {
	r, w := newChoiceRegistry("s")
	tok := WizardStepToken(w.PendingStep())

	resp, _, handled := r.HandleWizardChoice("s", tok, -1)
	if !handled || resp != "Wizard cancelled." {
		t.Fatalf("cancel press: handled=%v resp=%q, want the cancel reply", handled, resp)
	}
	if r.WizardActive("s") {
		t.Error("cancel press must clear the wizard")
	}
	if len(w.handles) != 0 {
		t.Errorf("Handle saw %v; cancel must not reach Handle", w.handles)
	}
}

// Verifies a press whose answer finishes the wizard (done) clears it from the
// registry, exactly like a typed final answer.
func TestHandleWizardChoice_DoneClearsWizard(t *testing.T) {
	r := NewRegistry()
	w := &scriptedStepWizard{
		step: &question.Question{Question: "Sure?", Options: []question.Option{{Label: "yes"}}},
		done: true,
	}
	r.SetWizard("s", w)

	if _, _, handled := r.HandleWizardChoice("s", WizardStepToken(w.PendingStep()), 0); !handled {
		t.Fatal("press must be handled")
	}
	if r.WizardActive("s") {
		t.Error("wizard must be cleared when the step finishes it")
	}
}

// Verifies the token is derived from the QUESTION alone, not from registry or
// wizard-instance identity: a token computed before a "restart" still answers
// a freshly-built registry's wizard sitting on the same step — which is what
// makes pre-restart buttons work against a restored wizard.
func TestHandleWizardChoice_WorksWithRestoredWizard(t *testing.T) {
	r1, _ := newChoiceRegistry("s")
	preRestartToken := WizardStepToken(r1.WizardPendingStep("s"))

	// Fresh registry + fresh wizard on the SAME step, as RestoreWizards builds.
	r2, w2 := newChoiceRegistry("s")

	resp, _, handled := r2.HandleWizardChoice("s", preRestartToken, 0)
	if !handled {
		t.Fatal("pre-restart token must answer the restored wizard on the same step")
	}
	if resp != "moved on" || len(w2.handles) != 1 {
		t.Errorf("restored press: resp=%q handles=%v", resp, w2.handles)
	}
}

// Verifies the button contract end to end for every structured step of every
// wizard: feeding each option's LABEL to Handle at that step advances the
// wizard or finishes it — it is never bounced back as the step's re-ask.
// This is what makes a tap equivalent to typing the label.
func TestStructuredStepLabelsAreValidHandleInputs(t *testing.T) {
	dir := t.TempDir()

	t.Run("android confirmEnable", func(t *testing.T) {
		for _, label := range []string{"yes", "no"} {
			cfgPath := filepath.Join(dir, "foci-enable.toml")
			if err := os.WriteFile(cfgPath, []byte("[[platforms]]\nid = \"telegram\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			w := newTestAndroidWizard(&mockSecretsStore{data: map[string]string{}}, cfgPath)
			w.step = androidStepConfirmEnable

			resp, done := w.Handle(label)
			if !done && w.step == androidStepConfirmEnable {
				t.Errorf("label %q did not advance confirmEnable (resp=%q)", label, resp)
			}
			if strings.Contains(resp, "Reply `yes` or `no`") {
				t.Errorf("label %q was re-asked: %q", label, resp)
			}
		}
	})

	t.Run("android confirmRestart", func(t *testing.T) {
		orig := restartFunc
		defer func() { restartFunc = orig }()
		restartFunc = func() (string, error) { return "Restarting...", nil }

		for _, label := range []string{"yes", "no"} {
			w := newTestAndroidWizard(&mockSecretsStore{data: map[string]string{}}, filepath.Join(dir, "foci-restart.toml"))
			w.step = androidStepConfirmRestart

			resp, done := w.Handle(label)
			if !done {
				t.Errorf("label %q must finish confirmRestart (resp=%q)", label, resp)
			}
			if strings.Contains(resp, "Reply `yes` or `no`") {
				t.Errorf("label %q was re-asked: %q", label, resp)
			}
		}
	})

	t.Run("config section", func(t *testing.T) {
		for _, section := range config.FieldSections() {
			w := newConfigSetWizard(testConfigSetDeps(nil))
			resp, done := w.Handle(section)
			if done || w.step != 1 {
				t.Errorf("section label %q must advance to the key step (step=%d resp=%q)", section, w.step, resp)
			}
			if strings.Contains(resp, "Unknown section") {
				t.Errorf("section label %q was rejected: %q", section, resp)
			}
		}
	})

	t.Run("config key", func(t *testing.T) {
		for _, section := range config.FieldSections() {
			for _, f := range config.FieldsInSection(section) {
				w := newConfigSetWizard(testConfigSetDeps(nil))
				w.Handle(section)
				resp, done := w.Handle(f.Key)
				if done || w.step != 2 {
					t.Errorf("key label %q in %q must advance to the value step (step=%d resp=%q)", f.Key, section, w.step, resp)
				}
				if strings.Contains(resp, "Unknown key") {
					t.Errorf("key label %q in %q was rejected: %q", f.Key, section, resp)
				}
			}
		}
	})

	t.Run("config bool value", func(t *testing.T) {
		for _, label := range []string{"true", "false"} {
			w := newConfigSetWizard(testConfigSetDeps(nil))
			w.Handle("debug")
			w.Handle("messages_in_log")
			resp, done := w.Handle(label)
			if !done {
				t.Errorf("bool label %q must finish the wizard (resp=%q)", label, resp)
			}
			if strings.Contains(resp, "Invalid value") {
				t.Errorf("bool label %q was rejected: %q", label, resp)
			}
		}
	})

	t.Run("agents model", func(t *testing.T) {
		for _, label := range []string{"opus", "sonnet", "haiku"} {
			w := newAgentWizard(testDeps(nil, nil))
			w.step = stepModel
			_, done := w.Handle(label)
			if done || w.step != stepCharMode {
				t.Errorf("model label %q must advance to charMode (step=%d)", label, w.step)
			}
		}
	})
}

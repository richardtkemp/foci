package dispatch

import (
	"context"
	"fmt"
	"testing"

	"foci/internal/command"
	"foci/internal/question"
)

// stepWizard is a wizard double with a configurable structured step: Handle
// records its text and (when doneOnHandle) finishes the wizard.
type stepWizard struct {
	handles      []string
	step         *question.Question // nil = free-text step
	response     string
	doneOnHandle bool
}

func (w *stepWizard) Handle(text string) (string, bool) {
	w.handles = append(w.handles, text)
	return w.response, w.doneOnHandle
}

func (w *stepWizard) PendingStep() *question.Question { return w.step }

// yesNoQuestion is the canonical structured step used across these tests.
func yesNoQuestion() *question.Question {
	return &question.Question{
		Header:   "Confirm",
		Question: "Proceed?",
		Options:  []question.Option{{Label: "yes"}, {Label: "no"}},
	}
}

// TestWizardButtons_OptionsThenCancel verifies the wizard button set: one
// button per option (label = option label, data "<token>:<index>") followed
// by a trailing Cancel button ("<token>:cancel"), and that the full callback
// data — "wz:" + data — fits Telegram's 64-byte callback_data limit even with
// a 2-digit option index.
func TestWizardButtons_OptionsThenCancel(t *testing.T) {
	// 12 options so indices reach two digits.
	opts := make([]question.Option, 12)
	for i := range opts {
		opts[i] = question.Option{Label: fmt.Sprintf("option-%02d", i)}
	}
	q := &question.Question{Header: "H", Question: "Q?", Options: opts}

	btns := WizardButtons(q)
	if len(btns) != len(opts)+1 {
		t.Fatalf("buttons = %d, want %d (options + Cancel)", len(btns), len(opts)+1)
	}
	for i, opt := range opts {
		if btns[i].Label != opt.Label {
			t.Errorf("button %d label = %q, want the option label %q", i, btns[i].Label, opt.Label)
		}
	}
	last := btns[len(btns)-1]
	if last.Label != "Cancel" {
		t.Errorf("last button = %q, want Cancel", last.Label)
	}

	// Data grammar and the Telegram 64-byte budget, via the token the
	// registry will later verify (one source: WizardStepToken).
	tok := command.WizardStepToken(q)
	for i := 0; i < len(opts); i++ {
		if want := fmt.Sprintf("%s:%d", tok, i); btns[i].Data != want {
			t.Errorf("option button %d data = %q, want %q", i, btns[i].Data, want)
		}
	}
	if btns[len(btns)-1].Data != tok+":cancel" {
		t.Errorf("cancel data = %q, want %q", btns[len(btns)-1].Data, tok+":cancel")
	}
	for _, b := range btns {
		if n := len("wz:" + b.Data); n > 64 {
			t.Errorf("callback data %q is %d bytes, over Telegram's 64-byte limit", "wz:"+b.Data, n)
		}
	}
}

// TestTryIntercept_WizardStepReported verifies the intercept reports the
// wizard's structured step while one is pending (so the platform can attach
// wz: buttons), and reports nil when the step is free text or the wizard just
// finished.
func TestTryIntercept_WizardStepReported(t *testing.T) {
	t.Run("structured step pending", func(t *testing.T) {
		reg := command.NewRegistry()
		reg.SetWizard("k1", &stepWizard{step: yesNoQuestion(), response: "next:"})

		ic, _ := newScopedInterceptor(reg)
		r := ic.TryIntercept(context.Background(), &InterceptMessage{Text: "answer", UserID: "u1", ChatID: 1})
		if !r.Consumed || r.WizardReply != "next:" {
			t.Fatalf("consumed=%v reply=%q, want the wizard's reply", r.Consumed, r.WizardReply)
		}
		if r.WizardStep == nil || len(r.WizardStep.Options) != 2 {
			t.Errorf("WizardStep = %+v, want the pending yes/no step", r.WizardStep)
		}
	})

	t.Run("free-text step", func(t *testing.T) {
		reg := command.NewRegistry()
		reg.SetWizard("k1", &stepWizard{response: "typing still works"})

		ic, _ := newScopedInterceptor(reg)
		r := ic.TryIntercept(context.Background(), &InterceptMessage{Text: "some value", UserID: "u1", ChatID: 1})
		if !r.Consumed || r.WizardReply == "" {
			t.Fatalf("consumed=%v reply=%q, want the wizard's reply", r.Consumed, r.WizardReply)
		}
		if r.WizardStep != nil {
			t.Errorf("WizardStep = %+v, want nil on a free-text step", r.WizardStep)
		}
	})

	t.Run("wizard finished", func(t *testing.T) {
		reg := command.NewRegistry()
		reg.SetWizard("k1", &stepWizard{step: yesNoQuestion(), response: "done!", doneOnHandle: true})

		ic, _ := newScopedInterceptor(reg)
		r := ic.TryIntercept(context.Background(), &InterceptMessage{Text: "yes", UserID: "u1", ChatID: 1})
		if !r.Consumed || r.WizardReply != "done!" {
			t.Fatalf("consumed=%v reply=%q", r.Consumed, r.WizardReply)
		}
		if r.WizardStep != nil {
			t.Errorf("WizardStep = %+v, want nil after the wizard finished", r.WizardStep)
		}
		if reg.WizardActive("k1") {
			t.Error("wizard must be cleared after done")
		}
	})
}

// TestDispatchCommand_WizardStepOnActivation verifies ResponseOutcome carries
// the activated wizard's structured step — for a typed command AND a button
// callback — but NOT for a command that merely ran while an older wizard was
// already active (generation unchanged).
func TestDispatchCommand_WizardStepOnActivation(t *testing.T) {
	newReg := func() *command.Registry {
		reg := command.NewRegistry()
		reg.Register(&command.Command{
			Name: "wizstart",
			Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
				reg.SetWizard(req.SessionKey, &stepWizard{step: yesNoQuestion(), response: "pick one"})
				return command.Response{Text: "wizard started"}, nil
			},
		})
		reg.Register(&command.Command{
			Name: "ping",
			Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
				return command.Response{Text: "pong"}, nil
			},
		})
		return reg
	}

	t.Run("typed command activates wizard", func(t *testing.T) {
		reg := newReg()
		d := NewDispatcher(reg, command.CommandContext{}, "agent1")
		d.SetSessionKeyFunc(func(chatID int64) string { return fmt.Sprintf("k%d", chatID) })

		outcome := d.DispatchCommand(context.Background(), "/wizstart", 2, "u")
		if outcome.NotHandled || outcome.Response == nil {
			t.Fatalf("outcome: notHandled=%v response=%v", outcome.NotHandled, outcome.Response)
		}
		if outcome.Response.WizardStep == nil || len(outcome.Response.WizardStep.Options) != 2 {
			t.Errorf("WizardStep = %+v, want the wizard's first structured step", outcome.Response.WizardStep)
		}
	})

	t.Run("button callback activates wizard", func(t *testing.T) {
		reg := newReg()
		d := NewDispatcher(reg, command.CommandContext{}, "agent1")
		d.SetSessionKeyFunc(func(chatID int64) string { return fmt.Sprintf("k%d", chatID) })

		outcome := d.DispatchCommandCallback(context.Background(), 2, "/wizstart")
		if outcome.NotHandled || outcome.Response == nil {
			t.Fatalf("outcome: notHandled=%v response=%v", outcome.NotHandled, outcome.Response)
		}
		if outcome.Response.WizardStep == nil {
			t.Error("callback-activated wizard must carry its structured step")
		}
	})

	t.Run("command under an older wizard gets no buttons", func(t *testing.T) {
		reg := newReg()
		d := NewDispatcher(reg, command.CommandContext{}, "agent1")
		d.SetSessionKeyFunc(func(chatID int64) string { return fmt.Sprintf("k%d", chatID) })

		// An older wizard is already active for chat 2's scope…
		reg.SetWizard("k2", &stepWizard{step: yesNoQuestion(), response: "waiting"})
		// …then an unrelated command runs there: generation unchanged.
		outcome := d.DispatchCommand(context.Background(), "/ping", 2, "u")
		if outcome.NotHandled || outcome.Response == nil {
			t.Fatalf("outcome: notHandled=%v response=%v", outcome.NotHandled, outcome.Response)
		}
		if outcome.Response.WizardStep != nil {
			t.Errorf("WizardStep = %+v, want nil: /ping did not activate the wizard", outcome.Response.WizardStep)
		}
	})

	t.Run("free-text activation carries no step", func(t *testing.T) {
		reg := command.NewRegistry()
		reg.Register(&command.Command{
			Name: "wizfree",
			Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
				reg.SetWizard(req.SessionKey, &stepWizard{response: "type it"}) // no structured step
				return command.Response{Text: "wizard started"}, nil
			},
		})
		d := NewDispatcher(reg, command.CommandContext{}, "agent1")
		d.SetSessionKeyFunc(func(chatID int64) string { return fmt.Sprintf("k%d", chatID) })

		outcome := d.DispatchCommand(context.Background(), "/wizfree", 1, "u")
		if outcome.NotHandled || outcome.Response == nil {
			t.Fatalf("outcome: notHandled=%v response=%v", outcome.NotHandled, outcome.Response)
		}
		if outcome.Response.WizardStep != nil {
			t.Errorf("WizardStep = %+v, want nil for a free-text first step", outcome.Response.WizardStep)
		}
	})
}

// TestDispatcherIsWizardCancel verifies the /cancel reroute predicate: true
// only for a bare /cancel (any case, outer spaces ignored) from a chat whose
// scope has an active wizard; false for other text, other chats, and a nil
// dispatcher.
func TestDispatcherIsWizardCancel(t *testing.T) {
	reg := command.NewRegistry()
	reg.SetWizard("k1", &stepWizard{response: "waiting"})
	d := NewDispatcher(reg, command.CommandContext{}, "agent1")
	d.SetSessionKeyFunc(func(chatID int64) string { return fmt.Sprintf("k%d", chatID) })

	for _, text := range []string{"/cancel", "/CANCEL", "/Cancel", "  /cancel  "} {
		if !d.IsWizardCancel(1, text) {
			t.Errorf("IsWizardCancel(1, %q) = false, want true (wizard active for chat 1)", text)
		}
	}
	for _, text := range []string{"/stop", "/cancelled", "/cancel now", "cancel", "/config set"} {
		if d.IsWizardCancel(1, text) {
			t.Errorf("IsWizardCancel(1, %q) = true, want false (not a bare /cancel)", text)
		}
	}
	if d.IsWizardCancel(2, "/cancel") {
		t.Error("IsWizardCancel(2, /cancel) = true, want false (no wizard for chat 2)")
	}

	var nilD *Dispatcher
	if nilD.IsWizardCancel(1, "/cancel") {
		t.Error("nil dispatcher must be safe and report false")
	}
}

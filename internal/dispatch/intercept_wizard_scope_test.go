package dispatch

import (
	"context"
	"fmt"
	"testing"

	"foci/internal/command"
)

// countingWizard is a wizard double that records every Handle call, so tests
// can prove not just that a message wasn't consumed but that the other chat's
// wizard never even saw it.
type countingWizard struct {
	handles  []string
	response string
	done     bool
}

func (w *countingWizard) Handle(text string) (string, bool) {
	w.handles = append(w.handles, text)
	return w.response, w.done
}

// newScopedInterceptor builds an Interceptor shaped like a primary bot's: a
// real Dispatcher whose session-key resolver maps each chat to its own key
// (chat N → "kN"), and a SessionKeyFn that reports chat 1's key — the DEFAULT
// chat's key, exactly what Bot.SessionKey() returns in production.
func newScopedInterceptor(reg *command.Registry) (*Interceptor, *Dispatcher) {
	d := NewDispatcher(reg, command.CommandContext{}, "agent1")
	d.SetSessionKeyFunc(func(chatID int64) string { return fmt.Sprintf("k%d", chatID) })
	ic := &Interceptor{
		Commands:     reg,
		LastMsgStore: command.NewLastMessageStore(),
		Dispatcher:   d,
		IsSecondary:  false,
		SessionKeyFn: func() string { return "k1" }, // default chat's key, as in production
		LogWarnf:     func(string, ...any) {},
		LogDebugf:    func(string, ...any) {},
	}
	return ic, d
}

// TestTryInterceptWizardScopedToStartingChat proves a wizard is fed only by
// the chat it was started under: a wizard activated for chat 2's key consumes
// chat 2's text with its reply, while chat 1's text never reaches it (not
// consumed, Handle not called) even though SessionKeyFn reports chat 1's key.
func TestTryInterceptWizardScopedToStartingChat(t *testing.T) {
	reg := command.NewRegistry()
	w := &countingWizard{response: "step two"}
	reg.SetWizard("k2", w)

	ic, _ := newScopedInterceptor(reg)

	// Message from chat 1 (the default chat): not consumed, wizard untouched.
	r1 := ic.TryIntercept(context.Background(), &InterceptMessage{Text: "hello", UserID: "u1", ChatID: 1})
	if r1.Consumed {
		t.Fatal("chat 1 message must not be consumed by chat 2's wizard")
	}
	if len(w.handles) != 0 {
		t.Errorf("chat 2's wizard Handle called with chat 1 text: %v", w.handles)
	}

	// Message from chat 2: consumed with the wizard's reply.
	r2 := ic.TryIntercept(context.Background(), &InterceptMessage{Text: "the value", UserID: "u2", ChatID: 2})
	if !r2.Consumed {
		t.Fatal("chat 2 message must be consumed by its own wizard")
	}
	if r2.WizardReply != "step two" {
		t.Errorf("WizardReply = %q, want %q", r2.WizardReply, "step two")
	}
	if len(w.handles) != 1 || w.handles[0] != "the value" {
		t.Errorf("wizard handled %v, want [%q]", w.handles, "the value")
	}
}

// TestTryInterceptDefaultChatWizardDoesNotConsumeOtherChat proves the first
// Problem case at the dispatch level: a wizard started in the DEFAULT chat
// (chat 1) never sees chat 2's ordinary text — it falls through the pipeline
// and is recorded for /repeat exactly as if no wizard existed.
func TestTryInterceptDefaultChatWizardDoesNotConsumeOtherChat(t *testing.T) {
	reg := command.NewRegistry()
	w := &countingWizard{response: "default chat wizard"}
	reg.SetWizard("k1", w)

	ic, _ := newScopedInterceptor(reg)

	r := ic.TryIntercept(context.Background(), &InterceptMessage{Text: "my secret value", UserID: "u2", ChatID: 2})
	if r.Consumed {
		t.Fatal("chat 2 message must not be consumed by chat 1's wizard")
	}
	if len(w.handles) != 0 {
		t.Errorf("chat 1's wizard Handle called with chat 2 text: %v", w.handles)
	}
	if got := ic.LastMsgStore.Get("u2"); got != "my secret value" {
		t.Errorf("chat 2 text not recorded for /repeat: got %q", got)
	}
	if !reg.WizardActive("k1") {
		t.Error("chat 1's wizard must stay active and unchanged")
	}
}

// TestTryInterceptCancelOnlyCancelsOwnChatWizard proves /cancel is scoped too:
// /cancel from chat 2 (no wizard there) leaves chat 1's wizard active, while
// /cancel from chat 1 clears it.
func TestTryInterceptCancelOnlyCancelsOwnChatWizard(t *testing.T) {
	reg := command.NewRegistry()
	reg.SetWizard("k1", &countingWizard{response: "waiting"})

	ic, _ := newScopedInterceptor(reg)

	r := ic.TryIntercept(context.Background(), &InterceptMessage{Text: "/cancel", UserID: "u2", ChatID: 2})
	if r.Consumed && r.WizardReply != "" {
		t.Fatalf("chat 2 /cancel got a wizard reply %q; it must not touch chat 1's wizard", r.WizardReply)
	}
	if !reg.WizardActive("k1") {
		t.Fatal("chat 1's wizard was cancelled by chat 2's /cancel")
	}

	// Positive control: /cancel from the wizard's own chat clears it.
	r = ic.TryIntercept(context.Background(), &InterceptMessage{Text: "/cancel", UserID: "u1", ChatID: 1})
	if !r.Consumed || r.WizardReply != "Wizard cancelled." {
		t.Fatalf("chat 1 /cancel: consumed=%v reply=%q, want cancellation reply", r.Consumed, r.WizardReply)
	}
	if reg.WizardActive("k1") {
		t.Error("chat 1 /cancel must clear chat 1's wizard")
	}
}

// TestTryInterceptCallbackStartedWizardFedByOwnChat proves a wizard started
// from an inline-button callback (DispatchCommandCallback resolves the INBOUND
// chat's key) is fed by text from that same chat, not the default chat.
func TestTryInterceptCallbackStartedWizardFedByOwnChat(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "wizstart",
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			reg.SetWizard(req.SessionKey, &countingWizard{response: "callback wizard step"})
			return command.Response{Text: "wizard started"}, nil
		},
	})

	ic, d := newScopedInterceptor(reg)

	// Button pressed in chat 2 starts the wizard under chat 2's key.
	outcome := d.DispatchCommandCallback(context.Background(), 2, "/wizstart")
	if outcome.NotHandled {
		t.Fatal("expected /wizstart callback to be handled")
	}
	if !reg.WizardActive("k2") {
		t.Fatal("callback wizard must be stored under chat 2's key")
	}

	// Text from chat 2 feeds it; text would NOT feed it from chat 1.
	r := ic.TryIntercept(context.Background(), &InterceptMessage{Text: "answer", UserID: "u2", ChatID: 2})
	if !r.Consumed || r.WizardReply != "callback wizard step" {
		t.Fatalf("chat 2 text: consumed=%v reply=%q, want the wizard's reply", r.Consumed, r.WizardReply)
	}
}

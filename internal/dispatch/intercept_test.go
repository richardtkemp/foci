package dispatch

import (
	"context"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/platform"
	"foci/internal/warnings"
)

// fakeWizard implements command.WizardHandler for testing wizard intercept.
type fakeWizard struct {
	response string
	done     bool
}

func (w *fakeWizard) Handle(text string) (string, bool) {
	return w.response, w.done
}

// fakeHandler implements platform.MessageHandler for testing TransformMessage.
// Only TransformMessage is used by the Interceptor; other methods panic if called.
type fakeHandler struct {
	transform func(string) string
}

func (h *fakeHandler) HandleMessage(_ context.Context, _ string, _ []string, _ []platform.Attachment) error {
	panic("not used by Interceptor")
}

func (h *fakeHandler) TransformMessage(text string) string {
	return h.transform(text)
}

func (h *fakeHandler) Warnings() *warnings.Queue {
	panic("not used by Interceptor")
}

// newTestInterceptor builds an Interceptor with sensible defaults for testing.
// The Dispatcher is nil (no command routing) and IsSecondary is false.
func newTestInterceptor(reg *command.Registry) *Interceptor {
	return &Interceptor{
		Commands:     reg,
		LastMsgStore: command.NewLastMessageStore(),
		Dispatcher:   nil,
		IsSecondary:  false,
		LogWarnf:     func(string, ...any) {},
		LogDebugf:    func(string, ...any) {},
	}
}

// TestIsRoutableCommand verifies the routing gate: real slash commands route,
// but leading-slash filesystem paths (which contain a further "/") do NOT —
// they must fall through to the agent as normal text. This mirrors the #770
// guard in DispatchText; without it a path passes the gate, gets declined by
// the dispatcher as NotHandled, and is silently dropped (the scout bug where
// "/home/rich/.../chroma.py:75 - error: why?" got no response).
func TestIsRoutableCommand(t *testing.T) {
	t.Parallel()
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name:    "status",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) { return command.Response{}, nil },
	})
	tests := []struct {
		text string
		want bool
	}{
		{"/status", true},                                  // real command
		{"/status arg", true},                              // command with args
		{"/", true},                                        // bare slash → empty name, routes
		{"/home/rich/git/embed/chroma.py:75 - why?", false}, // the scout bug: path, not command
		{"/etc/hosts", false},                              // path
		{"/usr/local/bin/foci is broken", false},           // path with trailing words
		{".status", true},                                  // known dot command
		{".sigh", false},                                   // unknown dot → falls through as text
		{"hello world", false},                             // plain text
		{"", false},                                        // empty
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			if got := IsRoutableCommand(tt.text, reg); got != tt.want {
				t.Errorf("IsRoutableCommand(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

// TestIsSlashPath verifies the shared path-vs-command predicate used by both
// IsRoutableCommand and DispatchText (#770).
func TestIsSlashPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		text string
		want bool
	}{
		{"/status", false},                                 // command, no embedded slash
		{"/status arg", false},                             // command with args
		{"/", false},                                       // bare slash → empty name
		{"/home/rich/git/embed/chroma.py:75 - why?", true}, // the scout bug
		{"/etc/hosts", true},                               // path
		{"/usr/local/bin/foci is broken", true},            // path with trailing words
		{"  /etc/hosts", true},                             // leading whitespace trimmed
		{"hello world", false},                             // not slash-prefixed
		{"", false},                                        // empty
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			if got := isSlashPath(tt.text); got != tt.want {
				t.Errorf("isSlashPath(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

// TestTryInterceptWizardActive verifies that when a wizard is active on the
// registry, all text messages are routed to the wizard and the result is
// returned as a consumed WizardReply.
func TestTryInterceptWizardActive(t *testing.T) {
	reg := command.NewRegistry()
	reg.SetWizard(&fakeWizard{response: "wizard says hello", done: false})

	ic := newTestInterceptor(reg)
	msg := &InterceptMessage{Text: "anything", UserID: "u1", ChatID: 1}

	result := ic.TryIntercept(context.Background(), msg)
	if !result.Consumed {
		t.Fatal("expected message to be consumed by wizard")
	}
	if result.WizardReply != "wizard says hello" {
		t.Errorf("expected wizard reply, got %q", result.WizardReply)
	}
}

// TestTryInterceptWizardDone verifies that when the wizard marks itself done,
// subsequent messages fall through to normal dispatch.
func TestTryInterceptWizardDone(t *testing.T) {
	reg := command.NewRegistry()
	reg.SetWizard(&fakeWizard{response: "done!", done: true})

	ic := newTestInterceptor(reg)

	// First message is consumed by wizard (which marks done).
	msg1 := &InterceptMessage{Text: "finish", UserID: "u1", ChatID: 1}
	r1 := ic.TryIntercept(context.Background(), msg1)
	if !r1.Consumed {
		t.Fatal("expected first message consumed by wizard")
	}

	// Second message should not be intercepted by wizard (it's cleared).
	msg2 := &InterceptMessage{Text: "hello agent", UserID: "u1", ChatID: 1}
	r2 := ic.TryIntercept(context.Background(), msg2)
	if r2.Consumed {
		t.Fatal("expected second message to not be consumed (wizard done)")
	}
}

// TestTryInterceptRecordsLastMessage verifies that non-command, non-slash text
// is recorded in the LastMessageStore for the /repeat command.
func TestTryInterceptRecordsLastMessage(t *testing.T) {
	reg := command.NewRegistry()
	ic := newTestInterceptor(reg)

	msg := &InterceptMessage{Text: "remember this", UserID: "u1", ChatID: 1}
	ic.TryIntercept(context.Background(), msg)

	if got := ic.LastMsgStore.Get("u1"); got != "remember this" {
		t.Errorf("expected last message to be recorded, got %q", got)
	}
}

// TestTryInterceptSlashNotRecorded verifies that slash commands are NOT
// recorded in the LastMessageStore (only plain text is).
func TestTryInterceptSlashNotRecorded(t *testing.T) {
	reg := command.NewRegistry()
	ic := newTestInterceptor(reg)

	msg := &InterceptMessage{Text: "/status", UserID: "u1", ChatID: 1}
	ic.TryIntercept(context.Background(), msg)

	if got := ic.LastMsgStore.Get("u1"); got != "" {
		t.Errorf("expected slash command not recorded, got %q", got)
	}
}

// TestTryInterceptStaleCommandDropped verifies that a slash command with a
// timestamp older than 30 seconds is silently dropped (consumed with no reply).
func TestTryInterceptStaleCommandDropped(t *testing.T) {
	reg := command.NewRegistry()
	var warned bool
	ic := newTestInterceptor(reg)
	ic.LogWarnf = func(string, ...any) { warned = true }

	msg := &InterceptMessage{
		Text:      "/restart",
		UserID:    "u1",
		ChatID:    1,
		Timestamp: time.Now().Add(-60 * time.Second),
	}

	result := ic.TryIntercept(context.Background(), msg)
	if !result.Consumed {
		t.Fatal("expected stale command to be consumed")
	}
	if result.WizardReply != "" || result.Outcome != nil {
		t.Error("expected silent consumption (no reply or outcome)")
	}
	if !warned {
		t.Error("expected a warning log for stale command")
	}
}

// TestTryInterceptFreshCommandNotDropped verifies that a slash command within
// the 30-second staleness window is NOT dropped by the staleness check. Since
// there's no dispatcher, it falls through as not-consumed.
func TestTryInterceptFreshCommandNotDropped(t *testing.T) {
	reg := command.NewRegistry()
	ic := newTestInterceptor(reg)

	msg := &InterceptMessage{
		Text:      "/status",
		UserID:    "u1",
		ChatID:    1,
		Timestamp: time.Now(),
	}

	result := ic.TryIntercept(context.Background(), msg)
	// Not consumed because there's no dispatcher to handle /status.
	if result.Consumed {
		t.Error("expected fresh command to not be consumed by staleness check alone")
	}
}

// TestTryInterceptZeroTimestampSkipsStalenessCheck verifies that when
// Timestamp is zero (the default), the staleness check is skipped entirely.
func TestTryInterceptZeroTimestampSkipsStalenessCheck(t *testing.T) {
	reg := command.NewRegistry()
	ic := newTestInterceptor(reg)

	msg := &InterceptMessage{
		Text:   "/something",
		UserID: "u1",
		ChatID: 1,
		// Timestamp is zero — no staleness check.
	}

	result := ic.TryIntercept(context.Background(), msg)
	// Not consumed (no dispatcher), but also not dropped by staleness.
	if result.Consumed {
		t.Error("expected zero-timestamp command to pass through staleness check")
	}
}

// TestTryInterceptCommandDispatch verifies that a slash command is dispatched
// via the Dispatcher and returns a consumed result with the CommandOutcome.
func TestTryInterceptCommandDispatch(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "status",
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "all systems go"}, nil
		},
	})

	d := NewDispatcher(reg, command.CommandContext{}, "agent1")
	ic := newTestInterceptor(reg)
	ic.Dispatcher = d

	msg := &InterceptMessage{Text: "/status", UserID: "u1", ChatID: 1}
	result := ic.TryIntercept(context.Background(), msg)

	if !result.Consumed {
		t.Fatal("expected command to be consumed")
	}
	if result.Outcome == nil {
		t.Fatal("expected non-nil outcome")
	}
	if result.Outcome.Response == nil {
		t.Fatal("expected response outcome")
	}
	if result.Outcome.Response.Result.Response.Text != "all systems go" {
		t.Errorf("unexpected response: %q", result.Outcome.Response.Result.Response.Text)
	}
}

// TestTryInterceptTransformProducesCommand verifies that when a
// MessageHandler.TransformMessage converts plain text into a command, it is
// dispatched and consumed.
func TestTryInterceptTransformProducesCommand(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "mana",
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "mana balance: 42"}, nil
		},
	})

	d := NewDispatcher(reg, command.CommandContext{}, "agent1")
	ic := newTestInterceptor(reg)
	ic.Dispatcher = d
	ic.Handler = &fakeHandler{
		transform: func(text string) string {
			if text == "m" {
				return "/mana"
			}
			return text
		},
	}

	msg := &InterceptMessage{Text: "m", UserID: "u1", ChatID: 1}
	result := ic.TryIntercept(context.Background(), msg)

	if !result.Consumed {
		t.Fatal("expected transformed command to be consumed")
	}
	if result.Outcome == nil || result.Outcome.Response == nil {
		t.Fatal("expected response outcome from transformed command")
	}
	if result.Outcome.Response.Result.Response.Text != "mana balance: 42" {
		t.Errorf("unexpected response: %q", result.Outcome.Response.Result.Response.Text)
	}
}

// TestTryInterceptTransformNoChange verifies that when TransformMessage returns
// the same text, no second dispatch attempt is made.
func TestTryInterceptTransformNoChange(t *testing.T) {
	reg := command.NewRegistry()
	ic := newTestInterceptor(reg)
	ic.Handler = &fakeHandler{
		transform: func(text string) string { return text }, // no-op
	}

	msg := &InterceptMessage{Text: "hello", UserID: "u1", ChatID: 1}
	result := ic.TryIntercept(context.Background(), msg)

	// Should fall through without being consumed (no dispatcher, no transform).
	if result.Consumed {
		t.Error("expected no-op transform to not consume the message")
	}
}

// TestTryInterceptTransformNonCommand verifies that when a transform produces
// non-command text, the result carries the transformed text for downstream use.
func TestTryInterceptTransformNonCommand(t *testing.T) {
	reg := command.NewRegistry()
	ic := newTestInterceptor(reg)
	ic.Handler = &fakeHandler{
		transform: func(text string) string {
			if text == "ot" {
				return "Check back over our conversation for open threads."
			}
			return text
		},
	}

	msg := &InterceptMessage{Text: "ot", UserID: "u1", ChatID: 1}
	result := ic.TryIntercept(context.Background(), msg)

	if result.Consumed {
		t.Error("non-command transform should not consume the message")
	}
	if result.Text != "Check back over our conversation for open threads." {
		t.Errorf("Text = %q, want transformed text", result.Text)
	}
}

// TestTryInterceptTextAlwaysSet verifies that result.Text is set even when
// no transform fires.
func TestTryInterceptTextAlwaysSet(t *testing.T) {
	reg := command.NewRegistry()
	ic := newTestInterceptor(reg)

	msg := &InterceptMessage{Text: "hello world", UserID: "u1", ChatID: 1}
	result := ic.TryIntercept(context.Background(), msg)

	if result.Text != "hello world" {
		t.Errorf("Text = %q, want original text", result.Text)
	}
}

// TestTryInterceptSecondaryBotIdleDrop verifies that a secondary bot with no
// active session silently drops non-command messages.
func TestTryInterceptSecondaryBotIdleDrop(t *testing.T) {
	reg := command.NewRegistry()
	var debugLogged bool
	ic := newTestInterceptor(reg)
	ic.IsSecondary = true
	ic.SessionKeyFn = func() string { return "" } // idle — no session
	ic.LogDebugf = func(string, ...any) { debugLogged = true }

	msg := &InterceptMessage{Text: "hello", UserID: "u1", ChatID: 1}
	result := ic.TryIntercept(context.Background(), msg)

	if !result.Consumed {
		t.Fatal("expected idle secondary bot to consume the message")
	}
	if result.WizardReply != "" || result.Outcome != nil {
		t.Error("expected silent consumption (no reply or outcome)")
	}
	if !debugLogged {
		t.Error("expected debug log for idle secondary drop")
	}
}

// TestTryInterceptSecondaryBotWithSession verifies that a secondary bot with
// an active session does NOT drop messages — they fall through for agent
// processing.
func TestTryInterceptSecondaryBotWithSession(t *testing.T) {
	reg := command.NewRegistry()
	ic := newTestInterceptor(reg)
	ic.IsSecondary = true
	ic.SessionKeyFn = func() string { return "active-session" }

	msg := &InterceptMessage{Text: "hello", UserID: "u1", ChatID: 1}
	result := ic.TryIntercept(context.Background(), msg)

	if result.Consumed {
		t.Error("expected secondary bot with active session to not consume the message")
	}
}

// TestTryInterceptNoConditions verifies the baseline case: no wizard, no
// command match, no transform, not secondary — message passes through
// unconsumed.
func TestTryInterceptNoConditions(t *testing.T) {
	reg := command.NewRegistry()
	ic := newTestInterceptor(reg)

	msg := &InterceptMessage{Text: "just chatting", UserID: "u1", ChatID: 1}
	result := ic.TryIntercept(context.Background(), msg)

	if result.Consumed {
		t.Error("expected message to not be consumed")
	}
	if result.WizardReply != "" || result.Outcome != nil {
		t.Error("expected empty result")
	}
}

// TestTryInterceptEmptyText verifies that an empty text message passes through
// without being consumed, and doesn't trigger wizard or last-message recording.
func TestTryInterceptEmptyText(t *testing.T) {
	reg := command.NewRegistry()
	// Even with an active wizard, empty text should skip the wizard check.
	reg.SetWizard(&fakeWizard{response: "should not see this", done: false})

	ic := newTestInterceptor(reg)
	msg := &InterceptMessage{Text: "", UserID: "u1", ChatID: 1}
	result := ic.TryIntercept(context.Background(), msg)

	if result.Consumed {
		t.Error("expected empty text to not be consumed")
	}
}

// TestTryInterceptNonSlashNotStaleChecked verifies that non-slash text with an
// old timestamp is NOT dropped by the staleness check (only slash commands are).
func TestTryInterceptNonSlashNotStaleChecked(t *testing.T) {
	reg := command.NewRegistry()
	ic := newTestInterceptor(reg)

	msg := &InterceptMessage{
		Text:      "old plain text",
		UserID:    "u1",
		ChatID:    1,
		Timestamp: time.Now().Add(-5 * time.Minute),
	}

	result := ic.TryIntercept(context.Background(), msg)
	// Not consumed — plain text passes through regardless of age.
	if result.Consumed {
		t.Error("expected old plain text to not be consumed by staleness check")
	}
}

// TestTryInterceptDotCommandDispatch verifies that dot-prefix commands (e.g.
// ".status") are dispatched via the Dispatcher.
func TestTryInterceptDotCommandDispatch(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "ping",
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "pong"}, nil
		},
	})

	d := NewDispatcher(reg, command.CommandContext{}, "agent1")
	ic := newTestInterceptor(reg)
	ic.Dispatcher = d

	msg := &InterceptMessage{Text: ".ping", UserID: "u1", ChatID: 1}
	result := ic.TryIntercept(context.Background(), msg)

	if !result.Consumed {
		t.Fatal("expected dot command to be consumed")
	}
	if result.Outcome == nil || result.Outcome.Response == nil {
		t.Fatal("expected response outcome for dot command")
	}
	if result.Outcome.Response.Result.Response.Text != "pong" {
		t.Errorf("unexpected response: %q", result.Outcome.Response.Result.Response.Text)
	}
}

// TestTryInterceptNilDispatcher verifies that tryDispatch returns nil when
// Dispatcher is nil, so a slash command falls through without panicking.
func TestTryInterceptNilDispatcher(t *testing.T) {
	reg := command.NewRegistry()
	ic := newTestInterceptor(reg)
	// Dispatcher is nil by default in newTestInterceptor.

	msg := &InterceptMessage{Text: "/anything", UserID: "u1", ChatID: 1}
	result := ic.TryIntercept(context.Background(), msg)

	if result.Consumed {
		t.Error("expected message to pass through with nil dispatcher")
	}
}

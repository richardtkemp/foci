package dispatch

import (
	"context"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/platform"
)

// TestLimitBreak_PassedToAgentAsText (#3128): on an agent with /limit_break,
// "/lb <text>" is not a command (the routing gate and the dispatcher both
// pass it on as text, and a fresh-timestamp intercept leaves it unconsumed);
// a bare "/lb" dispatches the command, which answers its usage. On an agent
// without the command, "/lb x" stays an unknown command.
func TestLimitBreak_PassedToAgentAsText(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(command.LimitBreakCommand())
	d := NewDispatcher(reg, command.CommandContext{}, "a", command.SourceTelegram)
	for _, text := range []string{"/lb hey", "/LB hey", "/limit_break hey", "/limit-break hey", ".lb hey"} {
		if IsRoutableCommand(text, reg) {
			t.Errorf("IsRoutableCommand(%q) = true, want the message passed on", text)
		}
		if out := d.DispatchCommand(context.Background(), text, 1, "u"); !out.NotHandled {
			t.Errorf("DispatchCommand(%q) handled it, want NotHandled", text)
		}
		i := &Interceptor{Commands: reg, LastMsgStore: command.NewLastMessageStore(), Dispatcher: d,
			LogWarnf: func(string, ...any) {}, LogDebugf: func(string, ...any) {}}
		res := i.TryIntercept(context.Background(), &InterceptMessage{Text: text, UserID: "u", ChatID: 1, Timestamp: time.Now().Add(-time.Minute)})
		if res.Consumed {
			t.Errorf("TryIntercept(%q) consumed it, want it delivered to the agent", text)
		}
	}
	for _, text := range []string{"/lb", "/limit_break  "} {
		if !IsRoutableCommand(text, reg) {
			t.Errorf("bare %q not routed to the command", text)
		}
		out := d.DispatchCommand(context.Background(), text, 1, "u")
		if out.NotHandled || out.Response == nil || out.Response.Result.Response.Text != platform.LimitBreakUsage {
			t.Errorf("bare %q outcome = %+v, want the usage", text, out)
		}
	}

	plain := command.NewRegistry()
	if !IsRoutableCommand("/lb x", plain) {
		t.Error("without /limit_break, /lb x is not routed as a command")
	}
}

// TestLimitBreak_OperatorGate: a caller the operator gate forbids the command
// gets its refusal, not a forced turn; an operator's message passes on.
func TestLimitBreak_OperatorGate(t *testing.T) {
	reg := command.NewRegistry()
	reg.Register(command.LimitBreakCommand())
	policy := command.NewOperatorPolicy(reg, "a", "telegram", config.OperatorAccess{Users: []string{"111"}})
	d := NewDispatcher(reg, command.CommandContext{Operators: command.OperatorPolicies{command.SourceTelegram: policy}}, "a", command.SourceTelegram)
	out := d.DispatchCommand(context.Background(), "/lb hey", 1, "222")
	if out.NotHandled || out.Response == nil || out.Response.Result.Response.Text != refusalText("limit_break") {
		t.Errorf("non-operator /lb outcome = %+v, want the refusal", out)
	}
	if out := d.DispatchCommand(context.Background(), "/lb hey", 1, "111"); !out.NotHandled {
		t.Errorf("operator /lb was handled as a command, want it passed on")
	}
}

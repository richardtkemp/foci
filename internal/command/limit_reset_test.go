package command

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/usagehold"
)

func stoppedCCAgent() *agent.Agent {
	a := &agent.Agent{AgentID: "cam", UsageHoldRegistry: usagehold.New()}
	a.DelegatedManager = &agent.DelegatedManager{BackendType: "claude-code"}
	a.ObserveUsage(usagehold.Event{Window: "five_hour", Reached: true, ResetAt: time.Now().Add(2 * time.Hour)})
	return a
}

func TestLimitResetCommand_LiftsHold(t *testing.T) {
	a := stoppedCCAgent()
	resp, err := LimitResetCommand().Execute(context.Background(), Request{}, CommandContext{Agent: a})
	if err != nil {
		t.Fatal(err)
	}
	if a.UsageHold().Phase != usagehold.Open {
		t.Errorf("phase after /limit_reset = %v, want open", a.UsageHold().Phase)
	}
	if !strings.Contains(resp.Text, "Lifted") || !strings.Contains(resp.Text, "stop") {
		t.Errorf("reply = %q, want it to name the lifted stop", resp.Text)
	}
	resp, err = LimitResetCommand().Execute(context.Background(), Request{}, CommandContext{Agent: a})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Text, "Nothing was blocked") {
		t.Errorf("second reply = %q, want nothing blocked", resp.Text)
	}
}

// TestManaCommand_LiftsOnlyWhenAvailable is requirement 2 of #3043: /usage
// on a Claude Code agent lifts when its read shows usage available, and
// changes nothing when it does not, or when the backend does not declare
// the lift.
func TestManaCommand_LiftsOnlyWhenAvailable(t *testing.T) {
	read := func(five, week int) delegator.UsageQuery {
		return func(context.Context, delegator.UsageTarget) (*delegator.UsageInfo, error) {
			return &delegator.UsageInfo{
				Backend:             "Claude Code",
				RateLimitsAvailable: true,
				FiveHour:            delegator.UsageWindow{Percent: five, Reported: true},
				SevenDay:            delegator.UsageWindow{Percent: week, Reported: true},
			}, nil
		}
	}

	a := stoppedCCAgent()
	resp, err := ManaCommand(read(100, 40), delegator.UsageTarget{AgentID: "cam"}, true).Execute(context.Background(), Request{}, CommandContext{Agent: a})
	if err != nil {
		t.Fatal(err)
	}
	if a.UsageHold().Phase != usagehold.Stop || strings.Contains(resp.Text, "Lifted") {
		t.Errorf("a read at 100%%: phase %v, reply %q; want the stop kept and no lift", a.UsageHold().Phase, resp.Text)
	}

	if _, err := ManaCommand(read(3, 40), delegator.UsageTarget{AgentID: "cam"}, false).Execute(context.Background(), Request{}, CommandContext{Agent: a}); err != nil {
		t.Fatal(err)
	}
	if a.UsageHold().Phase != usagehold.Stop {
		t.Errorf("without the lift: phase %v, want the stop kept", a.UsageHold().Phase)
	}

	resp, err = ManaCommand(read(3, 40), delegator.UsageTarget{AgentID: "cam"}, true).Execute(context.Background(), Request{}, CommandContext{Agent: a})
	if err != nil {
		t.Fatal(err)
	}
	if a.UsageHold().Phase != usagehold.Open || !strings.Contains(resp.Text, "Usage is available") {
		t.Errorf("a read at 3%%: phase %v, reply %q; want open and the lift named", a.UsageHold().Phase, resp.Text)
	}
}

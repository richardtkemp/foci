package delegator

import "context"

// plan.go — backend-contributed /plan delivery.
//
// A backend that supports plan mode registers a PlanDelivery from its init()
// (alongside Register). The command layer registers the /plan slash command
// iff a delivery exists for the agent's backend, and delegates the
// backend-specific mechanism to it. This keeps the "which backend does what"
// knowledge next to each backend instead of a string switch at the command
// registration site (#857).

// AgentInjector is the agent-level fresh-turn injection primitive a plan
// delivery may need — the ccstream backend uses it to drive an EnterPlanMode
// turn. It is satisfied structurally by *tools.AsyncNotifier; declaring a local
// interface keeps the delegator package free of an upward import on tools.
type AgentInjector interface {
	InjectToAgent(targetSession, message, replyToSession, trigger string)
}

// PlanDeps carries the runtime handles a PlanDelivery may use. A delivery pulls
// only what its backend needs: opencode fetches the live backend to send a
// plan-agent prompt; ccstream uses the notifier to drive an EnterPlanMode
// turn. Backend is a lazy thunk so a delivery that doesn't touch the backend
// (ccstream) never forces it into existence.
type PlanDeps struct {
	SessionKey string
	Notifier   AgentInjector
	Backend    func() (Delegator, error)
}

// PlanDelivery turns a "/plan <args>" request into a delivered action against a
// specific backend and returns the user-facing confirmation string. Each
// backend that supports plan mode sets Spec.PlanDelivery (and declares
// CapPlanMode); without one the /plan command does not appear for it.
type PlanDelivery func(ctx context.Context, deps PlanDeps, args string) (string, error)

// PlanDeliveryFor returns the plan delivery of a backend (Spec.PlanDelivery),
// and whether it has one. The command layer registers /plan iff ok is true.
func PlanDeliveryFor(name string) (PlanDelivery, bool) {
	s, _ := SpecFor(name)
	return s.PlanDelivery, s.PlanDelivery != nil
}

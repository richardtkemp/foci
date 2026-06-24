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
// only what its backend needs: cctmux fetches the live backend for a verbatim
// "/plan" slash command; ccstream uses the notifier to drive an EnterPlanMode
// turn. Backend is a lazy thunk so a delivery that doesn't touch the backend
// (ccstream) never forces it into existence.
type PlanDeps struct {
	SessionKey string
	Notifier   AgentInjector
	Backend    func() (Delegator, error)
}

// PlanDelivery turns a "/plan <args>" request into a delivered action against a
// specific backend and returns the user-facing confirmation string. Each
// backend that supports plan mode registers one via RegisterPlan; the absence
// of a registration is what makes the /plan command not appear for that backend.
type PlanDelivery func(ctx context.Context, deps PlanDeps, args string) (string, error)

var planDeliveries = make(map[string]PlanDelivery)

// RegisterPlan associates a plan delivery with a backend name. Typically called
// from a backend package's init(), alongside Register. Backends that never call
// this simply don't get a /plan command.
func RegisterPlan(name string, d PlanDelivery) {
	registryMu.Lock()
	defer registryMu.Unlock()
	planDeliveries[name] = d
}

// PlanDeliveryFor returns the plan delivery registered for a backend name, and
// whether one exists. The command layer registers /plan iff ok is true.
func PlanDeliveryFor(name string) (PlanDelivery, bool) {
	registryMu.Lock()
	defer registryMu.Unlock()
	d, ok := planDeliveries[name]
	return d, ok
}

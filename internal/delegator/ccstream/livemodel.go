// livemodel.go — LiveModelReporter implementation for ccstream (#2252).
//
// CC names its exact model before any turn completes: the system/init message
// at process start carries the resolved id ("claude-opus-5-5", not the "opus"
// alias foci launched it with), and every top-level assistant message restates
// it. OnResult already reports that value as TurnResult.Model, but only at
// turn end — until then foci's session meta still holds the configured alias,
// so whoami would report "opus" while the session demonstrably runs
// claude-opus-5-5. LiveModel exposes the same lastModel OnResult reads, with
// the same prefixedModel spelling, so callers can ask for the resolved id any
// time the process is running.

package ccstream

// LiveModel implements delegator.LiveModelReporter: the model CC last named
// (system/init or a top-level assistant message), spelled exactly as
// TurnResult.Model (prefixedModel). Read-only: no I/O, nothing sent to the CC
// process, never blocks on a turn. Returns "" when no model is known yet;
// never returns the synthetic sentinel.
func (b *Backend) LiveModel() string {
	b.mu.Lock()
	model := b.lastModel
	b.mu.Unlock()
	if model == "" || model == syntheticModel {
		return ""
	}
	return prefixedModel(model)
}

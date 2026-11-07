package codex

// LiveModel implements delegator.LiveModelReporter: the model codex last named,
// from thread start, thread resume, a fork or model/rerouted, spelled exactly as
// TurnResult.Model (qualifiedModel). Read-only: no I/O, nothing sent to codex,
// never blocks on a turn. Returns "" when no model is known yet, and between a
// choice of model and the turn/start that sends it (#2879).
func (b *Backend) LiveModel() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.modelSwitchPending {
		return ""
	}
	return qualifiedModel(b.model)
}

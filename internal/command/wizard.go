package command

// The Registry's wizard machinery: per-session activation, message takeover,
// optional structured steps, and restart persistence.
//
// Wizards are keyed by SCOPE — the session key of the conversation that
// started them — so two conversations (or two platforms) can each run their
// own wizard concurrently, and input from one session can never advance or
// hijack another session's wizard. An empty scope is legal (it behaves as one
// shared per-agent slot, the pre-scoping behaviour) but every current caller
// supplies the request's session key.
//
// Persistence mirrors the ask tool's "ask_pending" pattern: wizards that
// implement WizardSnapshotter are checkpointed to the session index on every
// mutation and restored at startup (RestoreWizards), so a server restart no
// longer strands the user mid-flow.

import (
	"encoding/json"
	"strings"
	"time"

	"foci/internal/log"
	"foci/internal/question"
	"foci/internal/session"
)

// WizardHandler is implemented by interactive wizards that take over message routing.
// While a wizard is active for a scope, all of that scope's messages are routed to
// Handle() instead of normal command dispatch or the agent queue.
type WizardHandler interface {
	Handle(text string) (response string, done bool)
}

// WizardDocProvider is an optional wizard capability: after Handle produces its
// reply, a wizard implementing this may also return a file to send alongside it
// (e.g. a QR image). The path is consumed once; the platform layer sends and
// then removes it.
type WizardDocProvider interface {
	PendingDoc() (path string)
}

// WizardStepProvider is an optional wizard capability: a wizard implementing
// it describes its CURRENT step as structured data (question text, header,
// options), which capable clients render as buttons instead of a plain text
// prompt. nil means "no structure for this step" — the transport falls back to
// a free-text step built from the plain prompt string. Option labels double as
// the text fed back to Handle when the user picks one (see the app path's
// answer translation), so they must be valid Handle inputs.
type WizardStepProvider interface {
	PendingStep() *question.Question
}

// WizardSnapshotter is an optional wizard capability: wizards implementing it
// survive a server restart. Kind names the wizard type for the restore factory
// (newWizardForKind); Snapshot serialises the wizard's collected state (step
// index + gathered values — never injected deps); Restore applies a snapshot
// onto a freshly-constructed instance. Wizards without it simply aren't
// persisted — a restart drops them, as before.
type WizardSnapshotter interface {
	WizardKind() string
	SnapshotWizard() ([]byte, error)
	RestoreWizard(data []byte) error
}

// wizardEntry is one scope's active wizard. gen is minted from the Registry's
// monotonic counter at activation, so "which wizard" checks survive
// replacement (a stale reference never matches a newer activation's gen).
type wizardEntry struct {
	handler WizardHandler
	gen     uint64
}

// wizardMetaKey is the agent_metadata key holding persisted in-flight wizards
// (JSON map scope → persistedWizard), mirroring the ask tool's "ask_pending".
const wizardMetaKey = "wizard_pending"

// wizardPendingTTL caps how long a persisted wizard survives across restarts —
// same 24h budget as pending asks.
const wizardPendingTTL = 24 * time.Hour

// persistedWizard is the stored form of one snapshot-capable wizard.
type persistedWizard struct {
	Kind    string          `json:"kind"`
	Data    json.RawMessage `json:"data"`
	SavedAt time.Time       `json:"savedAt"`
}

// EnableWizardPersistence points the registry at the session index so wizard
// state checkpoints on every mutation and can be restored after a restart.
// Call once at registry construction, before RestoreWizards.
func (r *Registry) EnableWizardPersistence(store *session.SessionIndex, agentID string) {
	r.wizardMu.Lock()
	defer r.wizardMu.Unlock()
	r.wizardStore = store
	r.wizardAgent = agentID
}

// SetWizard activates a wizard for scope, replacing any wizard that scope
// already had. Callers should finish seeding the wizard's state (fast-forward
// steps) BEFORE calling, so the persisted snapshot is current.
func (r *Registry) SetWizard(scope string, w WizardHandler) {
	r.wizardMu.Lock()
	defer r.wizardMu.Unlock()
	r.wizardGen++
	if r.wizards == nil {
		r.wizards = make(map[string]*wizardEntry)
	}
	r.wizards[scope] = &wizardEntry{handler: w, gen: r.wizardGen}
	r.persistWizardsLocked()
}

// ClearWizard removes scope's active wizard.
func (r *Registry) ClearWizard(scope string) {
	r.wizardMu.Lock()
	defer r.wizardMu.Unlock()
	delete(r.wizards, scope)
	r.persistWizardsLocked()
}

// WizardActive reports whether a wizard currently intercepts scope's messages.
func (r *Registry) WizardActive(scope string) bool {
	r.wizardMu.Lock()
	defer r.wizardMu.Unlock()
	return r.wizards[scope] != nil
}

// WizardGen returns the activation generation of scope's wizard, or 0 when
// none is active. Generations are minted from one monotonic counter, so a
// caller that snapshots the gen can later tell "same wizard" (equal, non-zero)
// from "replaced or gone" — including across a Dispatch that installed a new
// wizard for the scope.
func (r *Registry) WizardGen(scope string) uint64 {
	r.wizardMu.Lock()
	defer r.wizardMu.Unlock()
	if e := r.wizards[scope]; e != nil {
		return e.gen
	}
	return 0
}

// WizardPendingStep returns scope's wizard's current step as structured data,
// or nil when no wizard is active or the wizard doesn't implement
// WizardStepProvider (callers fall back to a free-text step).
func (r *Registry) WizardPendingStep(scope string) *question.Question {
	r.wizardMu.Lock()
	defer r.wizardMu.Unlock()
	e := r.wizards[scope]
	if e == nil {
		return nil
	}
	if p, ok := e.handler.(WizardStepProvider); ok {
		return p.PendingStep()
	}
	return nil
}

// HandleMessage routes a message to scope's active wizard, if any.
// Returns (response, true) if the wizard handled the message, or ("", false)
// if no wizard is active for the scope. Handles /cancel and /stop to abort the
// wizard. docPath is a file to send alongside the reply (e.g. a QR image) when
// the wizard implements WizardDocProvider, else "".
func (r *Registry) HandleMessage(scope, text string) (response string, docPath string, handled bool) {
	r.wizardMu.Lock()
	defer r.wizardMu.Unlock()

	e := r.wizards[scope]
	if e == nil {
		return "", "", false
	}

	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "/cancel" || lower == "/stop" || lower == ".cancel" || lower == ".stop" {
		delete(r.wizards, scope)
		r.persistWizardsLocked()
		return "Wizard cancelled.", "", true
	}

	resp, done := e.handler.Handle(text)
	if p, ok := e.handler.(WizardDocProvider); ok {
		docPath = p.PendingDoc()
	}
	if done {
		delete(r.wizards, scope)
	}
	r.persistWizardsLocked()
	return resp, docPath, true
}

// persistWizardsLocked checkpoints every snapshot-capable wizard to the
// session index. Best-effort (logs, never propagates), no-op without a store.
// Caller holds wizardMu.
func (r *Registry) persistWizardsLocked() {
	if r.wizardStore == nil {
		return
	}
	saved := make(map[string]persistedWizard)
	for scope, e := range r.wizards {
		snap, ok := e.handler.(WizardSnapshotter)
		if !ok {
			continue
		}
		data, err := snap.SnapshotWizard()
		if err != nil {
			log.Warnf("command", "wizard snapshot (%s): %v", snap.WizardKind(), err)
			continue
		}
		saved[scope] = persistedWizard{Kind: snap.WizardKind(), Data: data, SavedAt: time.Now()}
	}
	if len(saved) == 0 {
		if err := r.wizardStore.DeleteAgentMetadata(r.wizardAgent, wizardMetaKey); err != nil {
			log.Warnf("command", "clear persisted wizards: %v", err)
		}
		return
	}
	blob, err := json.Marshal(saved)
	if err != nil {
		log.Warnf("command", "marshal persisted wizards: %v", err)
		return
	}
	if err := r.wizardStore.SetAgentMetadata(r.wizardAgent, wizardMetaKey, string(blob)); err != nil {
		log.Warnf("command", "persist wizards: %v", err)
	}
}

// RestoreWizards rebuilds persisted in-flight wizards after a restart. Call
// once at startup, after the CommandContext is fully assembled (the restore
// factory pulls each wizard's deps from it) and after EnableWizardPersistence.
// Stale (>TTL), unknown-kind, or unrestorable entries are dropped; the cleaned
// set is re-persisted.
func (r *Registry) RestoreWizards(cc CommandContext) {
	r.wizardMu.Lock()
	defer r.wizardMu.Unlock()
	if r.wizardStore == nil {
		return
	}
	raw, err := r.wizardStore.GetAgentMetadata(r.wizardAgent, wizardMetaKey)
	if err != nil || raw == "" {
		return
	}
	var saved map[string]persistedWizard
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		log.Warnf("command", "unmarshal persisted wizards: %v — dropping", err)
		_ = r.wizardStore.DeleteAgentMetadata(r.wizardAgent, wizardMetaKey)
		return
	}
	if r.wizards == nil {
		r.wizards = make(map[string]*wizardEntry)
	}
	restored := 0
	for scope, p := range saved {
		if time.Since(p.SavedAt) > wizardPendingTTL {
			continue
		}
		w := newWizardForKind(p.Kind, cc)
		if w == nil {
			log.Warnf("command", "persisted wizard kind %q not restorable — dropping", p.Kind)
			continue
		}
		snap, ok := w.(WizardSnapshotter)
		if !ok {
			continue
		}
		if err := snap.RestoreWizard(p.Data); err != nil {
			log.Warnf("command", "restore wizard %q: %v — dropping", p.Kind, err)
			continue
		}
		r.wizardGen++
		r.wizards[scope] = &wizardEntry{handler: w, gen: r.wizardGen}
		restored++
	}
	r.persistWizardsLocked()
	if restored > 0 {
		log.Infof("command", "restored %d in-flight wizard(s) for agent %s", restored, r.wizardAgent)
	}
}

// newWizardForKind constructs a fresh, dep-injected wizard of the given
// persisted kind, ready for RestoreWizard. All five wizards live in this
// package, so this is a plain switch rather than a registration mechanism.
// Returns nil when the kind is unknown or its deps are unavailable.
func newWizardForKind(kind string, cc CommandContext) WizardHandler {
	switch kind {
	case wizardKindAgentsNew:
		if cc.AgentNewDeps == nil {
			return nil
		}
		return newAgentWizard(*cc.AgentNewDeps)
	case wizardKindConfigSet:
		if cc.ConfigSetDeps == nil {
			return nil
		}
		return newConfigSetWizard(*cc.ConfigSetDeps)
	case wizardKindSecretsSet:
		if store := secretsResolveStore(cc); store != nil {
			return newSecretsSetWizard(store)
		}
		return nil
	case wizardKindSecretsHostsAdd:
		if store := secretsResolveStore(cc); store != nil {
			return newSecretsHostsAddWizard(store, "")
		}
		return nil
	case wizardKindAndroid:
		if cc.SecretsStore == nil {
			return nil
		}
		return newAndroidWizard(cc)
	}
	return nil
}

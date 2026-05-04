package ccstream

import (
	"sync"
	"time"
)

// OutstandingKind discriminates what kind of user prompt is awaiting a
// response. Used by the registry as bookkeeping; per-kind state (questions,
// schema, answers) lives in the kind-specific stores (pendingPermission,
// pendingElicitation).
type OutstandingKind int

const (
	OutstandingPermission OutstandingKind = iota
	OutstandingElicitation
)

func (k OutstandingKind) String() string {
	switch k {
	case OutstandingPermission:
		return "permission"
	case OutstandingElicitation:
		return "elicitation"
	default:
		return "unknown"
	}
}

// outstandingPrompt holds the lifecycle metadata for one prompt awaiting user
// input. The kind-specific data (questions, answers, form state) lives in the
// per-kind stores; this carries only the requestID, kind, creation timestamp,
// and the cancel-listener fanout.
type outstandingPrompt struct {
	requestID string
	kind      OutstandingKind
	createdAt time.Time
	listeners []func(reason string)
}

// OutstandingRegistry tracks all user-input prompts awaiting a response on a
// single Backend. It unifies the lifecycle (Register / Resolve / Cancel) of
// permissions, AskUserQuestion sequences, and MCP elicitations under one
// surface, and provides a multi-listener cancel fanout so subsystems that
// display UI for a prompt can clean up when the prompt is cancelled by a
// non-user path (e.g. CC's control_cancel_request after a follow-up message
// aborts the in-flight tool).
//
// The kind-specific stores (pendingPerms, pendingElicits) keep their own
// state — the registry is the lifecycle layer, not a unified data store.
//
// All operations are thread-safe.
type OutstandingRegistry struct {
	mu      sync.Mutex
	items   map[string]*outstandingPrompt
	onEmpty func()
}

// NewOutstandingRegistry creates an empty registry.
func NewOutstandingRegistry() *OutstandingRegistry {
	return &OutstandingRegistry{
		items: make(map[string]*outstandingPrompt),
	}
}

// SetOnEmpty installs a callback fired when the last outstanding prompt is
// removed (whether by Resolve or Cancel). Pass nil to clear.
func (r *OutstandingRegistry) SetOnEmpty(fn func()) {
	r.mu.Lock()
	r.onEmpty = fn
	r.mu.Unlock()
}

// onEmptyHook returns the currently-installed onEmpty callback. Used by
// Backend.Restart to preserve the drain hook across registry replacement.
func (r *OutstandingRegistry) onEmptyHook() func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.onEmpty
}

// Register adds a new prompt under the given requestID. If a prompt with the
// same requestID already exists, it is replaced (its listeners are dropped).
func (r *OutstandingRegistry) Register(requestID string, kind OutstandingKind) {
	r.mu.Lock()
	r.items[requestID] = &outstandingPrompt{
		requestID: requestID,
		kind:      kind,
		createdAt: time.Now(),
	}
	r.mu.Unlock()
}

// AddCancelListener appends a listener for the given requestID. The listener
// fires only when Cancel is called for this requestID, not on Resolve. If no
// prompt is registered for requestID, the call is a silent no-op (caller must
// Register first).
//
// Multiple listeners may be registered for the same requestID; they fire in
// registration order. nil listeners are ignored.
func (r *OutstandingRegistry) AddCancelListener(requestID string, fn func(reason string)) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.items[requestID]
	if !ok {
		return
	}
	p.listeners = append(p.listeners, fn)
}

// Resolve removes the prompt without firing cancel listeners. Use when the
// user responded normally. Returns true if the prompt was found.
//
// If this removal empties the registry, onEmpty fires synchronously after the
// lock is released.
func (r *OutstandingRegistry) Resolve(requestID string) bool {
	r.mu.Lock()
	_, found := r.items[requestID]
	delete(r.items, requestID)
	empty := len(r.items) == 0
	onEmpty := r.onEmpty
	r.mu.Unlock()

	if found && empty && onEmpty != nil {
		onEmpty()
	}
	return found
}

// Cancel removes the prompt and fires all registered listeners with reason,
// in registration order. Listeners run with no lock held to avoid re-entrant
// deadlock. Returns true if the prompt was found.
//
// If this removal empties the registry, onEmpty fires after the listeners.
func (r *OutstandingRegistry) Cancel(requestID, reason string) bool {
	r.mu.Lock()
	p, found := r.items[requestID]
	delete(r.items, requestID)
	empty := len(r.items) == 0
	onEmpty := r.onEmpty
	r.mu.Unlock()

	if !found {
		return false
	}
	for _, fn := range p.listeners {
		fn(reason)
	}
	if empty && onEmpty != nil {
		onEmpty()
	}
	return true
}

// Has reports whether a prompt with requestID is registered.
func (r *OutstandingRegistry) Has(requestID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.items[requestID]
	return ok
}

// IsEmpty reports whether the registry has no outstanding prompts.
func (r *OutstandingRegistry) IsEmpty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.items) == 0
}

// Len returns the number of outstanding prompts.
func (r *OutstandingRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.items)
}

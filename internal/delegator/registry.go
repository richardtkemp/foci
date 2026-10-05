package delegator

import (
	"sort"
	"sync"
)

// Constructor creates a Delegator from backend-specific config.
// The config map comes from [agents.backend_config] in TOML.
type Constructor func(cfg map[string]any) (Delegator, error)

var (
	registryMu sync.Mutex
	specs      = make(map[string]Spec)
)

// Register registers a backend by its Spec — the only way a backend exists.
// Typically called from the backend package's init(). An invalid Spec is
// logged as an error and still registered (#2154 Q1: an incomplete or lying
// Spec fails the delegator/all tests, never the running gateway). A second
// registration of the same name is logged and ignored.
func Register(s Spec) {
	if err := s.Validate(); err != nil {
		delegatedLog.Errorf("invalid backend Spec: %v", err)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := specs[s.Name]; dup {
		delegatedLog.Errorf("backend %q registered twice; keeping the first registration", s.Name)
		return
	}
	specs[s.Name] = s
}

// SpecFor returns the Spec registered under name.
func SpecFor(name string) (Spec, bool) {
	registryMu.Lock()
	defer registryMu.Unlock()
	s, ok := specs[name]
	return s, ok
}

// Specs returns every registered Spec, sorted by name. Empty until the
// backend packages' init() functions have run (importing
// internal/delegator/all registers them all).
func Specs() []Spec {
	registryMu.Lock()
	defer registryMu.Unlock()
	out := make([]Spec, 0, len(specs))
	for _, s := range specs {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// New creates a Delegator by name using the registered constructor.
// Returns nil, nil if the name is not registered.
func New(name string, cfg map[string]any) (Delegator, error) {
	s, ok := SpecFor(name)
	if !ok || s.New == nil {
		return nil, nil
	}
	return s.New(cfg)
}

// IsRegistered reports whether a backend name has been registered.
func IsRegistered(name string) bool {
	_, ok := SpecFor(name)
	return ok
}

// SupportedNames returns the names of the registered backends the setup
// wizard offers (Spec.Wizard), sorted. Used to offer the live set of delegated
// backends in the /agents new wizard and the first-run setup rather than a
// hardcoded list — a newly registered backend appears automatically. Returns
// empty if no backend package is imported.
func SupportedNames() []string {
	names := []string{}
	for _, s := range Specs() {
		if s.Wizard {
			names = append(names, s.Name)
		}
	}
	return names
}

// RegisteredNames returns the names of ALL registered backends, sorted. Unlike
// SupportedNames it includes backends an agent may legally use but the setup
// wizard doesn't offer. Empty until the backend packages' init() functions
// have run (i.e. in the assembled foci-gw binary), so callers must treat an
// empty result as "registry not populated" and skip name validation rather
// than reject every backend. Used by config validation to catch a typo'd
// agent backend name early (#947).
func RegisteredNames() []string {
	all := Specs()
	names := make([]string, len(all))
	for i, s := range all {
		names[i] = s.Name
	}
	return names
}

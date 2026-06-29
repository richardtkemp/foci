package delegator

import (
	"sort"
	"sync"
)

// Constructor creates a Delegator from backend-specific config.
// The config map comes from [agents.backend_config] in TOML.
type Constructor func(cfg map[string]any) (Delegator, error)

var (
	registryMu   sync.Mutex
	constructors = make(map[string]Constructor)
	supported    = make(map[string]bool)
)

// Register registers a named backend constructor.
// Typically called from a backend package's init() function.
// supported indicates whether this backend should be offered in the setup wizard.
func Register(name string, c Constructor, isSupported bool) {
	registryMu.Lock()
	defer registryMu.Unlock()
	constructors[name] = c
	supported[name] = isSupported
}

// New creates a Delegator by name using the registered constructor.
// Returns nil, nil if the name is not registered.
func New(name string, cfg map[string]any) (Delegator, error) {
	registryMu.Lock()
	c := constructors[name]
	registryMu.Unlock()
	if c == nil {
		return nil, nil
	}
	return c(cfg)
}

// IsRegistered reports whether a backend name has been registered.
func IsRegistered(name string) bool {
	registryMu.Lock()
	defer registryMu.Unlock()
	_, ok := constructors[name]
	return ok
}

// SupportedNames returns the names of all registered backends that are marked
// as supported (i.e. suitable for presentation in setup wizards), sorted. Used
// to offer the live set of delegated backends in the /agents new wizard and the
// first-run setup rather than a hardcoded list — a newly registered backend
// appears automatically. Only populated once the backend packages' init()
// functions have run (i.e. in the assembled foci-gw binary); returns empty if
// none are imported.
func SupportedNames() []string {
	registryMu.Lock()
	defer registryMu.Unlock()
	names := make([]string, 0, len(constructors))
	for name := range constructors {
		if supported[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// RegisteredNames returns the names of ALL registered backends (supported or
// not), sorted. Unlike SupportedNames it includes backends an agent may legally
// use but the setup wizard doesn't offer (e.g. claude-code-tmux). Empty until the
// backend packages' init() functions have run (i.e. in the assembled foci-gw
// binary), so callers must treat an empty result as "registry not populated" and
// skip name validation rather than reject every backend. Used by config
// validation to catch a typo'd agent backend name early (#947).
func RegisteredNames() []string {
	registryMu.Lock()
	defer registryMu.Unlock()
	names := make([]string, 0, len(constructors))
	for name := range constructors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

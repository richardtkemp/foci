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
)

// Register registers a named backend constructor.
// Typically called from a backend package's init() function.
func Register(name string, c Constructor) {
	registryMu.Lock()
	defer registryMu.Unlock()
	constructors[name] = c
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

// RegisteredNames returns the names of all registered backends, sorted. Used
// to offer the live set of delegated backends in the /agents new wizard rather
// than a hardcoded list — a newly registered backend appears automatically.
// Only populated once the backend packages' init() functions have run (i.e. in
// the assembled foci-gw binary); returns empty if none are imported.
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

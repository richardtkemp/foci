package delegator

import "sync"

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

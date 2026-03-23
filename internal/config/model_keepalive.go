package config

import "time"

// ModelKeepaliveDefaults maps developer names to their prompt cache TTL.
// Models from these developers support automatic prompt caching with these TTLs.
var ModelKeepaliveDefaults = map[string]time.Duration{
	"openai":   5 * time.Minute,
	"deepseek": 5 * time.Minute,
}

// ResolveModelKeepalive determines whether keepalive should be enabled for a
// resolved model and what interval to use.
//
// Resolution:
//   - EnableKeepalive explicit (non-nil) → use it; nil → check developer in defaults map
//   - PromptCacheTTL explicit → parse it; empty → use defaults map
//   - Return interval = 0.95 * ttl
func ResolveModelKeepalive(resolved *ResolvedModel) (enabled bool, interval time.Duration) {
	if resolved == nil {
		return false, 0
	}

	// Determine enabled state
	if resolved.EnableKeepalive != nil {
		enabled = *resolved.EnableKeepalive
	} else {
		_, enabled = ModelKeepaliveDefaults[resolved.Developer]
	}

	if !enabled {
		return false, 0
	}

	// Determine TTL
	var ttl time.Duration
	if resolved.CacheTTL != "" {
		parsed, err := time.ParseDuration(resolved.CacheTTL)
		if err != nil || parsed <= 0 {
			return false, 0
		}
		ttl = parsed
	} else if defaultTTL, ok := ModelKeepaliveDefaults[resolved.Developer]; ok {
		ttl = defaultTTL
	} else {
		// Enabled explicitly but no TTL to derive interval from — use a safe default
		ttl = 5 * time.Minute
	}

	// 95% of TTL to fire before expiry
	interval = time.Duration(float64(ttl) * 0.95)
	return true, interval
}

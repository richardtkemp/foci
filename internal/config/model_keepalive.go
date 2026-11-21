package config

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ModelKeepaliveDefaults maps developer names to their prompt cache TTL.
// Models from these developers support automatic prompt caching with these TTLs.
// A developer whose TTL varies by model also has an entry in
// modelCacheTTLOverrides, which is consulted first.
var ModelKeepaliveDefaults = map[string]time.Duration{
	"openai":   5 * time.Minute,
	"deepseek": 5 * time.Minute,
}

// modelCacheTTLOverrides gives a developer's per-model default TTL. It returns
// ok=false when the model has no override, and the developer default applies.
var modelCacheTTLOverrides = map[string]func(modelID string) (time.Duration, bool){
	"openai": openAIModelCacheTTL,
}

// openAIGeneration matches the "gpt-<major>[.<minor>]" generation prefix of an
// OpenAI model ID, after an optional "openai/" prefix.
var openAIGeneration = regexp.MustCompile(`^(?:openai/)?gpt-(\d+)(?:\.(\d+))?`)

// openAIModelCacheTTL is the default prompt-cache lifetime of an OpenAI model.
// OpenAI's prompt-caching guide (https://platform.openai.com/docs/guides/prompt-caching,
// "Cache lifetime" and "Summary of model differences", checked 2026-11-21):
// GPT-5.6 and later have a minimum lifetime of 30 minutes by default
// (prompt_cache_options.ttl "30m"), and a cache read refreshes coverage to at
// least 30 minutes after that read. Earlier models keep entries only "around
// 5 to 10 minutes of inactivity" with in_memory retention (GPT-5.5 "typically
// around 30 minutes", with no stated minimum), so they keep the developer
// default. An ID with no generation number (an alias such as gpt-sol-latest,
// o3, gpt-oss-*) also keeps the default.
func openAIModelCacheTTL(modelID string) (time.Duration, bool) {
	m := openAIGeneration.FindStringSubmatch(strings.ToLower(modelID))
	if m == nil {
		return 0, false
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	minor := 0
	if m[2] != "" {
		if minor, err = strconv.Atoi(m[2]); err != nil {
			return 0, false
		}
	}
	if major > 5 || (major == 5 && minor >= 6) {
		return 30 * time.Minute, true
	}
	return 0, false
}

// defaultModelCacheTTL is the default prompt-cache TTL of a developer's model:
// the per-model override when one applies, else the developer default.
func defaultModelCacheTTL(developer, modelID string) (time.Duration, bool) {
	if override, ok := modelCacheTTLOverrides[developer]; ok {
		if ttl, ok := override(modelID); ok {
			return ttl, true
		}
	}
	ttl, ok := ModelKeepaliveDefaults[developer]
	return ttl, ok
}

// ResolveModelKeepalive determines whether keepalive should be enabled for a
// resolved model and what interval to use.
//
// Resolution:
//   - EnableKeepalive explicit (non-nil) → use it; nil → check developer in defaults map
//   - CacheTTL explicit → parse it; empty → per-model default (defaultModelCacheTTL)
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
	} else if defaultTTL, ok := defaultModelCacheTTL(resolved.Developer, resolved.ModelID); ok {
		ttl = defaultTTL
	} else {
		// Enabled explicitly but no TTL to derive interval from — use a safe default
		ttl = 5 * time.Minute
	}

	// 95% of TTL to fire before expiry
	interval = time.Duration(float64(ttl) * 0.95)
	return true, interval
}

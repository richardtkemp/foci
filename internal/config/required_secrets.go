package config

import (
	"fmt"
	"reflect"
	"strings"
)

// SecretRef represents a secret key that the configuration expects to exist.
type SecretRef struct {
	Key      string // secret key name, e.g. "telegram.scout", "openrouter.api_key"
	Context  string // human-readable context for warning messages
	AgentID  string // which agent this belongs to ("" for global)
	Platform bool   // true for platform secrets (telegram/discord bots)
}

// RequiredSecrets returns all secret keys that the current configuration expects
// to find in the secrets store. It combines two approaches:
//
//  1. Reflection: walks the config struct tree to find string fields with TOML
//     tags matching "secret", "*_secret", or "api_key". Non-empty values are
//     treated as explicit secret key references (the value IS the key name).
//
//  2. Conventions: derives implicit secret keys from config values where the
//     application auto-resolves secrets by naming convention (e.g. an agent
//     with telegram_bot="scout" and no bot_secret override needs "telegram.scout").
func RequiredSecrets(cfg *Config) []SecretRef {
	var refs []SecretRef

	// Phase 1: Reflection — find explicit secret references
	reflectSecretRefs(reflect.ValueOf(*cfg), "", &refs)

	// Phase 2: Convention — derive implicit secret keys
	refs = append(refs, conventionSecretRefs(cfg)...)

	// Deduplicate by key (keep first context seen)
	seen := make(map[string]bool, len(refs))
	unique := make([]SecretRef, 0, len(refs))
	for _, ref := range refs {
		if !seen[ref.Key] {
			seen[ref.Key] = true
			unique = append(unique, ref)
		}
	}
	return unique
}

// isSecretTag returns true if the TOML tag identifies a field whose value
// is a secret key name in the secrets store.
func isSecretTag(tag string) bool {
	return tag == "secret" || tag == "api_key" || strings.HasSuffix(tag, "_secret")
}

// reflectSecretRefs recursively walks a struct value looking for string fields
// whose TOML tags match isSecretTag. Non-empty values are added as explicit
// secret references.
func reflectSecretRefs(v reflect.Value, path string, refs *[]SecretRef) {
	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			fv := v.Field(i)

			tomlTag := field.Tag.Get("toml")
			if tomlTag == "" || tomlTag == "-" {
				continue
			}
			// Strip options after comma
			if idx := strings.IndexByte(tomlTag, ','); idx >= 0 {
				tomlTag = tomlTag[:idx]
			}

			fieldPath := tomlTag
			if path != "" {
				fieldPath = path + "." + tomlTag
			}

			if fv.Kind() == reflect.String && isSecretTag(tomlTag) {
				if val := fv.String(); val != "" {
					*refs = append(*refs, SecretRef{
						Key:     val,
						Context: fieldPath,
					})
				}
				continue
			}

			reflectSecretRefs(fv, fieldPath, refs)
		}

	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			elem := v.Index(i)
			elemPath := fmt.Sprintf("%s[%d]", path, i)
			// Use ID field for better context if available
			if elem.Kind() == reflect.Struct {
				if idField := elem.FieldByName("ID"); idField.IsValid() && idField.Kind() == reflect.String && idField.String() != "" {
					elemPath = fmt.Sprintf("%s[%s]", path, idField.String())
				}
			}
			reflectSecretRefs(elem, elemPath, refs)
		}

	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			elemPath := fmt.Sprintf("%s[%s]", path, iter.Key().String())
			reflectSecretRefs(iter.Value(), elemPath, refs)
		}

	case reflect.Ptr:
		if !v.IsNil() {
			reflectSecretRefs(v.Elem(), path, refs)
		}
	}
}

// conventionSecretRefs returns secret references implied by naming conventions
// rather than explicit config fields. These cover cases where a field is empty
// and the application auto-resolves the secret key by convention.
func conventionSecretRefs(cfg *Config) []SecretRef {
	var refs []SecretRef

	// --- Telegram bot tokens ---
	// Convention: agent with telegram_bot="scout" and no bot_secret override
	// needs secret "telegram.scout".
	for _, agent := range cfg.Agents {
		tg := agent.GetTelegramPlatform()
		if tg != nil && tg.Bot != "" && tg.BotSecret == "" {
			refs = append(refs, SecretRef{
				Key:      "telegram." + tg.Bot,
				Context:  fmt.Sprintf("agent %q telegram bot %q", agent.ID, tg.Bot),
				AgentID:  agent.ID,
				Platform: true,
			})
		}
		if tg != nil {
			for _, bot := range tg.FacetBots {
				refs = append(refs, SecretRef{
					Key:      "telegram." + bot,
					Context:  fmt.Sprintf("agent %q facet bot %q", agent.ID, bot),
					AgentID:  agent.ID,
					Platform: true,
				})
			}
		}
	}
	for _, bot := range cfg.Telegram.FacetBots {
		refs = append(refs, SecretRef{
			Key:      "telegram." + bot,
			Context:  fmt.Sprintf("global facet bot %q", bot),
			Platform: true,
		})
	}

	// --- Discord bot tokens ---
	// Convention: agent with discord bot="scout" and no bot_secret override
	// needs secret "discord.scout".
	for _, agent := range cfg.Agents {
		dc := agent.GetDiscordPlatform()
		if dc != nil && dc.Bot != "" && dc.BotSecret == "" {
			refs = append(refs, SecretRef{
				Key:      "discord." + dc.Bot,
				Context:  fmt.Sprintf("agent %q discord bot %q", agent.ID, dc.Bot),
				AgentID:  agent.ID,
				Platform: true,
			})
		}
	}

	// --- Endpoint API keys ---
	// Convention: endpoint "openrouter" with no api_key field needs "openrouter.api_key".
	// Skip "anthropic" — it has its own credential resolution (api_key, CC creds).
	// Resolve from model groups (powerful/fast/cheap) instead of per-agent models.
	usedEndpoints := make(map[string]bool)
	for _, groupModel := range []string{cfg.Groups.Powerful, cfg.Groups.Fast, cfg.Groups.Cheap} {
		if groupModel == "" {
			continue
		}
		resolved, err := ResolveModel(groupModel, "")
		if err != nil {
			continue
		}
		ep := resolved.Endpoint
		if ep == "anthropic" || usedEndpoints[ep] {
			continue
		}
		usedEndpoints[ep] = true

		epCfg, exists := cfg.Endpoints[ep]
		if !exists {
			continue
		}
		if epCfg.APIKey == "" {
			refs = append(refs, SecretRef{
				Key:     ep + ".api_key",
				Context: fmt.Sprintf("endpoint %q (convention)", ep),
			})
		}
	}

	// --- Brave search ---
	// If any agent effectively uses brave search, brave.api_key is needed.
	for _, agent := range cfg.Agents {
		sp := agent.SearchProvider
		if sp == "" {
			sp = cfg.Defaults.SearchProvider
		}
		if sp == "" {
			sp = cfg.Tools.SearchProvider
		}
		if sp == "brave" {
			refs = append(refs, SecretRef{
				Key:     "brave.api_key",
				Context: "brave search",
			})
			break
		}
	}

	// --- TTS providers ---
	// Convention: TTS with no explicit secret derives key from endpoint hostname.
	// edge-tts is free and needs no API key.
	for _, entry := range cfg.TTS {
		if entry.Format == "edge-tts" || entry.Secret != "" {
			continue
		}
		if key := HostnameSecretKey(entry.Endpoint); key != "" {
			refs = append(refs, SecretRef{
				Key:     key,
				Context: fmt.Sprintf("tts %q endpoint", entry.ID),
			})
		}
	}

	// --- STT providers ---
	// Convention: STT with no explicit secret derives key from endpoint hostname.
	for _, entry := range cfg.STT {
		if entry.Secret != "" {
			continue
		}
		if key := HostnameSecretKey(entry.Endpoint); key != "" {
			refs = append(refs, SecretRef{
				Key:     key,
				Context: fmt.Sprintf("stt %q endpoint", entry.ID),
			})
		}
	}

	return refs
}

// HostnameSecretKey extracts a conventional secret key name from an endpoint URL.
// It strips the scheme and path, removes "api." prefix, takes the first hostname
// segment, and appends ".api_key".
//
// Examples:
//
//	"https://api.groq.com/openai/v1" → "groq.api_key"
//	"https://openrouter.ai/api/v1"   → "openrouter.api_key"
//
// Returns "" if the URL is empty or no hostname can be extracted.
func HostnameSecretKey(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	host := endpoint
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	// Strip port: "localhost:8080" → "localhost"
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	host = strings.TrimPrefix(host, "api.")
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	if host == "" {
		return ""
	}
	return host + ".api_key"
}

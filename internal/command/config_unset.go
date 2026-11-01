package command

import (
	"errors"
	"fmt"
	"strings"

	"foci/internal/config"
)

// This file backs /config unset (#2716): remove an explicitly-written key
// from foci.toml so the field reverts to what it inherits — the global
// value for an agent override, else the built-in default. It is the chat
// counterpart of the app config editor's config.unset, and the inverse of
// /config set: the same comment-preserving writer (config.UnsetInFile,
// through deps.UnsetInFileFn, which also runs the live applier), the same
// field lookup, and the same per-agent rules:
//
//	unset <section>.<key>  — remove a global key (agent.<key> → this chat's agent block)
//	unset <agent> <key>    — remove a NAMED agent's per-agent override (live fields only)

// configUnsetUsage is the reply for a bare or malformed /config unset.
func configUnsetUsage() string {
	return "Usage:\n" +
		"  /config unset <section>.<key> — remove a key from foci.toml (it reverts to its default)\n" +
		"  /config unset <agent> <key> — remove a named agent's per-agent override (it reverts to the global value or default)"
}

// configUnset dispatches the two unset forms by argument count. User-input
// refusals (unknown field or agent, key not written in the file) return an
// explanatory reply with a nil error, as the set forms do; only a failed
// file read/write returns an error.
func configUnset(deps *ConfigSetDeps, args string) (string, error) {
	if deps.UnsetInFileFn == nil {
		return "Config unset is not available.", nil
	}
	parts := strings.Fields(args)
	switch len(parts) {
	case 1:
		return configUnsetDirect(deps, parts[0])
	case 2:
		return configUnsetAgent(deps, parts[0], parts[1])
	default:
		return configUnsetUsage(), nil
	}
}

// configUnsetDirect handles "unset <section>.<key>". The whole path is
// looked up first, exactly as ConfigSetDirect does, so a dotted key or a
// dotted map section is split where the registry says, not at the first
// dot. Section "agent" targets this chat's own [[agents]] block.
func configUnsetDirect(deps *ConfigSetDeps, path string) (string, error) {
	if !strings.Contains(path, ".") {
		return configUnsetUsage(), nil
	}
	field, ok := deps.LookupFn(path)
	if !ok {
		return fmt.Sprintf("Unknown config field %q. Use /config available to list fields.", path), nil
	}
	section, key := field.Section, field.Key

	target := config.SetTarget{Section: section, Key: key}
	if section == "agent" {
		target = config.SetTarget{Section: "agents", AgentID: deps.AgentID, Key: key}
	}

	name := section + "." + key
	old, err := deps.UnsetInFileFn(deps.ConfigPath, target)
	if errors.Is(err, config.ErrKeyNotSet) {
		return notSetMsg(name), nil
	}
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Unset %s (was %s).", name, old)
	if section == "agent" {
		sb.WriteString(" It now inherits the global value or the default.")
	} else if field.Default != "" {
		fmt.Fprintf(&sb, " It now uses the default: %s.", field.Default)
	} else {
		sb.WriteString(" It now uses the built-in default.")
	}
	if field.NeedsRestart {
		sb.WriteString("\nRestart to take effect.")
	} else {
		sb.WriteString("\nApplied live.")
	}
	return sb.String(), nil
}

// configUnsetAgent handles "unset <agent> <key>": remove the named agent's
// override. It applies the per-agent set's rules (configSetAgent) — the
// agent must be running, the key must have a per-agent registry row, and a
// restart-only field is refused, because the cross-agent forms are
// live-only. The reply reports the value the agent now resolves to, read
// back from the file through the ladder /config get uses (file rungs only).
func configUnsetAgent(deps *ConfigSetDeps, agentID, rawKey string) (string, error) {
	if strings.ContainsAny(agentID, ".=") {
		return configUnsetUsage(), nil
	}
	if msg, valid := validateAgentID(deps, agentID); !valid {
		return msg, nil
	}
	key := agentRegistryKey(rawKey)
	field, ok := deps.LookupFn("agent." + key)
	if !ok {
		return notPerAgentMsg(key), nil
	}
	if field.NeedsRestart {
		return needsRestartAgentMsg(key), nil
	}

	name := agentID + " " + field.Key
	old, err := deps.UnsetInFileFn(deps.ConfigPath, config.SetTarget{
		Section: "agents",
		AgentID: agentID,
		Key:     field.Key,
	})
	if errors.Is(err, config.ErrKeyNotSet) {
		return notSetMsg(name), nil
	}
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Unset %s (was %s).", name, old)
	if global, agents, rerr := config.ExplicitFileValues(deps.ConfigPath); rerr == nil {
		// No running rung: the running config is frozen at startup, so it
		// may still hold the very override just removed. With the file
		// rungs and the built-in default only, the reply names what the
		// agent resolves to once the override is gone.
		value, source := config.ResolveAgentValue(global, agents, agentID, field, nil)
		if value == "" {
			value = "(not set)"
		}
		fmt.Fprintf(&sb, " Now %s (%s).", value, source)
	}
	sb.WriteString("\nApplied live.")
	return sb.String(), nil
}

// notSetMsg is the reply when the key has no explicit value in foci.toml:
// nothing was written, and the field already uses what it inherits.
func notSetMsg(name string) string {
	return fmt.Sprintf("%s is not set in foci.toml, so there is nothing to unset (it already uses its inherited or default value).", name)
}

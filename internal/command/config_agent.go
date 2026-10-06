package command

import (
	"fmt"
	"strings"

	"foci/internal/config"
)

// This file backs the per-agent forms of /config (#1212):
//
//	config set <agent> <key>=<value>  — write a NAMED agent's live value
//	config get <agent> [<key>]        — read a named agent's value
//
// Both address the per-agent field registry (Section "agent") — the key is
// spelled as it appears under [[agents]] (e.g. keepalive.interval). Reads
// accept any agent row; writes only live-appliable (hot) ones, because the
// write path's live-apply callback is what makes a cross-agent edit safe to
// advertise: a restart-only field would land in the file but not take
// effect, so it is refused with a pointer at foci.toml instead.

// agentRegistryKey normalizes a per-agent key the user typed: trimmed, with
// an optional "agent." prefix stripped, so "agent.keepalive.interval" and
// "keepalive.interval" name the same field.
func agentRegistryKey(key string) string {
	return strings.TrimPrefix(strings.TrimSpace(key), "agent.")
}

// splitAgentSetArgs recognizes the "set <agent> <key>=<value>" shape: a
// first whitespace-delimited token that is a bare agent id (no "." and no
// "=" — both would identify it as a registry path instead), followed by a
// remainder containing "=". It splits on the FIRST whitespace run only and
// leaves the remainder's interior spacing alone, so a value may contain
// meaningful double spaces (its ends are trimmed later, in configSetAgent).
// Every other shape returns ok=false, so configSet's existing direct,
// keyboard and wizard branches see their inputs untouched.
func splitAgentSetArgs(args string) (agentID, rest string, ok bool) {
	args = strings.TrimLeft(args, " \t")
	end := strings.IndexAny(args, " \t")
	if end <= 0 {
		return "", "", false
	}
	agentID = args[:end]
	if strings.ContainsAny(agentID, ".=") {
		return "", "", false
	}
	rest = strings.TrimLeft(args[end:], " \t")
	if !strings.Contains(rest, "=") {
		return "", "", false
	}
	return agentID, rest, true
}

// validateAgentID reports whether agentID is one of the running agents
// (exact match, per AgentIDsFn). The returned message is the refusal reply
// when it is not; a nil AgentIDsFn yields an honest empty list rather than
// a bypass.
func validateAgentID(deps *ConfigSetDeps, agentID string) (string, bool) {
	var ids []string
	if deps.AgentIDsFn != nil {
		ids = deps.AgentIDsFn()
	}
	for _, id := range ids {
		if id == agentID {
			return "", true
		}
	}
	if len(ids) == 0 {
		return fmt.Sprintf("Unknown agent %q. No running agents found.", agentID), false
	}
	return fmt.Sprintf("Unknown agent %q. Running agents: %s.", agentID, strings.Join(ids, ", ")), false
}

// notPerAgentMsg is the reply for a key with no agent.<key> registry row at
// all — a field whose scope excludes agents (e.g. debug.enable_pprof) or a
// key that is not a registry field (e.g. backend). Deliberately distinct
// from needsRestartAgentMsg so the two refusals cannot be confused.
func notPerAgentMsg(key string) string {
	return fmt.Sprintf("%s is not a per-agent setting. Use /config set <section>.<key>=<value> for the global value, or /config available to list fields.", key)
}

// needsRestartAgentMsg is the reply for a real per-agent field that only
// takes effect after a restart: the per-agent set is live-only, so it
// refuses rather than writing a value that would silently not apply.
func needsRestartAgentMsg(key string) string {
	return fmt.Sprintf("%s needs a restart to take effect, so it cannot be set per agent from chat. Edit foci.toml directly and restart to change it.", key)
}

// configSetAgent handles "set <agent> <key>=<value>": writes value into the
// named agent's [[agents]] block and reports that it applied live. All
// user-input refusals (unknown agent, not a per-agent setting, restart-only
// field, invalid value) return an explanatory reply with a nil error — the
// explanation is what the chat should show; only a failed file write
// returns an error, mirroring ConfigSetDirect.
func configSetAgent(deps *ConfigSetDeps, agentID, rest string) (string, error) {
	if msg, valid := validateAgentID(deps, agentID); !valid {
		return msg, nil
	}

	// The value is everything after the FIRST "=", trimmed, so it may
	// contain spaces and further "=" characters.
	key, rawValue, _ := strings.Cut(rest, "=")
	key = agentRegistryKey(key)
	rawValue = strings.TrimSpace(rawValue)
	if key == "" || rawValue == "" {
		return fmt.Sprintf("Expected <agent> <key>=<value>, e.g. /config set %s keepalive.interval=1m", agentID), nil
	}

	field, ok := deps.LookupFn("agent." + key)
	if !ok {
		return notPerAgentMsg(key), nil
	}
	if field.NeedsRestart {
		return needsRestartAgentMsg(key), nil
	}

	// Format and validate exactly as ConfigSetDirect does (format first,
	// then constraint) — and on the canonical registry key, so casing
	// variants of a key write the registry's spelling.
	formatted, err := config.FormatTOMLValue(rawValue, field.Type)
	if err != nil {
		return fmt.Sprintf("Invalid value: %s", err), nil
	}
	if err := field.ValidateValue(rawValue); err != nil {
		return fmt.Sprintf("Invalid value: %s", err), nil
	}

	oldValue, err := deps.SetInFileFn(deps.ConfigPath, config.SetTarget{
		Section: "agents",
		AgentID: agentID,
		Key:     field.Key,
	}, formatted)
	if err != nil {
		return "", err
	}

	// Always "Applied live": the hot gate above refused everything the
	// live-apply path does not cover, and SetInFileFn's write callback
	// re-applies ALL agents from the freshly loaded file.
	var sb strings.Builder
	sb.WriteString(formatSetLine(agentID+" "+field.Key, formatted, oldValue))
	sb.WriteString("\nApplied live.")
	return sb.String(), nil
}

// configGet handles the /config get subcommand:
//
//	get               → usage
//	get <agent>       → the per-agent keys the per-agent set accepts (hot ones)
//	get <agent> <key> → the agent's current value for one key, with its source
//
// Values are read from the config FILE at call time, not the frozen startup
// config, so a value just written by the per-agent set is what get shows.
func configGet(deps *ConfigSetDeps, args string) (string, error) {
	parts := strings.Fields(args)
	switch len(parts) {
	case 1:
		return configGetKeyList(deps, parts[0]), nil
	case 2:
		return configGetAgentKey(deps, parts[0], parts[1])
	default: // no args, or too many
		return configGetUsage(), nil
	}
}

func configGetUsage() string {
	return "Usage:\n" +
		"  /config get <agent> <key> — read an agent's current value for a per-agent key\n" +
		"  /config get <agent> — list the per-agent keys that can be set live"
}

// configGetKeyList replies with the per-agent keys the per-agent set
// accepts — the hot rows of the agent section, in registry (sorted) order.
func configGetKeyList(deps *ConfigSetDeps, agentID string) string {
	if msg, valid := validateAgentID(deps, agentID); !valid {
		return msg
	}
	var keys []string
	for _, f := range deps.FieldsInSection("agent") {
		if !f.NeedsRestart {
			keys = append(keys, f.Key)
		}
	}
	return fmt.Sprintf("Live-settable keys for %s (read one with /config get %s <key>):\n%s",
		agentID, agentID, strings.Join(keys, ", "))
}

// configGetAgentKey replies with one agent's current value for one key and
// where it comes from. Any agent.<key> registry row is readable — hot or
// not, reading is harmless — and the reply states whether the field can
// also be set live.
func configGetAgentKey(deps *ConfigSetDeps, agentID, key string) (string, error) {
	if msg, valid := validateAgentID(deps, agentID); !valid {
		return msg, nil
	}
	key = agentRegistryKey(key)
	field, ok := deps.LookupFn("agent." + key)
	if !ok {
		return notPerAgentMsg(key), nil
	}

	global, agents, err := config.ExplicitFileValues(deps.ConfigPath)
	if err != nil {
		return fmt.Sprintf("Failed to read config file: %s", err), nil
	}
	value, source := agentFileValue(global, agents, agentID, field)

	display := value
	if display == "" {
		display = "(not set)"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s = %s\nSource: %s", agentID, field.Key, display, source)
	if field.NeedsRestart {
		sb.WriteString("\nNeeds a restart to change (edit foci.toml directly).")
	} else {
		fmt.Fprintf(&sb, "\nSettable live: /config set %s %s=<value>", agentID, field.Key)
	}
	return sb.String(), nil
}

// agentFileValue resolves one agent's effective value for field from the
// config FILE at call time, using the same ladder as the app config editor:
// an explicit [[agents]] override, else the inherited global section value,
// else the built-in default. File-only on purpose — the startup config is
// frozen, so it would go stale the moment a set lands.
func agentFileValue(global map[string]string, agents map[string]map[string]string, agentID string, field config.ConfigField) (value, source string) {
	if v, ok := agents[agentID][field.Key]; ok {
		return v, fmt.Sprintf("set for %s in foci.toml", agentID)
	}
	if prefix, rest, dotted := strings.Cut(field.Key, "."); dotted {
		if gs, mapped := config.AgentGlobalSections()[prefix]; mapped {
			if v, ok := global[gs+"."+rest]; ok {
				return v, fmt.Sprintf("inherited from [%s] in foci.toml", gs)
			}
		}
	}
	return field.Default, "built-in default"
}

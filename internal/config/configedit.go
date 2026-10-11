package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// This file backs the app protocol's config editing (wire §13): enumerating every
// registry field with its file state ("explicitly set" vs default), and removing a
// key so it reverts to the inherited/default value. SetInFile (setinfile.go) is the
// write half; both share the same line-oriented, comment-preserving conventions.

// AllFields returns the full registry (sorted by section then key).
func AllFields() []ConfigField {
	out := make([]ConfigField, len(configFields))
	copy(out, configFields)
	return out
}

// TypeName renders a FieldType as its wire name ("string", "int", "float",
// "bool", "duration", "string[]", "schedule").
func (t FieldType) TypeName() string {
	switch t {
	case FieldInt:
		return "int"
	case FieldFloat:
		return "float"
	case FieldBool:
		return "bool"
	case FieldDuration:
		return "duration"
	case FieldStringList:
		return "string[]"
	case FieldSchedule:
		return "schedule"
	default:
		return "string"
	}
}

// UnsetInFile removes an explicitly-set key from the TOML config file (the
// value reverts to whatever the key inherits: the global value for an agent
// override, else the built-in default). Same surgical, comment-preserving
// line editing as SetInFile; the removed value (all its lines) is returned. A key that
// isn't present is an error — there is nothing to unset.
func UnsetInFile(path string, target SetTarget, mode os.FileMode) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read config: %w", err)
	}

	lines := strings.Split(string(data), "\n")

	line := -1
	if target.Section == "agents" {
		loc, err := locateAgentKey(lines, target.AgentID, target.Key)
		if err != nil {
			return "", err
		}
		line = loc.found
	} else {
		from, to := findSectionBounds(lines, target.Section)
		if from < 0 {
			return "", fmt.Errorf("section [%s] not found in config file", target.Section)
		}
		active := keyLineRe(target.Key)
		top := topLevel(lines)
		for i := from + 1; i < to; i++ {
			if top[i] && active.MatchString(lines[i]) {
				line = i
				break
			}
		}
	}
	if line < 0 {
		return "", fmt.Errorf("%s is not set in the config file", target.Key)
	}

	end, old, err := valueSpan(lines, line)
	if err != nil {
		return "", err
	}
	lines = replaceSpan(lines, line, end, "")

	output := strings.Join(lines, "\n")
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(output), mode); err != nil {
		return "", fmt.Errorf("write temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath) // best effort cleanup
		return "", fmt.Errorf("rename: %w", err)
	}
	return old, nil
}

// ExplicitFileValues re-parses the config FILE (not the running config) and
// returns every explicitly-written scalar value: global as "section.key" →
// value, and per-agent (the [[agents]] blocks, keyed by id) as dotted key →
// value. Values render in display form (unquoted strings, "true"/"false",
// plain numbers) — the same form users type into the editor. Re-parsing per
// call keeps the result truthful after SetInFile/UnsetInFile edits, which the
// running config won't reflect until restart.
func ExplicitFileValues(path string) (global map[string]string, agents map[string]map[string]string, err error) {
	var raw map[string]any
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return nil, nil, fmt.Errorf("parse config: %w", err)
	}

	global = map[string]string{}
	agents = map[string]map[string]string{}

	for section, v := range raw {
		if section == "agents" {
			blocks, ok := v.([]map[string]any)
			if !ok {
				// BurntSushi may also produce []any of maps depending on input.
				if anyBlocks, ok2 := v.([]any); ok2 {
					for _, ab := range anyBlocks {
						if m, ok3 := ab.(map[string]any); ok3 {
							blocks = append(blocks, m)
						}
					}
				}
			}
			for _, block := range blocks {
				id, _ := block["id"].(string)
				if id == "" {
					continue
				}
				vals := map[string]string{}
				flattenScalars("", block, vals)
				delete(vals, "id")
				agents[id] = vals
			}
			continue
		}
		if m, ok := v.(map[string]any); ok {
			flattenScalars(section, m, global)
		} else if s, ok := renderScalar(v); ok {
			global[section] = s // top-level scalar key (e.g. data_dir)
		}
	}
	return global, agents, nil
}

// MapObjectEntries extracts named entries from the flattened file values for
// a map[string]Struct section (e.g. [models.powerful]). Returns entryName →
// subKey → value. Each entry is a direct child of the section prefix; deeper
// nesting (e.g. models.powerful.model) becomes a sub-key.
func MapObjectEntries(section string, flat map[string]string) map[string]map[string]string {
	prefix := section + "."
	out := map[string]map[string]string{}
	for k, v := range flat {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		parts := strings.SplitN(rest, ".", 2)
		if len(parts) != 2 {
			continue // section-level scalar, not an entry sub-key
		}
		entry, subKey := parts[0], parts[1]
		if out[entry] == nil {
			out[entry] = map[string]string{}
		}
		out[entry][subKey] = v
	}
	return out
}

// flattenScalars walks nested TOML tables, writing "prefix.key" → rendered
// value for every scalar leaf. Arrays/inline-table leaves are skipped — the
// field registry only covers scalars, so they can never be edited here.
func flattenScalars(prefix string, m map[string]any, out map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		full := k
		if prefix != "" {
			full = prefix + "." + k
		}
		switch v := m[k].(type) {
		case map[string]any:
			flattenScalars(full, v, out)
		default:
			if s, ok := renderScalar(v); ok {
				out[full] = s
			}
		}
	}
}

// renderScalar renders a decoded TOML scalar in display form; ok=false for
// non-scalars (arrays, table arrays).
func renderScalar(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		if t {
			return "true", true
		}
		return "false", true
	case int64:
		return fmt.Sprintf("%d", t), true
	case float64:
		return fmt.Sprintf("%g", t), true
	case []any:
		// A string array (e.g. auto_approve): render as a JSON array so the
		// FieldStringList editor can parse it into chips. Non-string elements
		// aren't settable yet, so skip (ok=false).
		items := make([]string, 0, len(t))
		for _, e := range t {
			s, isStr := e.(string)
			if !isStr {
				return "", false
			}
			items = append(items, s)
		}
		b, err := json.Marshal(items)
		if err != nil {
			return "", false
		}
		return string(b), true
	default:
		return "", false
	}
}

// TableArrayEntries reads the [[section]] array-of-tables blocks for spec's
// (possibly dotted) section from the TOML file at path, returning one
// map[subfield]value per block. Only sub-keys declared in spec.Fields are
// included, so a stray or unknown key in the file never reaches the client.
// Values keep their decoded Go types (string, float64, int64, bool) so a later
// json.Marshal produces the natural JSON shape the editor expects. An absent
// section yields an empty (non-nil) slice, not an error.
func TableArrayEntries(path string, spec ObjectFieldSpec) ([]map[string]any, error) {
	var raw map[string]any
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	parts := strings.Split(spec.Section, ".")
	cur := raw
	for _, p := range parts[:len(parts)-1] {
		m, ok := cur[p].(map[string]any)
		if !ok {
			return []map[string]any{}, nil // parent table absent
		}
		cur = m
	}
	blocks := toTableArrayBlocks(cur[parts[len(parts)-1]])
	allowed := map[string]bool{}
	for _, sf := range spec.Fields {
		allowed[sf.Key] = true
	}
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		entry := map[string]any{}
		for k, v := range b {
			if allowed[k] {
				entry[k] = v
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// toTableArrayBlocks normalizes a decoded array-of-tables value to
// []map[string]any. BurntSushi may yield either []map[string]any or []any of
// maps depending on input; anything else (or nil) yields no blocks.
func toTableArrayBlocks(v any) []map[string]any {
	switch t := v.(type) {
	case []map[string]any:
		return t
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

// ParseObjectListValue decodes the editor's JSON array-of-objects into the
// []map[string]any that SetTableArray writes, coercing each sub-value to the Go
// type its ObjectSubField declares (so SetTableArray's formatter emits the right
// TOML). Unknown sub-keys and type mismatches are rejected.
func ParseObjectListValue(spec ObjectFieldSpec, jsonStr string) ([]map[string]any, error) {
	var raw []map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil, fmt.Errorf("invalid object list: %w", err)
	}
	types := map[string]FieldType{}
	for _, sf := range spec.Fields {
		types[sf.Key] = sf.Type
	}
	out := make([]map[string]any, 0, len(raw))
	for i, obj := range raw {
		entry := map[string]any{}
		for k, v := range obj {
			ft, ok := types[k]
			if !ok {
				return nil, fmt.Errorf("entry %d: unknown field %q for [[%s]]", i+1, k, spec.Section)
			}
			cv, err := coerceObjectValue(v, ft)
			if err != nil {
				return nil, fmt.Errorf("entry %d field %q: %w", i+1, k, err)
			}
			entry[k] = cv
		}
		out = append(out, entry)
	}
	return out, nil
}

// coerceObjectValue converts a JSON-decoded value (string, float64, bool) to the
// Go type SetTableArray's formatter expects for the given FieldType.
func coerceObjectValue(v any, ft FieldType) (any, error) {
	switch ft {
	case FieldString, FieldDuration, FieldSchedule:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("expected a string")
		}
		return s, nil
	case FieldFloat:
		f, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("expected a number")
		}
		return f, nil
	case FieldInt:
		f, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("expected an integer")
		}
		return int64(f), nil
	case FieldBool:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("expected a boolean")
		}
		return b, nil
	default:
		return nil, fmt.Errorf("unsupported field type")
	}
}

// agentGlobalSections is the memoized AgentGlobalSections computation: a
// pure function of compile-time struct types, so it runs once per process.
// ResolveAgentValue consults the mapping per (agent × field), which would
// otherwise repeat this reflection walk on every schema build.
var agentGlobalSections = sync.OnceValue(func() map[string]string {
	byType := map[string]string{}
	for section, typ := range globalSections {
		byType[typ.String()] = section
	}
	out := map[string]string{}
	agentType := reflect.TypeOf(AgentConfig{})
	for i := 0; i < agentType.NumField(); i++ {
		f := agentType.Field(i)
		tag := extractTOMLTag(f)
		if tag == "" {
			continue
		}
		ft := f.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() != reflect.Struct {
			continue
		}
		if section, ok := byType[ft.String()]; ok {
			out[tag] = section
			continue
		}
		// Name fallback: per-agent OVERRIDE structs (AgentSessionsOverride,
		// AgentToolsOverride) share the global section's NAME and field keys but
		// not its Go type — map sessions→sessions, tools→tools by tag.
		if _, ok := globalSections[tag]; ok {
			out[tag] = tag
		}
	}
	return out
})

// AgentGlobalSections maps an agent-override key prefix (the AgentConfig
// field's TOML tag, e.g. "loop") to the GLOBAL registry section holding the
// same struct type (e.g. "agent_loop") — the section an unset agent override
// inherits from. Computed by matching struct types, so it can't drift from
// the registry. The result is memoized; every call returns a fresh copy, so
// callers cannot mutate the shared cache.
func AgentGlobalSections() map[string]string {
	out := agentGlobalSections()
	cp := make(map[string]string, len(out))
	maps.Copy(cp, out)
	return cp
}

// ResolveAgentValue resolves one agent's effective value for a registry
// field through the ONE agent-value ladder, shared by /config get and the
// app config editor: an explicit [[agents]] value in foci.toml, else the
// inherited global section value in foci.toml, else the running value, else
// the field's built-in default. global and agents are the flattened file
// values from ExplicitFileValues; running supplies the running-config rung
// (nil skips it) — AgentRunningValue is that rung's one implementation, so
// every caller feeds in the same running value. Pure; no I/O.
//
// Semantics, so the two callers cannot drift again:
//   - "Explicit" on the agent rung means the key is PRESENT in the agent's
//     [[agents]] block — a present empty string wins (and is returned).
//   - A present-but-empty GLOBAL value falls through to the later rungs,
//     and the running rung sits between the file and the default. Where the
//     two ladders this replaces differed (both of those cases), the shared
//     behaviour is the app editor's.
//
// The returned source names the rung that produced the value; only the
// editor's use discards it (its wire format carries values, not sources).
func ResolveAgentValue(global map[string]string, agents map[string]map[string]string, agentID string, field ConfigField, running func() string) (value, source string) {
	if v, ok := agents[agentID][field.Key]; ok {
		return v, fmt.Sprintf("set for %s in foci.toml", agentID)
	}
	if prefix, rest, dotted := strings.Cut(field.Key, "."); dotted {
		if gs, mapped := agentGlobalSections()[prefix]; mapped {
			if v, ok := global[gs+"."+rest]; ok && v != "" {
				return v, fmt.Sprintf("inherited from [%s] in foci.toml", gs)
			}
		}
	}
	if running != nil {
		if v := running(); v != "" {
			return v, "running config (not set in foci.toml)"
		}
	}
	return field.Default, "built-in default"
}

// AgentRunningValue returns the running value one agent effectively runs
// for a registry key (registry spelling, e.g. maintenance.reset_time): the
// mapped global section's running value first, else the agent's own
// running row. The global comes first because override fields
// (sessions.*, tools.*) carry their defaults only on the global struct —
// an unset override row reads "" — so the row alone cannot answer "what is
// this agent running?"; the row then covers keys whose prefix maps to no
// global section, and globals that run empty. This is the running rung
// ResolveAgentValue asks its running fn for; /config get and the app
// config editor both pass it, so the two surfaces cannot disagree.
//
// The file's say is settled before this runs: the resolver reaches the
// running rung only when the key is in neither the agent block nor the
// global section (a present-but-empty global counts as unset). Pure; no
// I/O.
func AgentRunningValue(cfg *Config, a AgentConfig, key string) string {
	if prefix, rest, dotted := strings.Cut(key, "."); dotted {
		if gs, mapped := agentGlobalSections()[prefix]; mapped {
			if v := LookupValue(cfg, AgentConfig{}, gs, rest); v != "" {
				return v
			}
		}
	}
	return LookupValue(cfg, a, "agent", key)
}

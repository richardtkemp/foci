package config

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

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
// "bool", "duration").
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
	default:
		return "string"
	}
}

// UnsetInFile removes an explicitly-set key from the TOML config file (the
// value reverts to whatever the key inherits: the global value for an agent
// override, else the built-in default). Same surgical, comment-preserving
// line editing as SetInFile; the removed line's value is returned. A key that
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
		for i := from + 1; i < to; i++ {
			if active.MatchString(lines[i]) {
				line = i
				break
			}
		}
	}
	if line < 0 {
		return "", fmt.Errorf("%s is not set in the config file", target.Key)
	}

	old := extractValue(lines[line])
	lines = append(lines[:line], lines[line+1:]...)

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

// AgentGlobalSections maps an agent-override key prefix (the AgentConfig
// field's TOML tag, e.g. "loop") to the GLOBAL registry section holding the
// same struct type (e.g. "agent_loop") — the section an unset agent override
// inherits from. Computed by matching struct types, so it can't drift from
// the registry.
func AgentGlobalSections() map[string]string {
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
}

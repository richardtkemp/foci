package config

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// validateTaggedDurations is the tag-driven backstop behind Validate's
// hand-written duration table and special cases (which keep precedence by
// running first): it walks the loaded config by reflection and requires every
// string or *string field tagged `type:"duration"` — wherever it sits: global
// sections, structs reached through pointers, embedded structs, map values
// ([models.<name>]), every [[agents]] block, and every [[platforms]] entry,
// global and per agent — to hold a value time.ParseDuration accepts.
//
// Empty values and nil pointers are skipped: several duration fields document
// "empty = <default>" or "empty disables it", and per-agent pointer fields
// stay nil to inherit. "0" is a valid duration and stays valid. Disabled
// [tracing] and [bitwarden] sections are skipped entirely — the documented
// rule that an unused endpoint/timeout typo shouldn't block startup.
func (cfg *Config) validateTaggedDurations() error {
	// Globals first, then agents, so a config with bad values in both scopes
	// reports the global one — mirroring validateQuietCompaction's order.
	if err := walkTaggedDurations(reflect.ValueOf(cfg).Elem(), durationScope{}); err != nil {
		return err
	}
	for _, a := range cfg.Agents {
		s := durationScope{agentID: a.ID, agentScoped: true}
		if err := walkTaggedDurations(reflect.ValueOf(a), s); err != nil {
			return err
		}
	}
	return nil
}

// configType gates the walk's Config-only exceptions so look-alike fields
// nested inside other sections are unaffected.
var configType = reflect.TypeOf(Config{})

// durationScope names where the walk currently is: the agent id when inside
// an [[agents]] block (agentScoped keeps a block with an empty id from being
// mistaken for a global section), and the dotted TOML table path relative to
// that scope ("" at the top of a block).
type durationScope struct {
	agentID     string
	agentScoped bool
	table       string
}

// child returns s extended by one more table segment.
func (s durationScope) child(seg string) durationScope {
	if s.table == "" {
		s.table = seg
	} else {
		s.table += "." + seg
	}
	return s
}

// where renders the location of key: `agent "<id>" [<table>] <key>`, each
// part omitted when empty — the shape validateQuietCompaction's errors use.
func (s durationScope) where(key string) string {
	var sb strings.Builder
	if s.agentScoped {
		fmt.Fprintf(&sb, "agent %q ", s.agentID)
	}
	if s.table != "" {
		fmt.Fprintf(&sb, "[%s] ", s.table)
	}
	sb.WriteString(key)
	return sb.String()
}

// walkTaggedDurations recurses over v's struct fields in declaration order
// (deterministic), checking the duration-tagged leaves it meets. Only
// read-only reflection is used (.String/.Bool/.Elem), never .Interface, so
// unexported fields are safe.
func walkTaggedDurations(v reflect.Value, s durationScope) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		fv := v.Field(i)

		// Embedded struct: its fields belong to this same table, with or
		// without a toml tag on the embedding ([sessions]
		// compaction_quiet_min_idle, [tools] tmux_watch_threshold).
		if f.Anonymous {
			if sv := derefStruct(fv); sv.IsValid() {
				if err := walkTaggedDurations(sv, s); err != nil {
					return err
				}
			}
			continue
		}

		tag := extractTOMLTag(f)
		if tag == "" {
			continue // no usable TOML key (covers toml:"-" bookkeeping fields)
		}

		// Config-only exceptions, gated on the exact type:
		//   - Agents is skipped here; validateTaggedDurations walks each
		//     block itself so its errors carry the agent id.
		//   - Tracing/Bitwarden are skipped while disabled, keeping
		//     validateTracing's and the duration table's enabled-only rule.
		if t == configType {
			switch f.Name {
			case "Agents":
				continue
			case "Tracing", "Bitwarden":
				if !fieldBool(fv, "Enabled") {
					continue
				}
			}
		}

		// Duration leaf.
		if f.Tag.Get("type") == "duration" {
			if val, ok := durationValue(fv); ok {
				if _, err := time.ParseDuration(val); err != nil {
					return fmt.Errorf("%s = %q: %w", s.where(tag), val, err)
				}
			}
			continue
		}

		// Named struct (or pointer to one): a nested table.
		if sv := derefStruct(fv); sv.IsValid() {
			if err := walkTaggedDurations(sv, s.child(tag)); err != nil {
				return err
			}
			continue
		}

		// Map of structs ([models.<name>]): keys in sorted order, so the
		// reported error is the same on every run.
		if f.Type.Kind() == reflect.Map && f.Type.Key().Kind() == reflect.String {
			keys := make([]string, 0, fv.Len())
			for _, k := range fv.MapKeys() {
				keys = append(keys, k.String())
			}
			sort.Strings(keys)
			for _, k := range keys {
				sv := derefStruct(fv.MapIndex(reflect.ValueOf(k).Convert(f.Type.Key())))
				if !sv.IsValid() {
					continue
				}
				if err := walkTaggedDurations(sv, s.child(tag).child(k)); err != nil {
					return err
				}
			}
			continue
		}

		// Array-of-tables ([[platforms]]): each element is named
		// <tag>.<ID> from the element's ID string field, else <tag>.<index>
		// (no ID-less slice carries a duration today; the index keeps the
		// walk total). AgentConfig also has an ID but never reaches here —
		// the Config-level exception above owns the agent blocks.
		if f.Type.Kind() == reflect.Slice {
			for idx := 0; idx < fv.Len(); idx++ {
				sv := derefStruct(fv.Index(idx))
				if !sv.IsValid() {
					continue
				}
				seg := strconv.Itoa(idx)
				if id := sv.FieldByName("ID"); id.Kind() == reflect.String {
					seg = id.String()
				}
				if err := walkTaggedDurations(sv, s.child(tag).child(seg)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// durationValue returns the string a duration-tagged field holds and whether
// it holds one at all: only string and non-nil *string fields qualify, and an
// empty string counts as unset ("empty = <default>" / "empty disables it").
func durationValue(fv reflect.Value) (string, bool) {
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return "", false
		}
		fv = fv.Elem()
	}
	if fv.Kind() != reflect.String || fv.String() == "" {
		return "", false
	}
	return fv.String(), true
}

// derefStruct returns the struct value behind v (following one pointer), or
// the zero Value when v is a nil pointer or not a struct at all.
func derefStruct(v reflect.Value) reflect.Value {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	return v
}

// fieldBool reads struct value v's bool field name (false when absent,
// mirroring Go's zero value).
func fieldBool(v reflect.Value, name string) bool {
	f := v.FieldByName(name)
	return f.IsValid() && f.Kind() == reflect.Bool && f.Bool()
}

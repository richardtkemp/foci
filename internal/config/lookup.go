package config

import (
	"fmt"
	"reflect"
	"strings"
)

// LookupValue returns the effective (running) value of a config field as a
// display string. For section "agent", the value comes from the AgentConfig;
// for all other sections, it comes from the matching Config sub-struct.
// Dotted keys (e.g. "keepalive.enabled") are resolved through nested structs.
// Returns "" if the section or key is not found.
func LookupValue(cfg *Config, agent AgentConfig, section, key string) string {
	var target reflect.Value

	if section == "agent" || section == "agents" {
		target = reflect.ValueOf(agent)
	} else {
		target = findSection(reflect.ValueOf(*cfg), section)
	}
	if !target.IsValid() {
		return ""
	}

	v := walkTOMLPath(target, key)
	if !v.IsValid() {
		return ""
	}
	return reflectToDisplay(v)
}

// findSection looks up a struct field whose TOML tag matches the section name.
func findSection(cfgVal reflect.Value, section string) reflect.Value {
	t := cfgVal.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := tomlTagName(t.Field(i))
		if tag == section {
			return cfgVal.Field(i)
		}
	}
	return reflect.Value{}
}

// walkTOMLPath resolves a dotted TOML key (e.g. "keepalive.enabled") through
// nested struct fields, matching each segment against TOML tags.
func walkTOMLPath(v reflect.Value, key string) reflect.Value {
	parts := strings.Split(key, ".")
	for _, part := range parts {
		v = derefPtr(v)
		if !v.IsValid() || v.Kind() != reflect.Struct {
			return reflect.Value{}
		}
		t := v.Type()
		found := false
		for i := 0; i < t.NumField(); i++ {
			if tomlTagName(t.Field(i)) == part {
				v = v.Field(i)
				found = true
				break
			}
		}
		if !found {
			return reflect.Value{}
		}
	}
	return v
}

// tomlTagName extracts the TOML key from a struct field tag, stripping options.
func tomlTagName(f reflect.StructField) string {
	tag := f.Tag.Get("toml")
	if idx := strings.IndexByte(tag, ','); idx >= 0 {
		tag = tag[:idx]
	}
	return tag
}

// derefPtr dereferences a pointer value, returning an invalid Value if nil.
func derefPtr(v reflect.Value) reflect.Value {
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return reflect.Value{}
		}
		return v.Elem()
	}
	return v
}

// reflectToDisplay converts a reflected config value to its display string.
func reflectToDisplay(v reflect.Value) string {
	v = derefPtr(v)
	if !v.IsValid() {
		return ""
	}
	return fmt.Sprintf("%v", v.Interface())
}

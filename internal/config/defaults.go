package config

import (
	"fmt"
	"reflect"
	"strconv"
	"time"
)

// Default-setting helpers for fields that need runtime context (IsDefined checks).
// Most defaults are now expressed as `default:"value"` struct tags in types.go,
// applied by ApplyTagDefaults. Only setStringDefaultDefined remains for fields
// whose defaults depend on runtime values (e.g. DataPath).

// setStringDefaultDefined sets *p to def when *p is empty AND the key was not explicitly defined.
func setStringDefaultDefined(p *string, def string, defined bool) {
	if *p == "" && !defined {
		*p = def
	}
}

// validateDurations checks that each value parses as a Go duration string.
// Returns the first error found, with section and key in the message.
func validateDurations(entries []durationEntry) error {
	for _, e := range entries {
		if _, err := time.ParseDuration(e.Value); err != nil {
			return fmt.Errorf("[%s] %s = %q: %w", e.Section, e.Key, e.Value, err)
		}
	}
	return nil
}

type durationEntry struct {
	Section, Key, Value string
}

// ApplyTagDefaults walks a struct (by pointer) and sets fields to the value
// specified in their `default` tag. For pointer fields, applies when nil.
// For non-pointer scalar fields, applies when zero. Recurses into nested structs.
//
// This keeps defaults co-located with field definitions in types.go:
//
//	SteerMode        *bool `toml:"steer_mode"         default:"true"`
//	MaxSystemPrompt  int   `toml:"max_system_prompt"  default:"20000"`
//
// Call on global config paths (not per-agent — agent pointer fields must
// stay nil for Merge to work correctly).
func ApplyTagDefaults(v any) {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return
	}
	applyTagDefaults(rv)
}

func applyTagDefaults(rv reflect.Value) {
	rt := rv.Type()
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
		ft := rt.Field(i)

		// Recurse into embedded and named struct fields.
		if f.Kind() == reflect.Struct {
			applyTagDefaults(f)
			continue
		}

		tag := ft.Tag.Get("default")
		if tag == "" {
			continue
		}

		// Pointer fields: apply when nil.
		if f.Kind() == reflect.Ptr {
			if !f.IsNil() {
				continue
			}
			setTagDefaultPtr(f, tag)
			continue
		}

		// Non-pointer scalar fields: apply when zero.
		if f.IsZero() {
			setTagDefaultScalar(f, tag)
		}
	}
}

func setTagDefaultPtr(f reflect.Value, tag string) {
	elemType := f.Type().Elem()
	switch elemType.Kind() {
	case reflect.Bool:
		v, _ := strconv.ParseBool(tag)
		f.Set(reflect.ValueOf(&v))
	case reflect.Int:
		v, _ := strconv.Atoi(tag)
		f.Set(reflect.ValueOf(&v))
	case reflect.Int64:
		v, _ := strconv.ParseInt(tag, 10, 64)
		f.Set(reflect.ValueOf(&v))
	case reflect.Float64:
		v, _ := strconv.ParseFloat(tag, 64)
		f.Set(reflect.ValueOf(&v))
	case reflect.String:
		// Handle named string types (e.g., InjectionLevel)
		val := reflect.New(elemType)
		val.Elem().SetString(tag)
		f.Set(val)
	}
}

func setTagDefaultScalar(f reflect.Value, tag string) {
	switch f.Kind() {
	case reflect.Bool:
		v, _ := strconv.ParseBool(tag)
		f.SetBool(v)
	case reflect.Int, reflect.Int64:
		v, _ := strconv.ParseInt(tag, 10, 64)
		f.SetInt(v)
	case reflect.Float64:
		v, _ := strconv.ParseFloat(tag, 64)
		f.SetFloat(v)
	case reflect.String:
		f.SetString(tag)
	}
}


package config

import (
	"fmt"
	"reflect"
	"strconv"
	"time"
)

// Default-setting helpers to reduce boilerplate in Load().
// Each function sets the target to the default value only when
// the current value is the zero value for its type.

func setStringDefault(p *string, def string) {
	if *p == "" {
		*p = def
	}
}

func setIntDefault(p *int, def int) {
	if *p == 0 {
		*p = def
	}
}


func setFloatDefault(p *float64, def float64) {
	if *p == 0 {
		*p = def
	}
}

// setStringDefaultDefined sets *p to def when *p is empty AND the key was not explicitly defined.
func setStringDefaultDefined(p *string, def string, defined bool) {
	if *p == "" && !defined {
		*p = def
	}
}

// setBoolDefaultDefined sets *p to def when the key was not explicitly defined in config.
// Use this for bool fields where the Go zero (false) is valid and we need
// metadata to distinguish "not set" from "set to false".
func setBoolDefaultDefined(p *bool, def bool, defined bool) { // nolint:unparam
	if !defined {
		*p = def
	}
}

// setIntDefaultDefined sets *p to def when *p is zero AND the key was not explicitly defined.
func setIntDefaultDefined(p *int, def int, defined bool) {
	if *p == 0 && !defined {
		*p = def
	}
}

// setFloatDefaultDefined sets *p to def when *p is zero AND the key was not explicitly defined.
func setFloatDefaultDefined(p *float64, def float64, defined bool) {
	if *p == 0 && !defined {
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

// ApplyTagDefaults walks a struct (by pointer) and sets nil pointer fields
// to the value specified in their `default` tag. Recurses into nested structs.
// Supports *bool, *int, *int64, *float64, *string, and *InjectionLevel.
//
// This keeps defaults co-located with field definitions in types.go:
//
//	SteerMode *bool `toml:"steer_mode" default:"true"`
//
// Call on global config paths (not per-agent — agent fields must stay nil
// for Merge to work correctly).
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

		// Only handle nil pointer fields with a default tag.
		if f.Kind() != reflect.Ptr || !f.IsNil() {
			continue
		}
		tag := ft.Tag.Get("default")
		if tag == "" {
			continue
		}

		setTagDefault(f, tag)
	}
}

func setTagDefault(f reflect.Value, tag string) {
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


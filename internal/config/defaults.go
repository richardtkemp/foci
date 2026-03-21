package config

import (
	"fmt"
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


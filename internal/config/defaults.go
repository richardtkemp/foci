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

func setInt64Default(p *int64, def int64) {
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

// MergeDefaults fills zero-value fields from the global config.
// If the receiver is entirely zero-value, replaces it wholesale.
func (ka *KeepaliveConfig) MergeDefaults(global KeepaliveConfig) {
	if *ka == (KeepaliveConfig{}) {
		*ka = global
		return
	}
	setStringDefault(&ka.Interval, global.Interval)
	setStringDefault(&ka.Prompt, global.Prompt)
}

// MergeDefaults fills zero-value fields from the global config.
func (bg *BackgroundConfig) MergeDefaults(global BackgroundConfig) {
	if *bg == (BackgroundConfig{}) {
		*bg = global
		return
	}
	setStringDefault(&bg.Interval, global.Interval)
	setStringDefault(&bg.Prompt, global.Prompt)
	setStringDefault(&bg.InvestInterval, global.InvestInterval)
}

// MergeDefaults fills zero-value fields from the global config.
// For *bool fields, copies global only when agent is nil and global is non-nil.
func (mf *MemoryFormationConfig) MergeDefaults(global MemoryFormationConfig) {
	if *mf == (MemoryFormationConfig{}) {
		*mf = global
		return
	}
	if mf.IntervalEnabled == nil && global.IntervalEnabled != nil {
		mf.IntervalEnabled = global.IntervalEnabled
	}
	setStringDefault(&mf.Interval, global.Interval)
	setStringDefault(&mf.IntervalPrompt, global.IntervalPrompt)
	if mf.ConsolidationEnabled == nil && global.ConsolidationEnabled != nil {
		mf.ConsolidationEnabled = global.ConsolidationEnabled
	}
	setStringDefault(&mf.ConsolidationInterval, global.ConsolidationInterval)
	setStringDefault(&mf.ConsolidationPrompt, global.ConsolidationPrompt)
	if mf.SessionEndEnabled == nil && global.SessionEndEnabled != nil {
		mf.SessionEndEnabled = global.SessionEndEnabled
	}
	setStringDefault(&mf.SessionEndPrompt, global.SessionEndPrompt)
}

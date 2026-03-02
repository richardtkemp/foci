package config

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
	setStringDefault(&bg.ManaStalenessTimeout, global.ManaStalenessTimeout)
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

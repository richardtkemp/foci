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

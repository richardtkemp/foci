package config

import "testing"

func TestValidateDurations(t *testing.T) {
	// Proves that validateDurations accepts valid Go duration strings without error
	// and returns an error identifying the field when an invalid duration is found.
	t.Run("valid durations pass", func(t *testing.T) {
		err := validateDurations([]durationEntry{
			{"logging", "rotation_period", "24h"},
			{"tools", "timeout", "30s"},
		})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("invalid duration returns error", func(t *testing.T) {
		err := validateDurations([]durationEntry{
			{"logging", "rotation_period", "24h"},
			{"tools", "timeout", "not-a-duration"},
		})
		if err == nil {
			t.Error("expected error for invalid duration")
		}
	})
}

func TestApplyTagDefaults(t *testing.T) {
	// Proves ApplyTagDefaults sets nil pointer fields from their `default` tags,
	// sets zero-value non-pointer scalar fields, and preserves non-nil/non-zero values.
	type inner struct {
		A *bool    `default:"true"`
		B *string  `default:"hello"`
		C *int     `default:"42"`
		D *float64 `default:"0.5"`
		E *bool    // no default tag — should stay nil
		// Non-pointer scalar fields
		F int     `default:"100"`
		G string  `default:"world"`
		H float64 `default:"3.14"`
		I int     // no default tag — should stay 0
	}
	type outer struct {
		X inner
	}
	v := outer{}
	ApplyTagDefaults(&v)

	if v.X.A == nil || !*v.X.A {
		t.Error("A should be true from tag default")
	}
	if v.X.B == nil || *v.X.B != "hello" {
		t.Error("B should be 'hello' from tag default")
	}
	if v.X.C == nil || *v.X.C != 42 {
		t.Error("C should be 42 from tag default")
	}
	if v.X.D == nil || *v.X.D != 0.5 {
		t.Error("D should be 0.5 from tag default")
	}
	if v.X.E != nil {
		t.Error("E should be nil (no default tag)")
	}
	if v.X.F != 100 {
		t.Errorf("F should be 100 from tag default, got %d", v.X.F)
	}
	if v.X.G != "world" {
		t.Errorf("G should be 'world' from tag default, got %q", v.X.G)
	}
	if v.X.H != 3.14 {
		t.Errorf("H should be 3.14 from tag default, got %f", v.X.H)
	}
	if v.X.I != 0 {
		t.Errorf("I should be 0 (no default tag), got %d", v.X.I)
	}

	// Non-nil pointer values should be preserved.
	v2 := outer{X: inner{A: Ptr(false), C: Ptr(0)}}
	ApplyTagDefaults(&v2)
	if *v2.X.A != false {
		t.Error("A should preserve explicit false")
	}
	if *v2.X.C != 0 {
		t.Error("C should preserve explicit 0")
	}

	// Non-zero scalar values should be preserved.
	v3 := outer{X: inner{F: 7, G: "custom"}}
	ApplyTagDefaults(&v3)
	if v3.X.F != 7 {
		t.Errorf("F should preserve explicit 7, got %d", v3.X.F)
	}
	if v3.X.G != "custom" {
		t.Errorf("G should preserve explicit 'custom', got %q", v3.X.G)
	}
}

func TestKeepaliveConfigMerge(t *testing.T) {
	// Proves that Merge fills all fields from the global config when the local
	// config is a zero value, and only fills nil fields when the local config
	// has partial values.
	global := KeepaliveConfig{Enabled: Ptr[bool](true), Interval: Ptr[string]("55m"), Prompt: Ptr[string]("global.md")}

	t.Run("replaces zero struct", func(t *testing.T) {
		ka := Merge(KeepaliveConfig{}, global)
		if DerefBool(ka.Enabled) != true || DerefStr(ka.Interval) != "55m" || DerefStr(ka.Prompt) != "global.md" {
			t.Errorf("expected full copy of global, got %+v", ka)
		}
	})
	t.Run("fills gaps", func(t *testing.T) {
		ka := Merge(KeepaliveConfig{Interval: Ptr[string]("30m")}, global)
		if DerefStr(ka.Interval) != "30m" {
			t.Errorf("Interval should be preserved, got %q", DerefStr(ka.Interval))
		}
		if DerefStr(ka.Prompt) != "global.md" {
			t.Errorf("Prompt should be filled from global, got %q", DerefStr(ka.Prompt))
		}
	})
}

func TestBackgroundConfigMerge(t *testing.T) {
	// Proves that Merge copies all global fields when the local config is zero,
	// and preserves non-nil local fields when merging.
	global := BackgroundConfig{Interval: Ptr[string]("5m"), Prompt: Ptr[string]("bg.md")}

	t.Run("replaces zero struct", func(t *testing.T) {
		bg := Merge(BackgroundConfig{}, global)
		if DerefStr(bg.Interval) != "5m" || DerefStr(bg.Prompt) != "bg.md" {
			t.Errorf("expected full copy of global, got %+v", bg)
		}
	})
	t.Run("fills gaps", func(t *testing.T) {
		bg := Merge(BackgroundConfig{Interval: Ptr[string]("10m")}, global)
		if DerefStr(bg.Interval) != "10m" {
			t.Errorf("Interval should be preserved, got %q", DerefStr(bg.Interval))
		}
	})
}

func TestReflectionConfigMerge(t *testing.T) {
	// Proves that Merge copies pointer fields from the global config when they
	// are nil locally, and preserves locally-set pointer values (including false)
	// without overwriting them.
	global := ReflectionConfig{
		IntervalEnabled: Ptr[bool](true), Interval: Ptr[string]("1h"), IntervalPrompt: Ptr[string]("mf.md"),
		SessionEndEnabled: Ptr[bool](true), SessionEndPrompt: Ptr[string]("se.md"),
	}

	t.Run("replaces zero struct", func(t *testing.T) {
		mf := Merge(ReflectionConfig{}, global)
		if DerefStr(mf.Interval) != "1h" || !DerefBool(mf.IntervalEnabled) {
			t.Errorf("expected full copy of global, got %+v", mf)
		}
	})
	t.Run("fills gaps preserving set values", func(t *testing.T) {
		mf := Merge(ReflectionConfig{IntervalEnabled: Ptr[bool](false), Interval: Ptr[string]("2h")}, global)
		if DerefBool(mf.IntervalEnabled) != false {
			t.Errorf("IntervalEnabled should be preserved as false")
		}
		if DerefStr(mf.Interval) != "2h" {
			t.Errorf("Interval should be preserved, got %q", DerefStr(mf.Interval))
		}
		if !DerefBool(mf.SessionEndEnabled) {
			t.Errorf("SessionEndEnabled should be filled from global")
		}
	})
}

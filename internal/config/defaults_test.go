package config

import "testing"

func TestSetStringDefault(t *testing.T) {
	// Proves that setStringDefault sets the value when the target is empty and
	// preserves the existing value when it is already non-empty.
	t.Run("sets when empty", func(t *testing.T) {
		v := ""
		setStringDefault(&v, "hello")
		if v != "hello" {
			t.Errorf("got %q, want %q", v, "hello")
		}
	})
	t.Run("preserves non-empty", func(t *testing.T) {
		v := "existing"
		setStringDefault(&v, "hello")
		if v != "existing" {
			t.Errorf("got %q, want %q", v, "existing")
		}
	})
}

func TestSetIntDefault(t *testing.T) {
	// Proves that setIntDefault sets the value when the target is zero and
	// preserves the existing value when it is already non-zero.
	t.Run("sets when zero", func(t *testing.T) {
		v := 0
		setIntDefault(&v, 42)
		if v != 42 {
			t.Errorf("got %d, want %d", v, 42)
		}
	})
	t.Run("preserves non-zero", func(t *testing.T) {
		v := 10
		setIntDefault(&v, 42)
		if v != 10 {
			t.Errorf("got %d, want %d", v, 10)
		}
	})
}

func TestSetFloatDefault(t *testing.T) {
	// Proves that setFloatDefault sets the value when the target is zero and
	// preserves an existing non-zero float value.
	t.Run("sets when zero", func(t *testing.T) {
		v := 0.0
		setFloatDefault(&v, 0.5)
		if v != 0.5 {
			t.Errorf("got %f, want %f", v, 0.5)
		}
	})
	t.Run("preserves non-zero", func(t *testing.T) {
		v := 0.25
		setFloatDefault(&v, 0.5)
		if v != 0.25 {
			t.Errorf("got %f, want %f", v, 0.25)
		}
	})
}

func TestSetBoolDefaultDefined(t *testing.T) {
	// Proves that setBoolDefaultDefined applies the default only when the field has
	// not been explicitly set (defined=false), and preserves the value when defined.
	t.Run("sets when not defined", func(t *testing.T) {
		v := false
		setBoolDefaultDefined(&v, true, false)
		if !v {
			t.Error("expected true when not defined")
		}
	})
	t.Run("preserves when defined", func(t *testing.T) {
		v := false
		setBoolDefaultDefined(&v, true, true)
		if v {
			t.Error("should preserve false when defined")
		}
	})
}

func TestSetIntDefaultDefined(t *testing.T) {
	// Proves that setIntDefaultDefined distinguishes between "zero because unset"
	// (applies default) and "zero because explicitly set" (preserves zero).
	t.Run("sets when zero and not defined", func(t *testing.T) {
		v := 0
		setIntDefaultDefined(&v, 10, false)
		if v != 10 {
			t.Errorf("got %d, want 10", v)
		}
	})
	t.Run("preserves non-zero even when not defined", func(t *testing.T) {
		v := 5
		setIntDefaultDefined(&v, 10, false)
		if v != 5 {
			t.Errorf("got %d, want 5", v)
		}
	})
	t.Run("preserves zero when defined", func(t *testing.T) {
		v := 0
		setIntDefaultDefined(&v, 10, true)
		if v != 0 {
			t.Errorf("got %d, want 0", v)
		}
	})
}

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

func TestMemoryFormationConfigMerge(t *testing.T) {
	// Proves that Merge copies pointer fields from the global config when they
	// are nil locally, and preserves locally-set pointer values (including false)
	// without overwriting them.
	global := MemoryFormationConfig{
		IntervalEnabled: Ptr[bool](true), Interval: Ptr[string]("1h"), IntervalPrompt: Ptr[string]("mf.md"),
		ConsolidationEnabled: Ptr[bool](true), ConsolidationInterval: Ptr[string]("20h"),
		SessionEndEnabled: Ptr[bool](true), SessionEndPrompt: Ptr[string]("se.md"),
	}

	t.Run("replaces zero struct", func(t *testing.T) {
		mf := Merge(MemoryFormationConfig{}, global)
		if DerefStr(mf.Interval) != "1h" || !DerefBool(mf.IntervalEnabled) {
			t.Errorf("expected full copy of global, got %+v", mf)
		}
	})
	t.Run("fills gaps preserving set values", func(t *testing.T) {
		mf := Merge(MemoryFormationConfig{IntervalEnabled: Ptr[bool](false), Interval: Ptr[string]("2h")}, global)
		if DerefBool(mf.IntervalEnabled) != false {
			t.Errorf("IntervalEnabled should be preserved as false")
		}
		if DerefStr(mf.Interval) != "2h" {
			t.Errorf("Interval should be preserved, got %q", DerefStr(mf.Interval))
		}
		if DerefStr(mf.ConsolidationInterval) != "20h" {
			t.Errorf("ConsolidationInterval should be filled, got %q", DerefStr(mf.ConsolidationInterval))
		}
		if !DerefBool(mf.SessionEndEnabled) {
			t.Errorf("SessionEndEnabled should be filled from global")
		}
	})
}

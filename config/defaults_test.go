package config

import "testing"

func TestSetStringDefault(t *testing.T) {
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

func TestSetInt64Default(t *testing.T) {
	t.Run("sets when zero", func(t *testing.T) {
		var v int64
		setInt64Default(&v, 1024)
		if v != 1024 {
			t.Errorf("got %d, want %d", v, 1024)
		}
	})
	t.Run("preserves non-zero", func(t *testing.T) {
		var v int64 = 512
		setInt64Default(&v, 1024)
		if v != 512 {
			t.Errorf("got %d, want %d", v, 512)
		}
	})
}

func TestSetFloatDefault(t *testing.T) {
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

func TestKeepaliveConfigMergeDefaults(t *testing.T) {
	global := KeepaliveConfig{Enabled: true, Interval: "55m", Prompt: "global.md"}

	t.Run("replaces zero struct", func(t *testing.T) {
		ka := KeepaliveConfig{}
		ka.MergeDefaults(global)
		if ka != global {
			t.Errorf("expected full copy of global, got %+v", ka)
		}
	})
	t.Run("fills gaps", func(t *testing.T) {
		ka := KeepaliveConfig{Interval: "30m"}
		ka.MergeDefaults(global)
		if ka.Interval != "30m" {
			t.Errorf("Interval should be preserved, got %q", ka.Interval)
		}
		if ka.Prompt != "global.md" {
			t.Errorf("Prompt should be filled from global, got %q", ka.Prompt)
		}
	})
}

func TestBackgroundConfigMergeDefaults(t *testing.T) {
	global := BackgroundConfig{Interval: "5m", Prompt: "bg.md", InvestInterval: "30m", ManaStalenessTimeout: "10m"}

	t.Run("replaces zero struct", func(t *testing.T) {
		bg := BackgroundConfig{}
		bg.MergeDefaults(global)
		if bg != global {
			t.Errorf("expected full copy of global, got %+v", bg)
		}
	})
	t.Run("fills gaps", func(t *testing.T) {
		bg := BackgroundConfig{Interval: "10m"}
		bg.MergeDefaults(global)
		if bg.Interval != "10m" {
			t.Errorf("Interval should be preserved, got %q", bg.Interval)
		}
		if bg.InvestInterval != "30m" {
			t.Errorf("InvestInterval should be filled, got %q", bg.InvestInterval)
		}
	})
}

func TestMemoryFormationConfigMergeDefaults(t *testing.T) {
	boolTrue := true
	global := MemoryFormationConfig{
		IntervalEnabled: &boolTrue, Interval: "1h", IntervalPrompt: "mf.md",
		ConsolidationEnabled: &boolTrue, ConsolidationInterval: "20h",
		SessionEndEnabled: &boolTrue, SessionEndPrompt: "se.md",
	}

	t.Run("replaces zero struct", func(t *testing.T) {
		mf := MemoryFormationConfig{}
		mf.MergeDefaults(global)
		if mf.Interval != "1h" || mf.IntervalEnabled != &boolTrue {
			t.Errorf("expected full copy of global, got %+v", mf)
		}
	})
	t.Run("fills gaps preserving set values", func(t *testing.T) {
		boolFalse := false
		mf := MemoryFormationConfig{IntervalEnabled: &boolFalse, Interval: "2h"}
		mf.MergeDefaults(global)
		if *mf.IntervalEnabled != false {
			t.Errorf("IntervalEnabled should be preserved as false")
		}
		if mf.Interval != "2h" {
			t.Errorf("Interval should be preserved, got %q", mf.Interval)
		}
		if mf.ConsolidationInterval != "20h" {
			t.Errorf("ConsolidationInterval should be filled, got %q", mf.ConsolidationInterval)
		}
		if mf.SessionEndEnabled != &boolTrue {
			t.Errorf("SessionEndEnabled should be filled from global")
		}
	})
}

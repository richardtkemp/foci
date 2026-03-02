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

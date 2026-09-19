package telemetry

import (
	"strings"
	"testing"
)

// TestRedactSecretValues covers the value-based half of the redactor: values
// >= minSecretLen are replaced wholesale (longest match first, so a value
// that contains a shorter value is never left partially scrubbed), and
// values shorter than minSecretLen are left alone so ordinary short prose
// isn't mangled.
func TestRedactSecretValues(t *testing.T) {
	t.Run("long value redacted", func(t *testing.T) {
		r := NewRedactor([]string{"SECRETVALUE123"})
		got, n := r.Redact("the token is SECRETVALUE123 in this message")
		if n != 1 {
			t.Errorf("n = %d, want 1", n)
		}
		if strings.Contains(got, "SECRETVALUE123") {
			t.Errorf("secret value leaked: %q", got)
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("expected [REDACTED] marker, got %q", got)
		}
	})

	t.Run("short value not redacted", func(t *testing.T) {
		r := NewRedactor([]string{"abc123"}) // 6 chars < minSecretLen(8)
		got, n := r.Redact("the code is abc123 today")
		if n != 0 {
			t.Errorf("n = %d, want 0 (value too short to redact)", n)
		}
		if got != "the code is abc123 today" {
			t.Errorf("got %q, want input unchanged", got)
		}
	})

	t.Run("longest value wins when one contains another", func(t *testing.T) {
		short := "SECRETVALUE123"    // 14 chars, contained in long
		long := "SECRETVALUE123XYZW" // 18 chars, contains short as a prefix
		r := NewRedactor([]string{short, long})
		got, n := r.Redact("prefix SECRETVALUE123XYZW suffix")
		if n != 1 {
			t.Errorf("n = %d, want 1 (one whole-value match)", n)
		}
		want := "prefix [REDACTED] suffix"
		if got != want {
			t.Errorf("got %q, want %q (longest-first should consume the whole value, not leave a partial tail)", got, want)
		}
	})

	t.Run("ordinary prose untouched", func(t *testing.T) {
		r := NewRedactor([]string{"SECRETVALUE123"})
		prose := "just a normal sentence about nothing in particular"
		got, n := r.Redact(prose)
		if n != 0 {
			t.Errorf("n = %d, want 0", n)
		}
		if got != prose {
			t.Errorf("got %q, want unchanged prose", got)
		}
	})
}

// TestRedactGenericPatterns exercises one representative of each credential
// shape the redactor recognises independent of any configured secret value.
func TestRedactGenericPatterns(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"anthropic key", "key: sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789 end"},
		{"github token", "token=" + "ghp_" + strings.Repeat("a", 24) + " done"},
		{"telegram bot token", "bot 123456789:" + strings.Repeat("A", 35) + " online"},
		{"jwt", "auth=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0." + strings.Repeat("x", 20)},
		{"bearer token", "Authorization: Bearer " + strings.Repeat("a", 24)},
		{"pem block", "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("Q", 40) + "\n-----END RSA PRIVATE KEY-----"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := NewRedactor(nil)
			got, n := r.Redact(c.input)
			if n == 0 {
				t.Fatalf("input not redacted at all: %q", c.input)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Errorf("got %q, want a [REDACTED] marker", got)
			}
		})
	}

	t.Run("ordinary prose returns zero", func(t *testing.T) {
		r := NewRedactor(nil)
		prose := "the quick brown fox jumps over the lazy dog, 12345 times"
		got, n := r.Redact(prose)
		if n != 0 {
			t.Errorf("n = %d, want 0", n)
		}
		if got != prose {
			t.Errorf("got %q, want unchanged", got)
		}
	})
}

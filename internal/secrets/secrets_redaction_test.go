package secrets

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	// TestRedact proves that every secret value in the store is replaced with [REDACTED]
	// in the output, while non-secret text is left intact.
	path := writeSecrets(t, `
[anthropic]
setup_token = "sk-ant-oat01-supersecret"

[custom]
api_key = "BSA-mykey123"
`)
	s, _ := Load(path)

	input := `Config dump:
ANTHROPIC_TOKEN=sk-ant-oat01-supersecret
API_KEY=BSA-mykey123
other stuff`

	result := s.Redact(input)

	if strings.Contains(result, "sk-ant-oat01-supersecret") {
		t.Error("token not redacted")
	}
	if strings.Contains(result, "BSA-mykey123") {
		t.Error("api_key not redacted")
	}
	if !strings.Contains(result, "[REDACTED]") {
		t.Error("missing [REDACTED] placeholder")
	}
	if !strings.Contains(result, "other stuff") {
		t.Error("non-secret text was modified")
	}
}

func TestRedactShortValues(t *testing.T) {
	// TestRedactShortValues proves that very short secret values (fewer than 4 characters)
	// are intentionally excluded from redaction to avoid false positives on common tokens.
	path := writeSecrets(t, `
[custom]
short = "ab"
long = "longersecret123"
`)
	s, _ := Load(path)

	input := "ab is fine, longersecret123 is not"
	result := s.Redact(input)

	if !strings.Contains(result, "ab is fine") {
		t.Errorf("short value was redacted: %q", result)
	}
	if strings.Contains(result, "longersecret123") {
		t.Error("long value not redacted")
	}
}

func TestRedactEmpty(t *testing.T) {
	// TestRedactEmpty proves that Redact on a store with no secrets is a pure passthrough,
	// returning the input unchanged.
	s, _ := Load("/nonexistent")
	result := s.Redact("nothing to redact")
	if result != "nothing to redact" {
		t.Errorf("result = %q", result)
	}
}

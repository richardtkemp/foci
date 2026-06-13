package tools

import (
	"strings"
	"testing"
)

// requireError asserts err is non-nil and contains substr.
func requireError(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Errorf("error = %q, want substring %q", err.Error(), substr)
	}
}

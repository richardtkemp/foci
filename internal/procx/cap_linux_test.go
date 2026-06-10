//go:build linux

package procx

import (
	"os"
	"strings"
	"testing"
)

// readCapAmb returns the raw hex value of the CapAmb field from
// /proc/self/status (the process's ambient capability set).
func readCapAmb(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatalf("read /proc/self/status: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "CapAmb:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "CapAmb:"))
		}
	}
	t.Fatal("CapAmb field not found in /proc/self/status")
	return ""
}

// TestClearAmbientCaps proves that clearAmbientCaps() empties the process
// ambient capability set without error. This is the load-bearing half of the
// P0-1 fix: ambient caps are preserved across execve for non-root processes,
// so any CAP_SETGID left in the ambient set would let a spawned child re-add
// the foci-secrets group. After the clear, CapAmb must be all zeroes — meaning
// children procx spawns inherit no ambient capabilities.
func TestClearAmbientCaps(t *testing.T) {
	if err := clearAmbientCaps(); err != nil {
		t.Fatalf("clearAmbientCaps: %v", err)
	}

	capAmb := readCapAmb(t)
	// The hex string is fixed-width zero-padded; an empty ambient set is all
	// zeroes regardless of width.
	if strings.Trim(capAmb, "0") != "" {
		t.Fatalf("ambient capability set not empty after clear: CapAmb=%s", capAmb)
	}
}

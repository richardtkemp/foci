package main

import (
	"bytes"
	"strings"
	"testing"

	"foci/internal/session"
)

// TestWriteKeysMatchesRegistry proves the tool prints exactly the registry's
// keys — one per line, in registry order, nothing added — which is the
// line-by-line parse contract audit-session-metadata.sh depends on.
func TestWriteKeysMatchesRegistry(t *testing.T) {
	if len(session.SessionMetadataKeys) == 0 {
		t.Fatal("SessionMetadataKeys is empty — the registry must never be")
	}
	var buf bytes.Buffer
	if err := writeKeys(&buf); err != nil {
		t.Fatalf("writeKeys: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != len(session.SessionMetadataKeys) {
		t.Fatalf("printed %d lines, registry has %d keys", len(lines), len(session.SessionMetadataKeys))
	}
	for i, mk := range session.SessionMetadataKeys {
		if lines[i] != mk.Key {
			t.Errorf("line %d = %q, want registry key %q", i+1, lines[i], mk.Key)
		}
		if strings.ContainsAny(mk.Key, "\n\r") {
			t.Errorf("registry key %q contains a newline — breaks one-key-per-line output", mk.Key)
		}
	}
}

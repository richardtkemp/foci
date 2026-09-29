package log

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppendAPILine: each call appends v as one JSON line to api.jsonl (the
// cost ledger writes one per booked call; this package owns only the file).
func TestAppendAPILine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.jsonl")
	f := openAPIWriter(t, path)

	for i := 0; i < 3; i++ {
		AppendAPILine(map[string]any{"session": "test", "n": i})
	}

	_ = f.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), data)
	}
	for i, l := range lines {
		var got struct {
			Session string `json:"session"`
			N       int    `json:"n"`
		}
		if err := json.Unmarshal([]byte(l), &got); err != nil {
			t.Fatalf("line %d does not parse: %v\n%s", i, err, l)
		}
		if got.Session != "test" || got.N != i {
			t.Errorf("line %d = %+v", i, got)
		}
	}
}

// TestAppendAPILineDisabled: with no api.jsonl open it is a no-op, and a value
// that cannot be marshalled writes nothing rather than a broken line.
func TestAppendAPILineDisabled(t *testing.T) {
	SetAPIWriter(nil)
	AppendAPILine(map[string]any{"session": "test"})

	path := filepath.Join(t.TempDir(), "api.jsonl")
	f := openAPIWriter(t, path)
	AppendAPILine(func() {})
	_ = f.Close()
	if data, _ := os.ReadFile(path); len(data) != 0 {
		t.Errorf("an unmarshallable value wrote %q", data)
	}
}

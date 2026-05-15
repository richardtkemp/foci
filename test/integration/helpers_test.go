//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// recorderEntry mirrors the JSONL shape cc-stub writes. Kept private to
// the integration test package — it's an internal contract between
// cc-stub and the L2 tests, not a public API.
//
// Two kinds:
//   "invocation" — one per process spawn (workdir, resume_id, flags)
//   "user_message" — one per user message processed (session_id, workdir, text_prefix)
type recorderEntry struct {
	Kind       string   `json:"kind"`
	Timestamp  string   `json:"ts"`
	Workdir    string   `json:"workdir"`
	ResumeID   string   `json:"resume_id,omitempty"`
	Model      string   `json:"model,omitempty"`
	Flags      []string `json:"flags,omitempty"`
	PID        int      `json:"pid,omitempty"`
	SessionID  string   `json:"session_id,omitempty"`
	TextPrefix string   `json:"text_prefix,omitempty"`
}

// readRecorderEntries parses every JSONL line from the recorder file.
// Missing file returns an empty slice (caller is polling) — that's a
// valid intermediate state, not an error.
func readRecorderEntries(t *testing.T, path string) []recorderEntry {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []recorderEntry
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var r recorderEntry
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("decode recorder line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

// invocationsByWorkdir filters to invocation entries whose workdir
// contains the given substring. Order-preserving.
func invocationsByWorkdir(entries []recorderEntry, workdirSubstr string) []recorderEntry {
	var out []recorderEntry
	for _, e := range entries {
		if e.Kind == "invocation" && strings.Contains(e.Workdir, workdirSubstr) {
			out = append(out, e)
		}
	}
	return out
}

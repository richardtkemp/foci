package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/secrets"
)

// TestIsolatedHTTPFileParamsContained proves that http_request in an isolated
// spawn cannot read or write outside its baseDir via file params. Raw/isolated
// spawns are advertised as confined to a temp dir, but body_file, files[], and
// save_to previously accepted arbitrary absolute paths — the P1-2 sandbox
// escape (e.g. files[].file_path = secrets.toml, or save_to = ~/.bashrc).
func TestIsolatedHTTPFileParamsContained(t *testing.T) {
	store, _ := secrets.Load("/nonexistent")
	baseDir := t.TempDir()
	base := NewHTTPRequestTool(store, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)
	tool := NewIsolatedHTTPRequestTool(base, store, baseDir)

	cases := []struct {
		name   string
		params map[string]any
	}{
		{"body_file", map[string]any{"url": "http://example.invalid/", "method": "POST", "body_file": "/etc/passwd"}},
		{"files", map[string]any{"url": "http://example.invalid/", "method": "POST", "files": []map[string]any{{"field_name": "f", "file_path": "/etc/passwd"}}}},
		{"save_to", map[string]any{"url": "http://example.invalid/", "method": "GET", "save_to": "/etc/cron.d/x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			params, _ := json.Marshal(c.params)
			_, err := tool.Execute(context.Background(), params)
			if err == nil {
				t.Fatalf("%s: expected absolute path outside baseDir to be rejected", c.name)
			}
		})
	}
}

// TestIsolatedSummaryContained proves the isolated summary tool rejects a file
// argument that escapes baseDir, so a raw spawn can't summarise (exfiltrate)
// files outside its sandbox.
func TestIsolatedSummaryContained(t *testing.T) {
	store, _ := secrets.Load("/nonexistent")
	baseDir := t.TempDir()
	base := NewSummaryTool(store, nil, "")
	tool := NewIsolatedSummaryTool(base, store, baseDir)

	params, _ := json.Marshal(map[string]any{"file": "/etc/passwd", "prompt": "dump"})
	if _, err := tool.Execute(context.Background(), params); err == nil {
		t.Fatal("expected absolute path outside baseDir to be rejected")
	}

	// A relative path within baseDir resolves (the read then fails because the
	// file doesn't exist, but containment must not be what rejects it).
	inside := filepath.Join(baseDir, "note.txt")
	_ = inside
	params, _ = json.Marshal(map[string]any{"file": "note.txt", "prompt": "x"})
	_, err := tool.Execute(context.Background(), params)
	if err != nil && strings.Contains(err.Error(), "traversal") {
		t.Fatalf("in-baseDir relative path should not be a traversal error: %v", err)
	}
}

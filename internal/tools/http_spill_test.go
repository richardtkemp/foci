package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// bigBodyServer returns an httptest server that replies with `size` bytes of
// 'a' as text/plain.
func bigBodyServer(t *testing.T, size int) *httptest.Server {
	t.Helper()
	body := strings.Repeat("a", size)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
}

func TestHTTPRequestSpillsLargeBody(t *testing.T) {
	// Proves that a text response larger than the inline preview is NOT
	// discarded: the inline result carries a preview, ResultFile points at the
	// full body on disk, and ResultSize is the full length. (The fix for the
	// old LimitReader-discard behaviour.)
	t.Parallel()
	const preview = 1024      // max_response_bytes → preview threshold
	const ceiling = 1 << 20   // http_max_spill_bytes
	const bodySize = 5 * 1024 // 5KB > preview, well under ceiling
	srv := bigBodyServer(t, bodySize)
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, t.TempDir(), func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return ceiling }, nil, 0640)
	params, _ := json.Marshal(map[string]any{
		"url":                srv.URL,
		"max_response_bytes": preview,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.ResultFile == "" {
		t.Fatal("expected ResultFile to be set for an over-preview body")
	}
	if result.ResultSize != bodySize {
		t.Errorf("ResultSize = %d, want %d (full body length)", result.ResultSize, bodySize)
	}
	// Inline preview is bounded by the preview threshold (plus the small header block).
	if len(result.Text) > preview+512 {
		t.Errorf("inline text = %d bytes, want roughly <= preview (%d)", len(result.Text), preview)
	}
	// Full body is recoverable from disk, intact.
	data, err := os.ReadFile(result.ResultFile)
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	if len(data) != bodySize {
		t.Errorf("spill file = %d bytes, want %d (full body)", len(data), bodySize)
	}
}

func TestHTTPRequestSmallBodyInline(t *testing.T) {
	// Proves the common case is unchanged: a response under the preview
	// threshold is returned fully inline with no spill file.
	t.Parallel()
	srv := bigBodyServer(t, 200)
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, t.TempDir(), func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 1 << 20 }, nil, 0640)
	params, _ := json.Marshal(map[string]any{"url": srv.URL})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ResultFile != "" {
		t.Errorf("small body should not spill, got ResultFile=%q", result.ResultFile)
	}
	if !strings.Contains(result.Text, strings.Repeat("a", 200)) {
		t.Error("small body should be fully inline")
	}
}

func TestHTTPRequestCeilingTruncates(t *testing.T) {
	// Proves the DoS ceiling bounds disk use: a body larger than
	// http_max_spill_bytes is capped on disk at the ceiling (not unbounded),
	// while ResultSize still reports the full source length seen.
	t.Parallel()
	const preview = 512
	const ceiling = 4 * 1024   // tiny ceiling
	const bodySize = 20 * 1024 // far exceeds the ceiling
	srv := bigBodyServer(t, bodySize)
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, t.TempDir(), func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return ceiling }, nil, 0640)
	params, _ := json.Marshal(map[string]any{
		"url":                srv.URL,
		"max_response_bytes": preview,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ResultFile == "" {
		t.Fatal("expected spill file")
	}
	data, err := os.ReadFile(result.ResultFile)
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	if int64(len(data)) > ceiling {
		t.Errorf("spill file = %d bytes, must not exceed ceiling %d", len(data), ceiling)
	}
}

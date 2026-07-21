package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPRequestBasicGET(t *testing.T) {
	// Proves that a basic GET request succeeds, returns HTTP 200, and includes the JSON response body in the result.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","method":"%s"}`, r.Method)
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"url": srv.URL + "/test",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "HTTP 200") {
		t.Errorf("expected HTTP 200 in result: %s", result.Text)
	}
	if !strings.Contains(result.Text, `"status":"ok"`) {
		t.Errorf("expected response body in result: %s", result.Text)
	}
	if !strings.Contains(result.Text, `"method":"GET"`) {
		t.Errorf("expected GET method in result: %s", result.Text)
	}
}

func TestHTTPRequestQueryParams(t *testing.T) {
	// Proves that query parameters passed via the "query" map are correctly appended to the URL and received by the server.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "q=%s&page=%s", r.URL.Query().Get("q"), r.URL.Query().Get("page"))
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"url": srv.URL + "/search",
		"query": map[string]string{
			"q":    "test query",
			"page": "2",
		},
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "q=test query") {
		t.Errorf("expected query param q: %s", result.Text)
	}
	if !strings.Contains(result.Text, "page=2") {
		t.Errorf("expected query param page: %s", result.Text)
	}
}

func TestHTTPRequestCustomTimeout(t *testing.T) {
	// Proves that a request with an explicit timeout parameter completes normally when the server responds within the limit.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL,
		"timeout": 60,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "ok") {
		t.Errorf("expected ok in result: %s", result.Text)
	}
}

func TestHTTPRequestTimeoutCap(t *testing.T) {
	// Proves that a 1-second timeout causes a deadline-exceeded error when the server takes 1.5 seconds to respond.
	t.Parallel()
	// A slow server that takes 1.5 seconds
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprint(w, "slow")
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)

	// Request with 1-second timeout should fail
	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL,
		"timeout": 1,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// Assert the SEMANTIC timeout, not its phrasing. net/http's Client.Timeout
	// wrapper reformats the message ("...request canceled (Client.Timeout
	// exceeded...)") so it no longer literally contains "deadline exceeded" —
	// wrapping the client transport in ratelimit.Transport (4e0af98f) surfaced
	// this — but the wrapped error is still context.DeadlineExceeded (verified
	// via errors.Is; the timeout is functionally unchanged). String-matching the
	// phrasing was the fragility; errors.Is is stable across it.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected a context.DeadlineExceeded timeout, got: %v", err)
	}
}

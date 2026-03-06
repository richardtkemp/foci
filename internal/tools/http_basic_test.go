package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHTTPRequestBasicGET verifies basic HTTP GET request with JSON response
func TestHTTPRequestBasicGET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","method":"%s"}`, r.Method)
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
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

// TestHTTPRequestQueryParams verifies query parameter handling
func TestHTTPRequestQueryParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "q=%s&page=%s", r.URL.Query().Get("q"), r.URL.Query().Get("page"))
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
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

// TestHTTPRequestCustomTimeout verifies custom timeout parameter is respected
func TestHTTPRequestCustomTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
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

// TestHTTPRequestTimeoutCap verifies timeout enforcement against slow servers
func TestHTTPRequestTimeoutCap(t *testing.T) {
	t.Parallel()
	// A slow server that takes 1.5 seconds
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprint(w, "slow")
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)

	// Request with 1-second timeout should fail
	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL,
		"timeout": 1,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") && !strings.Contains(err.Error(), "context") {
		t.Errorf("expected timeout error, got: %v", err)
	}
}

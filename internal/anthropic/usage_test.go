package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetUsageSuccess(t *testing.T) {
	util := 55.0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify path
		if r.URL.Path != "/api/oauth/usage" {
			t.Errorf("path = %q, want /api/oauth/usage", r.URL.Path)
		}
		// Verify headers
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-oauth-token" {
			t.Errorf("Authorization = %q", auth)
		}
		if beta := r.Header.Get("anthropic-beta"); !strings.Contains(beta, "oauth-2025-04-20") {
			t.Errorf("anthropic-beta = %q, want oauth", beta)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(UsageResponse{
			FiveHour: &UsageWindow{Utilization: &util},
		})
	}))
	defer server.Close()

	client := &UsageClient{
		oauthToken: "test-oauth-token",
		httpClient: http.DefaultClient,
		baseURL:    server.URL,
	}

	resp, err := client.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("GetUsage: %v", err)
	}
	if resp.FiveHour == nil {
		t.Fatal("FiveHour is nil")
	}
	if *resp.FiveHour.Utilization != 55.0 {
		t.Errorf("utilization = %f, want 55.0", *resp.FiveHour.Utilization)
	}
}

func TestGetUsageEmptyToken(t *testing.T) {
	client := &UsageClient{
		oauthToken: "",
		httpClient: http.DefaultClient,
		baseURL:    "http://localhost",
	}

	_, err := client.GetUsage(context.Background())
	if err == nil {
		t.Fatal("expected error for empty token")
	}
	if !strings.Contains(err.Error(), "OAuth token not configured") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestGetUsageAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token"}`))
	}))
	defer server.Close()

	client := &UsageClient{
		oauthToken: "bad-token",
		httpClient: http.DefaultClient,
		baseURL:    server.URL,
	}

	_, err := client.GetUsage(context.Background())
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "API error (status 401)") {
		t.Errorf("error = %q", err.Error())
	}
}

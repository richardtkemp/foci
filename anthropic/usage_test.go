package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFormatUsageNil(t *testing.T) {
	result := FormatUsage(nil)
	if result != "No usage data" {
		t.Errorf("FormatUsage(nil) = %q", result)
	}
}

func TestFormatUsageEmpty(t *testing.T) {
	result := FormatUsage(&UsageResponse{})
	if result != "No active usage limits" {
		t.Errorf("FormatUsage(empty) = %q", result)
	}
}

func TestFormatUsagePercentage(t *testing.T) {
	// >= 1% — no decimals
	util := 42.0
	result := FormatUsage(&UsageResponse{
		FiveHour: &UsageWindow{Utilization: &util},
	})
	if !strings.Contains(result, "42% used") {
		t.Errorf("result = %q, want '42%% used'", result)
	}

	// < 1% — one decimal
	util = 0.3
	result = FormatUsage(&UsageResponse{
		FiveHour: &UsageWindow{Utilization: &util},
	})
	if !strings.Contains(result, "0.3% used") {
		t.Errorf("result = %q, want '0.3%% used'", result)
	}
}

func TestFormatUsageResetTime(t *testing.T) {
	util := 50.0
	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	result := FormatUsage(&UsageResponse{
		FiveHour: &UsageWindow{
			Utilization: &util,
			ResetsAt:    &future,
		},
	})
	if !strings.Contains(result, "resets") {
		t.Errorf("result = %q, want 'resets'", result)
	}
}

func TestFormatUsageOverage(t *testing.T) {
	util := 80.0
	result := FormatUsage(&UsageResponse{
		FiveHour: &UsageWindow{Utilization: &util},
		ExtraUsage: &ExtraUsage{
			IsEnabled:   true,
			UsedCredits: 1.50,
		},
	})
	if !strings.Contains(result, "overage $1.50") {
		t.Errorf("result = %q, want 'overage $1.50'", result)
	}
}

func TestFormatUsageOverageDisabled(t *testing.T) {
	util := 80.0
	result := FormatUsage(&UsageResponse{
		FiveHour: &UsageWindow{Utilization: &util},
		ExtraUsage: &ExtraUsage{
			IsEnabled:   false,
			UsedCredits: 5.0,
		},
	})
	if strings.Contains(result, "overage") {
		t.Errorf("result = %q, should not show overage when disabled", result)
	}
}

func TestFormatUsageOverageZero(t *testing.T) {
	util := 80.0
	result := FormatUsage(&UsageResponse{
		FiveHour: &UsageWindow{Utilization: &util},
		ExtraUsage: &ExtraUsage{
			IsEnabled:   true,
			UsedCredits: 0.0,
		},
	})
	if strings.Contains(result, "overage") {
		t.Errorf("result = %q, should not show overage when zero", result)
	}
}

func TestFormatUsageAllFields(t *testing.T) {
	util := 75.0
	future := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339Nano)
	result := FormatUsage(&UsageResponse{
		FiveHour: &UsageWindow{
			Utilization: &util,
			ResetsAt:    &future,
		},
		ExtraUsage: &ExtraUsage{
			IsEnabled:   true,
			UsedCredits: 2.75,
		},
	})
	if !strings.Contains(result, "75% used") {
		t.Errorf("result missing utilization: %q", result)
	}
	if !strings.Contains(result, "resets") {
		t.Errorf("result missing reset time: %q", result)
	}
	if !strings.Contains(result, "overage $2.75") {
		t.Errorf("result missing overage: %q", result)
	}
}

func TestParseResetTimePast(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339Nano)
	result := parseResetTime(past)
	if result != "now" {
		t.Errorf("parseResetTime(past) = %q, want %q", result, "now")
	}
}

func TestParseResetTimeLessThanMinute(t *testing.T) {
	soon := time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339Nano)
	result := parseResetTime(soon)
	if result != "in <1m" {
		t.Errorf("parseResetTime(30s) = %q, want %q", result, "in <1m")
	}
}

func TestParseResetTimeMinutes(t *testing.T) {
	future := time.Now().Add(45 * time.Minute).UTC().Format(time.RFC3339Nano)
	result := parseResetTime(future)
	if !strings.HasPrefix(result, "in ") || !strings.HasSuffix(result, "m") {
		t.Errorf("parseResetTime(45m) = %q, want 'in Xm'", result)
	}
}

func TestParseResetTimeHours(t *testing.T) {
	future := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339Nano)
	result := parseResetTime(future)
	if !strings.HasPrefix(result, "in ") || !strings.HasSuffix(result, "h") {
		t.Errorf("parseResetTime(3h) = %q, want 'in Xh'", result)
	}
}

func TestParseResetTimeMoreThan24h(t *testing.T) {
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339Nano)
	result := parseResetTime(future)
	// Should show as a time like "2pm"
	if strings.HasPrefix(result, "in ") {
		t.Errorf("parseResetTime(48h) = %q, should not be relative", result)
	}
	if result == "" {
		t.Error("parseResetTime(48h) returned empty string")
	}
}

func TestParseResetTimeInvalid(t *testing.T) {
	result := parseResetTime("not-a-timestamp")
	if result != "" {
		t.Errorf("parseResetTime(invalid) = %q, want empty", result)
	}
}

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

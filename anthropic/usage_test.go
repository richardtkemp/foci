package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if !strings.HasPrefix(result, "in ") || !strings.Contains(result, "h") {
		t.Errorf("parseResetTime(3h) = %q, want 'in Xh' or 'in Xh Ym'", result)
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

func TestReadCredentialsToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")

	creds := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-test123","refreshToken":"sk-ant-ort01-test456","expiresAt":1771770729992}}`
	os.WriteFile(path, []byte(creds), 0644)

	token, err := ReadCredentialsToken(path)
	if err != nil {
		t.Fatalf("ReadCredentialsToken: %v", err)
	}
	if token != "sk-ant-oat01-test123" {
		t.Errorf("token = %q, want %q", token, "sk-ant-oat01-test123")
	}
}

func TestReadCredentialsTokenMissingFile(t *testing.T) {
	_, err := ReadCredentialsToken("/nonexistent/path/credentials.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestReadCredentialsTokenInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	os.WriteFile(path, []byte("not json"), 0644)

	_, err := ReadCredentialsToken(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestNewUsageClientWithFunc(t *testing.T) {
	callCount := 0
	client := NewUsageClientWithFunc(func() string {
		callCount++
		return "dynamic-token"
	})

	// getToken should call the func
	if got := client.getToken(); got != "dynamic-token" {
		t.Errorf("getToken() = %q, want %q", got, "dynamic-token")
	}
	if callCount != 1 {
		t.Errorf("callCount = %d, want 1", callCount)
	}

	// Call again — should call func again (not cached)
	client.getToken()
	if callCount != 2 {
		t.Errorf("callCount = %d, want 2", callCount)
	}
}

func TestFormatManaNil(t *testing.T) {
	if got := FormatMana(nil); got != "" {
		t.Errorf("FormatMana(nil) = %q, want empty", got)
	}
}

func TestFormatManaResetNil(t *testing.T) {
	if got := FormatManaReset(nil); got != "" {
		t.Errorf("FormatManaReset(nil) = %q, want empty", got)
	}
}

func TestFormatManaResetNoFiveHour(t *testing.T) {
	if got := FormatManaReset(&UsageResponse{}); got != "" {
		t.Errorf("FormatManaReset(empty) = %q, want empty", got)
	}
}

func TestFormatManaResetNoResetsAt(t *testing.T) {
	util := 50.0
	if got := FormatManaReset(&UsageResponse{FiveHour: &UsageWindow{Utilization: &util}}); got != "" {
		t.Errorf("FormatManaReset(no ResetsAt) = %q, want empty", got)
	}
}

func TestFormatManaResetWithTime(t *testing.T) {
	util := 50.0
	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	got := FormatManaReset(&UsageResponse{
		FiveHour: &UsageWindow{
			Utilization: &util,
			ResetsAt:    &future,
		},
	})
	if !strings.HasPrefix(got, "in ") || !strings.Contains(got, "h") {
		t.Errorf("FormatManaReset(2h) = %q, want 'in Xh' or 'in Xh Ym'", got)
	}
}

func TestFormatManaResetPast(t *testing.T) {
	util := 50.0
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339Nano)
	got := FormatManaReset(&UsageResponse{
		FiveHour: &UsageWindow{
			Utilization: &util,
			ResetsAt:    &past,
		},
	})
	if got != "now" {
		t.Errorf("FormatManaReset(past) = %q, want %q", got, "now")
	}
}

func TestFormatManaResetMinutes(t *testing.T) {
	util := 50.0
	future := time.Now().Add(45 * time.Minute).UTC().Format(time.RFC3339Nano)
	got := FormatManaReset(&UsageResponse{
		FiveHour: &UsageWindow{
			Utilization: &util,
			ResetsAt:    &future,
		},
	})
	if !strings.HasPrefix(got, "in ") || !strings.HasSuffix(got, "m") {
		t.Errorf("FormatManaReset(45m) = %q, want 'in Xm'", got)
	}
}

func TestFormatManaNoFiveHour(t *testing.T) {
	if got := FormatMana(&UsageResponse{}); got != "" {
		t.Errorf("FormatMana(empty) = %q, want empty", got)
	}
}

func TestFormatManaValues(t *testing.T) {
	tests := []struct {
		util float64
		want string
	}{
		{0, "100%"},
		{25, "75%"},
		{50, "50%"},
		{99.5, "0.5%"},
		{100, "0.0%"},
		{110, "0.0%"}, // clamped to 0
	}
	for _, tt := range tests {
		util := tt.util
		got := FormatMana(&UsageResponse{
			FiveHour: &UsageWindow{Utilization: &util},
		})
		if got != tt.want {
			t.Errorf("FormatMana(util=%.1f) = %q, want %q", tt.util, got, tt.want)
		}
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

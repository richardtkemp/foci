package anthropic

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writeCCCreds writes a Claude Code format credentials file.
func writeCCCreds(t *testing.T, path, accessToken, refreshToken string, expiresAt time.Time) {
	t.Helper()
	data := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  accessToken,
			"refreshToken": refreshToken,
			"expiresAt":    expiresAt.UnixMilli(),
		},
	}
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCCTokenSource_ReadsFile(t *testing.T) {
	// Proves that NewCCTokenSource successfully reads a valid credentials file and Token() returns the access token.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	writeCCCreds(t, path, "tok-abc", "ref-123", time.Now().Add(time.Hour))

	src, err := NewCCTokenSource(path)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}

	tok, err := src.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "tok-abc" {
		t.Errorf("got %q, want tok-abc", tok)
	}
}

func TestCCTokenSource_ReadsFromDiskEachCall(t *testing.T) {
	// Proves that Token() reads from disk on every call and picks up file changes immediately, without any polling interval.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	writeCCCreds(t, path, "tok-1", "ref-1", time.Now().Add(time.Hour))

	src, err := NewCCTokenSource(path)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}

	tok, _ := src.Token()
	if tok != "tok-1" {
		t.Fatalf("expected tok-1, got %q", tok)
	}

	// Overwrite file — next call should return new token immediately.
	writeCCCreds(t, path, "tok-2", "ref-2", time.Now().Add(time.Hour))

	tok, _ = src.Token()
	if tok != "tok-2" {
		t.Errorf("expected tok-2 after file change, got %q", tok)
	}
}

func TestCCTokenSource_HandlesMissingFile(t *testing.T) {
	// Proves that NewCCTokenSource returns an error when the credentials file does not exist.
	_, err := NewCCTokenSource("/nonexistent/path/creds.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestCCTokenSource_HandlesCorruptFile(t *testing.T) {
	// Proves that NewCCTokenSource returns an error when the credentials file contains invalid JSON.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := NewCCTokenSource(path)
	if err == nil {
		t.Fatal("expected error for corrupt file")
	}
}

func TestCCTokenSource_HandlesCorruptFileMidRun(t *testing.T) {
	// Proves that if the file becomes corrupt after successful startup, Token() returns the last known good token, providing resilience during file rewrites.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	writeCCCreds(t, path, "tok-good", "ref-1", time.Now().Add(time.Hour))

	src, err := NewCCTokenSource(path)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}

	// Corrupt the file.
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}

	// Should return last known token, not error.
	tok, err := src.Token()
	if err != nil {
		t.Fatalf("expected stale token, got error: %v", err)
	}
	if tok != "tok-good" {
		t.Errorf("expected tok-good (stale), got %q", tok)
	}
}

func TestCCTokenSource_ExpiredTokenReturnsError(t *testing.T) {
	// Proves that Token() returns an error when the token is already expired, and triggers a refresh callback.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	writeCCCreds(t, path, "tok-expired", "ref-1", time.Now().Add(-time.Hour))

	var refreshed atomic.Int32

	src, err := NewCCTokenSource(path)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}
	src.SetRefreshFunc(func() { refreshed.Add(1) })

	_, err = src.Token()
	if err == nil {
		t.Fatal("expected error for expired token")
	}

	// Give the goroutine time to fire.
	time.Sleep(50 * time.Millisecond)

	if refreshed.Load() != 1 {
		t.Errorf("expected refresh to fire once, fired %d times", refreshed.Load())
	}
}

func TestCCTokenSource_ExpiredRefreshFiresOnce(t *testing.T) {
	// Proves that repeated Token() calls on an expired token only trigger one refresh (not one per call).
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	writeCCCreds(t, path, "tok-expired", "ref-1", time.Now().Add(-time.Hour))

	var refreshed atomic.Int32

	src, err := NewCCTokenSource(path)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}
	src.SetRefreshFunc(func() { refreshed.Add(1) })

	// Multiple calls — should only trigger one refresh.
	src.Token()
	src.Token()
	src.Token()

	time.Sleep(50 * time.Millisecond)

	if refreshed.Load() != 1 {
		t.Errorf("expected 1 refresh, got %d", refreshed.Load())
	}
}

func TestCCTokenSource_FreshTokenResetsRefreshFlag(t *testing.T) {
	// Proves that after a refresh fires and the file is updated with a valid token, the refresh flag resets so a subsequent expiry triggers another refresh.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	writeCCCreds(t, path, "tok-expired", "ref-1", time.Now().Add(-time.Hour))

	var refreshed atomic.Int32

	src, err := NewCCTokenSource(path)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}
	src.SetRefreshFunc(func() { refreshed.Add(1) })

	// Trigger first refresh.
	src.Token()
	time.Sleep(50 * time.Millisecond)
	if refreshed.Load() != 1 {
		t.Fatalf("expected 1 refresh, got %d", refreshed.Load())
	}

	// Write a fresh token — should reset the flag.
	writeCCCreds(t, path, "tok-fresh", "ref-2", time.Now().Add(time.Hour))
	tok, err := src.Token()
	if err != nil {
		t.Fatalf("Token after refresh: %v", err)
	}
	if tok != "tok-fresh" {
		t.Errorf("expected tok-fresh, got %q", tok)
	}

	// Expire it again — should trigger another refresh.
	writeCCCreds(t, path, "tok-expired-again", "ref-3", time.Now().Add(-time.Hour))
	src.Token()
	time.Sleep(50 * time.Millisecond)

	if refreshed.Load() != 2 {
		t.Errorf("expected 2 refreshes (reset after fresh), got %d", refreshed.Load())
	}
}

func TestCCTokenSource_CheckRefreshTriggersProactively(t *testing.T) {
	// Proves that CheckRefresh triggers a proactive refresh when the token is within the expiry threshold, without waiting for actual expiry.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	// Token expires in 2 minutes — within the 5-minute default threshold.
	writeCCCreds(t, path, "tok-soon", "ref-1", time.Now().Add(2*time.Minute))

	var refreshed atomic.Int32

	src, err := NewCCTokenSource(path)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}
	src.SetRefreshFunc(func() { refreshed.Add(1) })

	// Token should still be valid (not expired yet).
	tok, err := src.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "tok-soon" {
		t.Errorf("expected tok-soon, got %q", tok)
	}

	// CheckRefresh should detect near-expiry and trigger refresh.
	src.CheckRefresh()
	time.Sleep(50 * time.Millisecond)

	if refreshed.Load() != 1 {
		t.Errorf("expected proactive refresh, got %d fires", refreshed.Load())
	}
}

func TestCCTokenSource_CheckRefreshNoOpWhenFarFromExpiry(t *testing.T) {
	// Proves that CheckRefresh does NOT trigger a refresh when the token has plenty of time left.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	writeCCCreds(t, path, "tok-far", "ref-1", time.Now().Add(time.Hour))

	var refreshed atomic.Int32

	src, err := NewCCTokenSource(path)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}
	src.SetRefreshFunc(func() { refreshed.Add(1) })

	src.CheckRefresh()
	time.Sleep(50 * time.Millisecond)

	if refreshed.Load() != 0 {
		t.Errorf("expected no refresh for far-off expiry, got %d fires", refreshed.Load())
	}
}

func TestCCTokenSource_FociFormat(t *testing.T) {
	// Proves that the token source can parse foci's native flat credential format in addition to the Claude Code nested format.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")

	// Write foci-native format.
	data := map[string]any{
		"access_token":  "foci-tok",
		"refresh_token": "foci-ref",
		"expires_at":    time.Now().Add(time.Hour).UnixMilli(),
	}
	b, _ := json.Marshal(data)
	os.WriteFile(path, b, 0600)

	src, err := NewCCTokenSource(path)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}

	tok, _ := src.Token()
	if tok != "foci-tok" {
		t.Errorf("got %q, want foci-tok", tok)
	}
}

func TestCCTokenSource_SetExpiryThreshold(t *testing.T) {
	// Proves SetExpiryThreshold controls the proactive-refresh window: a token expiring in 2 minutes triggers no refresh under a 1-minute threshold, then triggers one after widening to 10 minutes.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	writeCCCreds(t, path, "tok", "ref", time.Now().Add(2*time.Minute))

	var refreshed atomic.Int32
	src, err := NewCCTokenSource(path)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}
	src.SetRefreshFunc(func() { refreshed.Add(1) })

	src.SetExpiryThreshold(time.Minute)
	src.CheckRefresh()
	time.Sleep(50 * time.Millisecond)
	if refreshed.Load() != 0 {
		t.Fatalf("refresh fired with 1m threshold and 2m left, got %d", refreshed.Load())
	}

	src.SetExpiryThreshold(10 * time.Minute)
	src.CheckRefresh()
	time.Sleep(50 * time.Millisecond)
	if refreshed.Load() != 1 {
		t.Errorf("expected refresh with 10m threshold and 2m left, got %d", refreshed.Load())
	}
}

func TestCCTokenSource_ReadErrorWithNoCachedToken(t *testing.T) {
	// Proves Token() errors with "no CC token available" when the file is unreadable and no token was ever cached.
	src := &CCTokenSource{path: "/nonexistent/creds.json"}
	_, err := src.Token()
	if err == nil || !strings.Contains(err.Error(), "no CC token available") {
		t.Errorf("err = %v, want no-token error", err)
	}
}

func TestCCTokenSource_ReadErrorWithExpiredCachedToken(t *testing.T) {
	// Proves that when the file disappears and the cached token has expired, Token() refuses to serve the stale token, errors, and triggers a refresh.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	// Valid at startup but expires almost immediately.
	writeCCCreds(t, path, "tok-short", "ref", time.Now().Add(10*time.Millisecond))

	var refreshed atomic.Int32
	src, err := NewCCTokenSource(path)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}
	src.SetRefreshFunc(func() { refreshed.Add(1) })

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// Force the cached expiry into the past deterministically.
	src.mu.Lock()
	src.expiresAt = time.Now().Add(-time.Minute)
	src.mu.Unlock()

	_, err = src.Token()
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v, want expired-token error", err)
	}
	time.Sleep(50 * time.Millisecond)
	if refreshed.Load() != 1 {
		t.Errorf("expected refresh trigger, got %d", refreshed.Load())
	}
}

func TestCCTokenSource_CheckRefreshNoOpWithZeroExpiry(t *testing.T) {
	// Proves CheckRefresh is a no-op when no expiry is known (zero time), instead of treating it as imminently expiring.
	var refreshed atomic.Int32
	src := &CCTokenSource{expiryThreshold: defaultExpiryThreshold}
	src.SetRefreshFunc(func() { refreshed.Add(1) })

	src.CheckRefresh()
	time.Sleep(50 * time.Millisecond)
	if refreshed.Load() != 0 {
		t.Errorf("refresh fired despite zero expiry, got %d", refreshed.Load())
	}
}

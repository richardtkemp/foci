package anthropic

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

	src, err := NewCCTokenSource(path, 30*time.Second)
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

func TestCCTokenSource_CachesWithinPollInterval(t *testing.T) {
	// Proves that the token source serves the cached value without re-reading the file when called within the poll interval, even after the file has been updated.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	writeCCCreds(t, path, "tok-1", "ref-1", time.Now().Add(time.Hour))

	src, err := NewCCTokenSource(path, time.Hour) // very long poll interval
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}

	// Overwrite file — should not be re-read within poll interval.
	writeCCCreds(t, path, "tok-2", "ref-2", time.Now().Add(time.Hour))

	tok, _ := src.Token()
	if tok != "tok-1" {
		t.Errorf("expected cached tok-1 within poll interval, got %q", tok)
	}
}

func TestCCTokenSource_DetectsChanges(t *testing.T) {
	// Proves that after the poll interval expires the token source re-reads the file and returns the updated token.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	writeCCCreds(t, path, "tok-old", "ref-1", time.Now().Add(time.Hour))

	src, err := NewCCTokenSource(path, time.Millisecond) // very short interval
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}

	// Overwrite with new token.
	writeCCCreds(t, path, "tok-new", "ref-2", time.Now().Add(time.Hour))

	// Wait for poll interval to expire.
	time.Sleep(5 * time.Millisecond)

	tok, _ := src.Token()
	if tok != "tok-new" {
		t.Errorf("expected tok-new after file change, got %q", tok)
	}
}

func TestCCTokenSource_HandlesMissingFile(t *testing.T) {
	// Proves that NewCCTokenSource returns an error rather than silently succeeding when the credentials file does not exist.
	_, err := NewCCTokenSource("/nonexistent/path/creds.json", 30*time.Second)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestCCTokenSource_HandlesCorruptFile(t *testing.T) {
	// Proves that NewCCTokenSource returns an error at construction time when the credentials file contains invalid JSON.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := NewCCTokenSource(path, 30*time.Second)
	if err == nil {
		t.Fatal("expected error for corrupt file")
	}
}

func TestCCTokenSource_HandlesCorruptFileMidRun(t *testing.T) {
	// Proves that if the file becomes corrupt after successful startup, Token() returns the last known good token instead of an error, providing resilience during file rewrites.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	writeCCCreds(t, path, "tok-good", "ref-1", time.Now().Add(time.Hour))

	src, err := NewCCTokenSource(path, time.Millisecond)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}

	// Corrupt the file.
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Millisecond)

	// Should return last known token, not error.
	tok, err := src.Token()
	if err != nil {
		t.Fatalf("expected stale token, got error: %v", err)
	}
	if tok != "tok-good" {
		t.Errorf("expected tok-good (stale), got %q", tok)
	}
}

func TestCCTokenSource_DetectsExpiryAndFiresCallback(t *testing.T) {
	// Proves that the OnExpired callback fires exactly once when an expired token is detected, and does not fire again on subsequent polls of the same expired token.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	// Write token that is already expired.
	writeCCCreds(t, path, "tok-expired", "ref-1", time.Now().Add(-time.Hour))

	var fired atomic.Int32

	src, err := NewCCTokenSource(path, time.Millisecond)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}
	src.OnExpired(func() { fired.Add(1) })

	// Trigger a poll by reading token.
	time.Sleep(5 * time.Millisecond)
	tok, _ := src.Token()
	if tok != "tok-expired" {
		t.Errorf("expected stale tok-expired, got %q", tok)
	}

	// Give the goroutine time to fire.
	time.Sleep(50 * time.Millisecond)

	if fired.Load() != 1 {
		t.Errorf("expected onExpired to fire once, fired %d times", fired.Load())
	}

	// Second poll — should not fire again.
	time.Sleep(5 * time.Millisecond)
	src.Token()
	time.Sleep(50 * time.Millisecond)

	if fired.Load() != 1 {
		t.Errorf("expected onExpired to fire only once, fired %d times", fired.Load())
	}
}

func TestCCTokenSource_ResetsExpiredOnFreshToken(t *testing.T) {
	// Proves that after an expiry fires the callback and the file is then updated with a valid token, the expiry flag resets so a subsequent re-expiry fires the callback a second time.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	// Start expired.
	writeCCCreds(t, path, "tok-expired", "ref-1", time.Now().Add(-time.Hour))

	var fired atomic.Int32

	src, err := NewCCTokenSource(path, time.Millisecond)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}
	src.OnExpired(func() { fired.Add(1) })

	// Trigger first expiry detection.
	time.Sleep(5 * time.Millisecond)
	src.Token()
	time.Sleep(50 * time.Millisecond)
	if fired.Load() != 1 {
		t.Fatalf("expected 1 fire, got %d", fired.Load())
	}

	// Write a fresh token.
	writeCCCreds(t, path, "tok-fresh", "ref-2", time.Now().Add(time.Hour))
	time.Sleep(5 * time.Millisecond)
	src.Token()
	time.Sleep(50 * time.Millisecond)

	// Now expire it again.
	writeCCCreds(t, path, "tok-expired-again", "ref-3", time.Now().Add(-time.Hour))
	time.Sleep(5 * time.Millisecond)
	src.Token()
	time.Sleep(50 * time.Millisecond)

	if fired.Load() != 2 {
		t.Errorf("expected 2 fires (reset after fresh), got %d", fired.Load())
	}
}

func TestCCTokenSource_StartStop(t *testing.T) {
	// Proves that the background polling goroutine started by Start() picks up file changes automatically, and that Stop() cleanly terminates it.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	writeCCCreds(t, path, "tok-1", "ref-1", time.Now().Add(time.Hour))

	src, err := NewCCTokenSource(path, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}

	ctx := context.Background()
	src.Start(ctx)

	// Update file — background loop should pick it up.
	writeCCCreds(t, path, "tok-updated", "ref-2", time.Now().Add(time.Hour))
	time.Sleep(50 * time.Millisecond)

	tok, _ := src.Token()
	if tok != "tok-updated" {
		t.Errorf("expected background poll to pick up tok-updated, got %q", tok)
	}

	src.Stop()
}

func TestCCTokenSource_FociFormat(t *testing.T) {
	// Proves that the token source can parse foci's native flat credential format (access_token, refresh_token, expires_at at top level) in addition to the Claude Code nested format.
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

	src, err := NewCCTokenSource(path, 30*time.Second)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}

	tok, _ := src.Token()
	if tok != "foci-tok" {
		t.Errorf("got %q, want foci-tok", tok)
	}
}

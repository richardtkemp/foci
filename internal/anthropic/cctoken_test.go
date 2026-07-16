package anthropic

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	// Proves that Token() returns an error when the token is already expired.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	writeCCCreds(t, path, "tok-expired", "ref-1", time.Now().Add(-time.Hour))

	src, err := NewCCTokenSource(path)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}

	_, err = src.Token()
	if err == nil {
		t.Fatal("expected error for expired token")
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

func TestCCTokenSource_ReadErrorWithNoCachedToken(t *testing.T) {
	// Proves Token() errors with "no CC token available" when the file is unreadable and no token was ever cached.
	src := &CCTokenSource{path: "/nonexistent/creds.json"}
	_, err := src.Token()
	if err == nil || !strings.Contains(err.Error(), "no CC token available") {
		t.Errorf("err = %v, want no-token error", err)
	}
}

func TestCCTokenSource_ReadErrorWithExpiredCachedToken(t *testing.T) {
	// Proves that when the file disappears and the cached token has expired, Token() refuses to serve the stale token and errors.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	// Valid at startup but expires almost immediately.
	writeCCCreds(t, path, "tok-short", "ref", time.Now().Add(10*time.Millisecond))

	src, err := NewCCTokenSource(path)
	if err != nil {
		t.Fatalf("NewCCTokenSource: %v", err)
	}

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
}

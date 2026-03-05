package anthropic

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSetupToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")

	creds := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-test123","refreshToken":"sk-ant-ort01-test456","expiresAt":1771770729992}}`
	os.WriteFile(path, []byte(creds), 0644)

	token, err := ReadSetupToken(path)
	if err != nil {
		t.Fatalf("ReadSetupToken: %v", err)
	}
	if token != "sk-ant-oat01-test123" {
		t.Errorf("token = %q, want %q", token, "sk-ant-oat01-test123")
	}
}

func TestReadSetupTokenMissingFile(t *testing.T) {
	_, err := ReadSetupToken("/nonexistent/path/credentials.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestReadSetupTokenInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	os.WriteFile(path, []byte("not json"), 0644)

	_, err := ReadSetupToken(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestReadSetupTokenEmptyToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	os.WriteFile(path, []byte(`{"claudeAiOauth":{"accessToken":""}}`), 0644)

	token, err := ReadSetupToken(path)
	if err != nil {
		t.Fatalf("ReadSetupToken: %v", err)
	}
	if token != "" {
		t.Errorf("token = %q, want empty", token)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	got := expandHome("~/test/path")
	want := home + "/test/path"
	if got != want {
		t.Errorf("expandHome(~/test/path) = %q, want %q", got, want)
	}

	// Non-~ path unchanged
	got = expandHome("/absolute/path")
	if got != "/absolute/path" {
		t.Errorf("expandHome(/absolute/path) = %q", got)
	}
}

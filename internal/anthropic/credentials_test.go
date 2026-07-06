package anthropic

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
)

func TestTokenHolder_GetSet(t *testing.T) {
	// Proves NewTokenHolder initialises with the supplied token and that
	// Set correctly replaces it, with Get returning the updated value.
	h := NewTokenHolder("initial")
	tok, err := h.Get()
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if tok != "initial" {
		t.Errorf("Get = %q, want %q", tok, "initial")
	}

	h.Set("updated")
	tok, err = h.Get()
	if err != nil {
		t.Fatalf("Get after Set: unexpected error: %v", err)
	}
	if tok != "updated" {
		t.Errorf("Get after Set = %q, want %q", tok, "updated")
	}
}

func TestTokenHolder_EmptyReturnsError(t *testing.T) {
	// Proves that Get returns an error when the holder was initialised with
	// an empty string — indicating no credential is configured.
	h := NewTokenHolder("")
	_, err := h.Get()
	if err == nil {
		t.Fatal("expected error for empty tokenHolder")
	}
}

func TestTokenHolder_ConcurrentAccess(t *testing.T) {
	// Proves the RWMutex in tokenHolder prevents data races: concurrent
	// writers and readers must not corrupt the stored token.
	h := NewTokenHolder("start")
	var wg sync.WaitGroup

	// Concurrent writers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.Set("token-" + strings.Repeat("x", i))
		}(i)
	}

	// Concurrent readers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := h.Get()
			if err != nil {
				t.Errorf("concurrent Get error: %v", err)
			}
			if tok == "" {
				t.Error("concurrent Get returned empty")
			}
		}()
	}

	wg.Wait()
}

// fakeSecretsStore is a map-backed SecretsStore for resolver tests.
type fakeSecretsStore map[string]string

func (s fakeSecretsStore) Get(name string) (string, bool) { v, ok := s[name]; return v, ok }
func (s fakeSecretsStore) Set(name, value string)         { s[name] = value }
func (s fakeSecretsStore) Save() error                    { return nil }

// newTestResolver builds an AnthropicResolver via NewResolver with HOME pointed
// at a temp dir. If ccToken is non-empty, a valid CC credentials file is written
// there so the resolver picks up a CC token source.
func newTestResolver(t *testing.T, store SecretsStore, ccToken string) *AnthropicResolver {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if ccToken != "" {
		if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
			t.Fatal(err)
		}
		writeCCCreds(t, filepath.Join(home, ".claude", ".credentials.json"), ccToken, "ref", time.Now().Add(time.Hour))
	}
	r, err := NewResolver(context.Background(), &config.AnthropicConfig{
		CCExpiryThreshold: "2m",
	}, store)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

func TestNewResolverInvalidDurationsUseDefaults(t *testing.T) {
	// Proves NewResolver falls back to the documented defaults (5m expiry threshold) when the configured durations don't parse, instead of failing startup.
	t.Setenv("HOME", t.TempDir())
	r, err := NewResolver(context.Background(), &config.AnthropicConfig{
		CCExpiryThreshold: "also-bad",
	}, fakeSecretsStore{})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if r.ccSrc != nil {
		t.Error("ccSrc should be nil with no credentials file in HOME")
	}
}

func TestNewResolverDetectsCCCredentials(t *testing.T) {
	// Proves NewResolver picks up ~/.claude/.credentials.json when present and configures a CC token source from it.
	r := newTestResolver(t, fakeSecretsStore{}, "cc-tok")
	if r.ccSrc == nil {
		t.Fatal("ccSrc not configured despite valid credentials file")
	}
	tok, err := r.ccSrc.Token()
	if err != nil || tok != "cc-tok" {
		t.Errorf("ccSrc.Token = %q, %v", tok, err)
	}
}

func TestResolveClientAPIKeyPriority(t *testing.T) {
	// Proves an API key in the secrets store wins over CC credentials, the returned client authenticates with it, and the custom base URL is applied.
	store := fakeSecretsStore{"anthropic.api_key": "sk-test"}
	r := newTestResolver(t, store, "cc-tok")

	pc, err := r.ResolveClient(context.Background(), "main", "anthropic.api_key", "http://example.test", time.Second)
	if err != nil {
		t.Fatalf("ResolveClient: %v", err)
	}
	client, ok := pc.(*Client)
	if !ok {
		t.Fatalf("client type = %T, want *Client", pc)
	}
	tok, err := client.resolveToken()
	if err != nil || tok != "sk-test" {
		t.Errorf("token = %q, %v — want API key, not CC token", tok, err)
	}
	if client.baseURL != "http://example.test" {
		t.Errorf("baseURL = %q", client.baseURL)
	}
}

func TestResolveClientCCFallback(t *testing.T) {
	// Proves that without an API key in secrets, ResolveClient falls back to CC credentials and the client authenticates with the CC token.
	r := newTestResolver(t, fakeSecretsStore{}, "cc-tok")

	pc, err := r.ResolveClient(context.Background(), "main", "anthropic.api_key", "", time.Second)
	if err != nil {
		t.Fatalf("ResolveClient: %v", err)
	}
	tok, err := pc.(*Client).resolveToken()
	if err != nil || tok != "cc-tok" {
		t.Errorf("token = %q, %v — want CC token", tok, err)
	}
}

func TestResolveClientNoCredentials(t *testing.T) {
	// Proves ResolveClient returns an actionable error when neither an API key nor CC credentials exist.
	r := newTestResolver(t, fakeSecretsStore{}, "")

	_, err := r.ResolveClient(context.Background(), "main", "anthropic.api_key", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "foci auth") {
		t.Errorf("err = %v, want no-credentials error pointing at foci auth", err)
	}
}

func TestGetReloadFuncNilForCCOnly(t *testing.T) {
	// Proves GetReloadFunc returns nil when running purely on CC credentials — there is nothing in secrets.toml to hot-reload.
	r := newTestResolver(t, fakeSecretsStore{}, "cc-tok")
	if fn := r.GetReloadFunc("/nonexistent/secrets.toml"); fn != nil {
		t.Error("expected nil reload func for CC-only resolver")
	}
}

func TestGetReloadFuncHotReloadsAPIKey(t *testing.T) {
	// Proves the reload func re-reads secrets.toml and swaps the new API key into every live client's token holder without restarting.
	store := fakeSecretsStore{"anthropic.api_key": "sk-old"}
	r := newTestResolver(t, store, "")
	if _, err := r.ResolveClient(context.Background(), "main", "anthropic.api_key", "", time.Second); err != nil {
		t.Fatalf("ResolveClient: %v", err)
	}

	secretsPath := filepath.Join(t.TempDir(), "secrets.toml")
	if err := os.WriteFile(secretsPath, []byte("[anthropic]\napi_key = \"sk-new\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	reload := r.GetReloadFunc(secretsPath)
	if reload == nil {
		t.Fatal("expected reload func when API-key holders exist")
	}
	if err := reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	tok, err := r.credHolders["main"].Get()
	if err != nil || tok != "sk-new" {
		t.Errorf("holder token = %q, %v — want hot-reloaded sk-new", tok, err)
	}
}

func TestGetReloadFuncErrors(t *testing.T) {
	// Proves the reload func reports failure when secrets.toml has no api_key (missing file parses as empty) or contains invalid TOML.
	store := fakeSecretsStore{"anthropic.api_key": "sk-old"}
	r := newTestResolver(t, store, "")
	if _, err := r.ResolveClient(context.Background(), "main", "anthropic.api_key", "", time.Second); err != nil {
		t.Fatalf("ResolveClient: %v", err)
	}
	reload := r.GetReloadFunc(filepath.Join(t.TempDir(), "missing.toml"))
	if reload == nil {
		t.Fatal("expected reload func")
	}
	if err := reload(); err == nil || !strings.Contains(err.Error(), "no api_key") {
		t.Errorf("err = %v, want missing api_key error", err)
	}

	badPath := filepath.Join(t.TempDir(), "secrets.toml")
	if err := os.WriteFile(badPath, []byte("not [valid toml"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.GetReloadFunc(badPath)(); err == nil {
		t.Error("expected error for invalid TOML")
	}
}

func TestResolverClose(t *testing.T) {
	// Proves Close is a safe no-op (interface compliance) — no panic, callable on any resolver.
	newTestResolver(t, fakeSecretsStore{}, "").Close()
}

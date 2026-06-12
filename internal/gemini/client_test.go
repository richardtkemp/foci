package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/provider"

	"google.golang.org/genai"
)

// Canned response bodies for the fake Gemini API.
const (
	fakeGenerateBody    = `{"responseId":"resp_1","candidates":[{"content":{"parts":[{"text":"hello from gemini"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`
	fakeCacheCreateBody = `{"name":"cachedContents/test-cache-1"}`
	fakeCacheUpdateBody = `{"name":"cachedContents/test-cache-1"}`
	fakeFreeTierBody    = `{"error":{"code":429,"message":"quota exceeded: TotalCachedContentStorageTokensPerModelFreeTier, limit=0","status":"RESOURCE_EXHAUSTED"}}`
)

// fakeResponse is a canned HTTP response for one fake API route.
type fakeResponse struct {
	status int
	body   string
}

// fakeAPI is an http.Handler that simulates the Gemini REST API. It routes
// requests by URL/method to canned responses and records each call so tests
// can assert on request counts, paths, and bodies.
type fakeAPI struct {
	mu        sync.Mutex
	responses map[string]fakeResponse
	calls     map[string]int
	lastBody  map[string][]byte
	lastPath  map[string]string
}

// newFakeAPI returns a fakeAPI with happy-path responses for all routes.
func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		responses: map[string]fakeResponse{
			"generate":    {http.StatusOK, fakeGenerateBody},
			"count":       {http.StatusOK, `{"totalTokens":42}`},
			"cacheCreate": {http.StatusOK, fakeCacheCreateBody},
			"cacheUpdate": {http.StatusOK, fakeCacheUpdateBody},
			"cacheDelete": {http.StatusOK, `{}`},
		},
		calls:    make(map[string]int),
		lastBody: make(map[string][]byte),
		lastPath: make(map[string]string),
	}
}

// set overrides the canned response for a route.
func (f *fakeAPI) set(route string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[route] = fakeResponse{status, body}
}

func (f *fakeAPI) callCount(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[route]
}

func (f *fakeAPI) body(route string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.lastBody[route])
}

func (f *fakeAPI) path(route string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPath[route]
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := classifyRoute(r)
	body, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.calls[route]++
	f.lastBody[route] = body
	f.lastPath[route] = r.URL.Path
	resp, ok := f.responses[route]
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":{"code":501,"message":"unexpected route ` + route + `"}}`))
		return
	}
	w.WriteHeader(resp.status)
	_, _ = w.Write([]byte(resp.body))
}

// classifyRoute maps a request to one of the fake API's route keys.
func classifyRoute(r *http.Request) string {
	switch {
	case strings.Contains(r.URL.Path, ":generateContent"):
		return "generate"
	case strings.Contains(r.URL.Path, ":countTokens"):
		return "count"
	case strings.Contains(r.URL.Path, "cachedContents"):
		switch r.Method {
		case http.MethodPost:
			return "cacheCreate"
		case http.MethodPatch:
			return "cacheUpdate"
		case http.MethodDelete:
			return "cacheDelete"
		}
	}
	return "unknown"
}

// newTestClient starts an httptest server backed by f and creates a production
// Client pointed at it via the SDK's GOOGLE_GEMINI_BASE_URL environment variable.
func newTestClient(t *testing.T, f *fakeAPI, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("GOOGLE_GEMINI_BASE_URL", srv.URL)

	c, err := NewClient(context.Background(), "test-api-key", opts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// newTestCacheManager starts an httptest server backed by f and returns a
// CacheManager wired to a raw genai client pointed at it.
func newTestCacheManager(t *testing.T, f *fakeAPI, ttl time.Duration) *CacheManager {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	gc, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey:      "test-api-key",
		Backend:     genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{BaseURL: srv.URL},
	})
	if err != nil {
		t.Fatalf("genai.NewClient: %v", err)
	}
	return NewCacheManager(gc, ttl)
}

// testSystem returns a simple system content for cache tests.
func testSystem(text string) *genai.Content {
	return &genai.Content{Parts: []*genai.Part{{Text: text}}, Role: "user"}
}

func TestNewClient_Metadata(t *testing.T) {
	// Proves that NewClient applies options correctly: Endpoint and HandlesOwnRetries report fixed values, and caching availability follows whether WithCacheTTL was given.
	c := newTestClient(t, newFakeAPI(), WithHTTPTimeout(5*time.Second))
	if c.Endpoint() != "Gemini API" {
		t.Errorf("Endpoint = %q, want Gemini API", c.Endpoint())
	}
	if !c.HandlesOwnRetries() {
		t.Error("HandlesOwnRetries should be true (SDK retries internally)")
	}
	if c.IsCachingAvailable() {
		t.Error("caching should be unavailable without WithCacheTTL")
	}

	cached := newTestClient(t, newFakeAPI(), WithCacheTTL(time.Hour))
	if !cached.IsCachingAvailable() {
		t.Error("caching should be available with WithCacheTTL")
	}
}

func TestSendMessage_TextResponse(t *testing.T) {
	// Proves the full request/response round trip: the developer prefix is stripped from the model in the URL, system/tools/thinking are included in the request body, and the JSON response is translated to provider format.
	f := newFakeAPI()
	c := newTestClient(t, f)

	resp, err := c.SendMessage(context.Background(), &provider.MessageRequest{
		Model:     "google/gemini-2.5-flash",
		MaxTokens: 1000,
		System:    []provider.SystemBlock{{Type: "text", Text: "be helpful"}},
		Messages:  []provider.Message{{Role: "user", Content: provider.TextContent("hi")}},
		Tools: []provider.ToolDef{
			provider.NewCustomTool("exec", "run commands", json.RawMessage(`{"type":"object"}`)),
		},
		Thinking: &provider.ThinkingConfig{BudgetTokens: 2000},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Developer prefix stripped: URL path should contain bare model ID.
	path := f.path("generate")
	if !strings.Contains(path, "models/gemini-2.5-flash") || strings.Contains(path, "google") {
		t.Errorf("path = %q, want bare model gemini-2.5-flash", path)
	}

	// System, tools, and thinking all present in request body.
	body := f.body("generate")
	for _, want := range []string{"systemInstruction", "be helpful", "functionDeclarations", "thinkingConfig"} {
		if !strings.Contains(body, want) {
			t.Errorf("request body missing %q: %s", want, body)
		}
	}

	if got := provider.TextOf(resp.Content); got != "hello from gemini" {
		t.Errorf("text = %q", got)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want input 10 / output 5", resp.Usage)
	}
}

func TestSendMessage_APIError(t *testing.T) {
	// Proves that an HTTP error from the API is classified into a provider.APIError with the matching status code rather than leaking SDK error types.
	f := newFakeAPI()
	f.set("generate", http.StatusTooManyRequests, `{"error":{"code":429,"message":"slow down","status":"RESOURCE_EXHAUSTED"}}`)
	c := newTestClient(t, f)

	_, err := c.SendMessage(context.Background(), &provider.MessageRequest{
		Model:    "gemini-2.5-flash",
		Messages: []provider.Message{{Role: "user", Content: provider.TextContent("hi")}},
	})
	apiErr := &provider.APIError{}
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected provider.APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", apiErr.StatusCode)
	}
}

func TestSendMessage_CacheLifecycle(t *testing.T) {
	// Proves the explicit-cache flow: the first send creates a cache and strips system/tools from the request (replaced by cachedContent), the second send reuses the cache without recreating it, and Close deletes it.
	f := newFakeAPI()
	c := newTestClient(t, f, WithCacheTTL(time.Hour))

	req := &provider.MessageRequest{
		Model:    "gemini-2.5-flash",
		System:   []provider.SystemBlock{{Type: "text", Text: "be helpful"}},
		Messages: []provider.Message{{Role: "user", Content: provider.TextContent("hi")}},
	}

	if _, err := c.SendMessage(context.Background(), req); err != nil {
		t.Fatalf("SendMessage 1: %v", err)
	}
	if got := f.callCount("cacheCreate"); got != 1 {
		t.Fatalf("cacheCreate calls = %d, want 1", got)
	}
	body := f.body("generate")
	if !strings.Contains(body, "cachedContents/test-cache-1") {
		t.Errorf("request should reference cached content: %s", body)
	}
	if strings.Contains(body, "systemInstruction") {
		t.Errorf("system instruction should be omitted when cache is used: %s", body)
	}

	// Second send reuses the cache: no new create call.
	if _, err := c.SendMessage(context.Background(), req); err != nil {
		t.Fatalf("SendMessage 2: %v", err)
	}
	if got := f.callCount("cacheCreate"); got != 1 {
		t.Errorf("cacheCreate calls after reuse = %d, want 1", got)
	}

	c.Close(context.Background())
	if got := f.callCount("cacheDelete"); got != 1 {
		t.Errorf("cacheDelete calls = %d, want 1", got)
	}
}

func TestSendMessage_FreeTierDisablesCaching(t *testing.T) {
	// Proves that a free-tier cache-creation failure (429 with limit=0) permanently disables caching — the message still succeeds with inline system instruction and later sends skip cache creation entirely.
	f := newFakeAPI()
	f.set("cacheCreate", http.StatusTooManyRequests, fakeFreeTierBody)
	c := newTestClient(t, f, WithCacheTTL(time.Hour))

	req := &provider.MessageRequest{
		Model:    "gemini-2.5-flash",
		System:   []provider.SystemBlock{{Type: "text", Text: "be helpful"}},
		Messages: []provider.Message{{Role: "user", Content: provider.TextContent("hi")}},
	}

	if _, err := c.SendMessage(context.Background(), req); err != nil {
		t.Fatalf("SendMessage should succeed without cache: %v", err)
	}
	if !strings.Contains(f.body("generate"), "systemInstruction") {
		t.Error("system instruction should be sent inline when caching fails")
	}
	if c.IsCachingAvailable() {
		t.Error("caching should be marked unavailable after free-tier detection")
	}

	if _, err := c.SendMessage(context.Background(), req); err != nil {
		t.Fatalf("SendMessage 2: %v", err)
	}
	if got := f.callCount("cacheCreate"); got != 1 {
		t.Errorf("cacheCreate calls = %d, want 1 (no retry after free-tier detection)", got)
	}
}

func TestClient_CloseWithoutCache(t *testing.T) {
	// Proves that Close on a client without caching enabled is a safe no-op and issues no API calls.
	f := newFakeAPI()
	c := newTestClient(t, f)
	c.Close(context.Background())
	if got := f.callCount("cacheDelete"); got != 0 {
		t.Errorf("cacheDelete calls = %d, want 0", got)
	}
}

func TestCountTokens(t *testing.T) {
	// Proves that CountTokens strips the developer prefix from the model, folds system and tools into the counted contents (the SDK rejects them in the count config for this backend), and returns the API's total token count.
	f := newFakeAPI()
	c := newTestClient(t, f)

	n, err := c.CountTokens(context.Background(), &provider.MessageRequest{
		Model:    "google/gemini-2.5-flash",
		System:   []provider.SystemBlock{{Type: "text", Text: "be helpful"}},
		Messages: []provider.Message{{Role: "user", Content: provider.TextContent("hi")}},
		Tools: []provider.ToolDef{
			provider.NewCustomTool("exec", "run commands", json.RawMessage(`{"type":"object"}`)),
		},
	})
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if n != 42 {
		t.Errorf("tokens = %d, want 42", n)
	}
	if path := f.path("count"); !strings.Contains(path, "models/gemini-2.5-flash") {
		t.Errorf("path = %q, want bare model gemini-2.5-flash", path)
	}
	// System text and tool declarations must be folded into contents so they count.
	body := f.body("count")
	for _, want := range []string{"be helpful", "exec"} {
		if !strings.Contains(body, want) {
			t.Errorf("count request body missing %q: %s", want, body)
		}
	}
}

func TestCountTokens_Error(t *testing.T) {
	// Proves that an API failure during token counting is surfaced as a wrapped error rather than a zero count being silently returned.
	f := newFakeAPI()
	f.set("count", http.StatusInternalServerError, `{"error":{"code":500,"message":"boom","status":"INTERNAL"}}`)
	c := newTestClient(t, f)

	_, err := c.CountTokens(context.Background(), &provider.MessageRequest{
		Model:    "gemini-2.5-flash",
		Messages: []provider.Message{{Role: "user", Content: provider.TextContent("hi")}},
	})
	if err == nil || !strings.Contains(err.Error(), "count tokens") {
		t.Errorf("err = %v, want wrapped count tokens error", err)
	}
}

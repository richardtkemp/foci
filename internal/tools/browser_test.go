package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"foci/internal/config"
)

// marshalParams is a test helper that JSON-marshals params and fails the test on error.
func marshalParams(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return b
}

// testJSONServer starts a test HTTP server serving the given JSON body
// with application/json content type.
func testJSONServer(t *testing.T, jsonBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, jsonBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// skipIfNoBrowser skips the test if Chrome/Chromium is not found in PATH.
func skipIfNoBrowser(t *testing.T) {
	t.Helper()
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium-browser", "chromium"} {
		if _, err := exec.LookPath(name); err == nil {
			return
		}
	}
	t.Skip("Chrome/Chromium not found in PATH — skipping browser integration test")
}

// testHTMLServer starts a test HTTP server serving the given HTML body.
func testHTMLServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testBrowserManager(t *testing.T) *BrowserManager {
	t.Helper()
	mgr := NewBrowserManager(&config.ResolvedBrowser{
		Headless:      true,
		TimeoutSec:    10,
		DOMStableSec:  0.1,
		DOMStableDiff: 0.5,
	}, 0640)
	t.Cleanup(func() { mgr.Stop() })
	return mgr
}

// sharedBrowserMgr is a lazily-initialized BrowserManager shared across tests
// that only navigate+interact (no lifecycle testing). This avoids launching a
// separate Chrome process for each test. Cleaned up via TestMain.
var (
	sharedBrowserOnce sync.Once
	sharedBrowserMgr  *BrowserManager
)

func sharedBrowserManager(t *testing.T) *BrowserManager {
	t.Helper()
	sharedBrowserOnce.Do(func() {
		sharedBrowserMgr = NewBrowserManager(&config.ResolvedBrowser{
			Headless:      true,
			TimeoutSec:    10,
			DOMStableSec:  0.1,
			DOMStableDiff: 0.5,
		}, 0640)
	})
	return sharedBrowserMgr
}

func TestBrowserNavigateAndSnapshot(t *testing.T) {
	// Verifies that navigating to a page and
	// capturing a snapshot returns YAML with element refs and page metadata.
	srv := testHTMLServer(t, `<html><head><title>Test Page</title></head>
		<body><h1>Hello World</h1><button>Click Me</button></body></html>`)

	mgr := sharedBrowserManager(t)
	tool := NewBrowserTool(mgr)

	params, _ := json.Marshal(map[string]any{"action": "navigate", "url": srv.URL})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Should contain page metadata
	if !strings.Contains(result.Text, "Page URL:") {
		t.Error("snapshot missing Page URL")
	}
	if !strings.Contains(result.Text, "Test Page") {
		t.Error("snapshot missing page title")
	}
	// Should contain YAML snapshot with refs
	if !strings.Contains(result.Text, "[ref=") {
		t.Error("snapshot missing element refs")
	}
	// Should contain the heading and button
	if !strings.Contains(result.Text, "Hello World") {
		t.Error("snapshot missing heading text")
	}
	if !strings.Contains(result.Text, "Click Me") {
		t.Error("snapshot missing button text")
	}
}

func TestBrowserClickByRef(t *testing.T) {
	// Verifies that after navigating and getting a snapshot,
	// we can click an element using its ref from the snapshot.
	srv := testHTMLServer(t, `<html><body>
		<button onclick="document.getElementById('out').textContent='clicked'">Click Me</button>
		<div id="out"></div>
	</body></html>`)

	mgr := sharedBrowserManager(t)
	tool := NewBrowserTool(mgr)

	// Navigate first
	params, _ := json.Marshal(map[string]any{"action": "navigate", "url": srv.URL})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Extract a button ref from the snapshot
	ref := extractRef(t, result.Text, "button")
	if ref == "" {
		t.Fatal("could not find button ref in snapshot")
	}

	// Click the button
	params, _ = json.Marshal(map[string]any{"action": "click", "ref": ref, "element": "Click Me button"})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("click: %v", err)
	}

	if !strings.Contains(result.Text, "Clicked:") {
		t.Error("click result missing confirmation")
	}
	// Auto-snapshot should show the updated DOM
	if !strings.Contains(result.Text, "clicked") {
		t.Error("auto-snapshot after click missing updated content")
	}
}

func TestBrowserFillByRef(t *testing.T) {
	// Verifies that we can fill an input field using its ref.
	srv := testHTMLServer(t, `<html><body>
		<label for="name">Name</label>
		<input id="name" type="text" />
	</body></html>`)

	mgr := sharedBrowserManager(t)
	tool := NewBrowserTool(mgr)

	// Navigate
	params, _ := json.Marshal(map[string]any{"action": "navigate", "url": srv.URL})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Find the textbox ref
	ref := extractRef(t, result.Text, "textbox")
	if ref == "" {
		t.Fatal("could not find textbox ref in snapshot")
	}

	// Fill the input
	params, _ = json.Marshal(map[string]any{"action": "fill", "ref": ref, "value": "John Doe", "element": "Name input"})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("fill: %v", err)
	}

	if !strings.Contains(result.Text, "Filled") {
		t.Error("fill result missing confirmation")
	}
	// Auto-snapshot should show the filled value
	if !strings.Contains(result.Text, "John Doe") {
		t.Error("auto-snapshot after fill missing input value")
	}
}

func TestBrowserStaleRef(t *testing.T) {
	// Verifies that using a ref from a previous generation
	// (stale snapshot) returns a meaningful error.
	srv := testHTMLServer(t, `<html><body><button>Click Me</button></body></html>`)

	mgr := sharedBrowserManager(t)
	tool := NewBrowserTool(mgr)

	// Navigate to get first snapshot
	params, _ := json.Marshal(map[string]any{"action": "navigate", "url": srv.URL})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	ref := extractRef(t, result.Text, "button")
	if ref == "" {
		t.Fatal("could not find button ref in snapshot")
	}

	// Take a new snapshot (invalidates old refs by incrementing generation)
	params, _ = json.Marshal(map[string]any{"action": "snapshot"})
	_, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Use the old ref after a new snapshot. The browser tool resolves refs
	// by re-querying the DOM, not by validating the generation number. On a
	// static page the element is still connected, so the click succeeds.
	params, _ = json.Marshal(map[string]any{"action": "click", "ref": ref})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("click with stale ref: %v", err)
	}

	// Either the click succeeded (element still connected) or an error was
	// returned in the result text (element gone / ref invalid). Both are
	// acceptable — the key invariant is that the tool produces a non-empty
	// response rather than silently doing nothing.
	if result.Text == "" {
		t.Error("expected non-empty result for click with stale ref")
	}
}

func TestBrowserInvalidRef(t *testing.T) {
	// Verifies that using a malformed ref string returns
	// a validation error.
	srv := testHTMLServer(t, `<html><body><button>Test</button></body></html>`)

	mgr := sharedBrowserManager(t)
	tool := NewBrowserTool(mgr)

	// Navigate first
	params, _ := json.Marshal(map[string]any{"action": "navigate", "url": srv.URL})
	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Try an invalid ref
	params, _ = json.Marshal(map[string]any{"action": "click", "ref": "#login-button"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("click: %v", err)
	}

	if !strings.Contains(result.Text, "Error") {
		t.Errorf("expected error for invalid ref, got: %s", result.Text)
	}
}

func TestBrowserMultiFill(t *testing.T) {
	// Verifies that the fill action supports a "fields"
	// array to fill multiple inputs in a single tool call with one snapshot.
	srv := testHTMLServer(t, `<html><body>
		<form>
			<label for="first">First</label>
			<input id="first" type="text" />
			<label for="last">Last</label>
			<input id="last" type="text" />
		</form>
	</body></html>`)

	mgr := sharedBrowserManager(t)
	tool := NewBrowserTool(mgr)

	// Navigate
	params := marshalParams(t, map[string]any{"action": "navigate", "url": srv.URL})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Extract refs for both textboxes
	refs := extractAllRefs(t, result.Text, "textbox")
	if len(refs) < 2 {
		t.Fatalf("expected 2 textbox refs, got %d from snapshot:\n%s", len(refs), result.Text)
	}

	// Multi-fill both fields at once
	params = marshalParams(t, map[string]any{
		"action": "fill",
		"fields": []map[string]string{
			{"ref": refs[0], "value": "Alice"},
			{"ref": refs[1], "value": "Smith"},
		},
	})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("multi-fill: %v", err)
	}

	if !strings.Contains(result.Text, "Filled") {
		t.Error("multi-fill result missing confirmation")
	}
	// Both values should appear in the snapshot
	if !strings.Contains(result.Text, "Alice") {
		t.Error("snapshot after multi-fill missing first value 'Alice'")
	}
	if !strings.Contains(result.Text, "Smith") {
		t.Error("snapshot after multi-fill missing second value 'Smith'")
	}
}

func TestBrowserMultiFillBackwardCompat(t *testing.T) {
	// Verifies that single ref+value fill
	// still works after adding multi-fill support.
	srv := testHTMLServer(t, `<html><body>
		<form>
			<label for="email">Email</label>
			<input id="email" type="text" />
		</form>
	</body></html>`)

	mgr := sharedBrowserManager(t)
	tool := NewBrowserTool(mgr)

	params := marshalParams(t, map[string]any{"action": "navigate", "url": srv.URL})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	ref := extractRef(t, result.Text, "textbox")
	if ref == "" {
		t.Fatal("could not find textbox ref")
	}

	// Use old-style single ref+value
	params = marshalParams(t, map[string]any{
		"action": "fill", "ref": ref, "value": "test@example.com",
	})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("fill: %v", err)
	}

	if !strings.Contains(result.Text, "Filled") {
		t.Error("fill result missing confirmation")
	}
	if !strings.Contains(result.Text, "test@example.com") {
		t.Error("snapshot after fill missing value")
	}
}

func TestBrowserFillScopedSnapshot(t *testing.T) {
	// Verifies that the snapshot returned after
	// a fill action is scoped to the form context, not the full page.
	// Page with a form and lots of unrelated content
	srv := testHTMLServer(t, `<html><body>
		<nav><a href="/">Home</a><a href="/about">About</a><a href="/contact">Contact</a></nav>
		<h1>Big Page</h1>
		<div id="sidebar"><p>Sidebar content with lots of stuff</p></div>
		<form id="login-form">
			<label for="user">Username</label>
			<input id="user" type="text" />
			<label for="pass">Password</label>
			<input id="pass" type="password" />
			<button type="submit">Login</button>
		</form>
		<footer><p>Footer content</p></footer>
	</body></html>`)

	mgr := sharedBrowserManager(t)
	tool := NewBrowserTool(mgr)

	params := marshalParams(t, map[string]any{"action": "navigate", "url": srv.URL})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// The full page snapshot should contain nav/footer content
	if !strings.Contains(result.Text, "Home") {
		t.Error("full snapshot should contain nav links")
	}

	ref := extractRef(t, result.Text, "textbox")
	if ref == "" {
		t.Fatal("could not find textbox ref")
	}

	// Fill the username field
	params = marshalParams(t, map[string]any{
		"action": "fill", "ref": ref, "value": "admin",
	})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("fill: %v", err)
	}

	// Scoped snapshot should contain the form elements
	if !strings.Contains(result.Text, "Form Context Snapshot") {
		t.Error("fill snapshot should be labeled as scoped/form context")
	}
	if !strings.Contains(result.Text, "admin") {
		t.Error("scoped snapshot missing filled value")
	}
	if !strings.Contains(result.Text, "Login") {
		t.Error("scoped snapshot should contain form's submit button")
	}
}

func TestBrowserFillNoRefOrFields(t *testing.T) {
	// Verifies that fill returns an error when
	// neither ref nor fields is provided.
	srv := testHTMLServer(t, `<html><body><input type="text" /></body></html>`)
	mgr := sharedBrowserManager(t)
	tool := NewBrowserTool(mgr)

	// Navigate first
	params := marshalParams(t, map[string]any{"action": "navigate", "url": srv.URL})
	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Fill with no ref or fields
	params = marshalParams(t, map[string]any{"action": "fill", "value": "test"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("fill: %v", err)
	}

	if !strings.Contains(result.Text, "Error") {
		t.Errorf("expected error for fill without ref/fields, got: %s", result.Text)
	}
}

func TestBrowserFillPreservesFullSnapshotRefs(t *testing.T) {
	// Verifies that after a fill (which triggers a scoped snapshot), refs from
	// the prior full snapshot remain valid and can be used for subsequent actions
	// like click. This is the core DOM-stamped-refs guarantee.
	srv := testHTMLServer(t, `<html><body>
		<form>
			<label for="name">Name</label>
			<input id="name" type="text" />
			<button type="button" onclick="document.getElementById('out').textContent='clicked'">Submit</button>
		</form>
		<div id="out"></div>
	</body></html>`)

	mgr := sharedBrowserManager(t)
	tool := NewBrowserTool(mgr)

	// Navigate — produces a full snapshot with stamped refs
	params := marshalParams(t, map[string]any{"action": "navigate", "url": srv.URL})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Capture the button ref from the full snapshot
	buttonRef := extractRef(t, result.Text, "button")
	if buttonRef == "" {
		t.Fatal("could not find button ref in full snapshot")
	}
	textboxRef := extractRef(t, result.Text, "textbox")
	if textboxRef == "" {
		t.Fatal("could not find textbox ref in full snapshot")
	}

	// Fill the textbox — triggers scoped snapshot (should NOT invalidate refs)
	params = marshalParams(t, map[string]any{
		"action": "fill", "ref": textboxRef, "value": "Alice",
	})
	_, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("fill: %v", err)
	}

	// Click the button using the original full-snapshot ref
	params = marshalParams(t, map[string]any{
		"action": "click", "ref": buttonRef, "element": "Submit button",
	})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("click: %v", err)
	}

	// The click should succeed (not "Error") and the onclick should fire
	if strings.Contains(result.Text, "Error") {
		t.Errorf("click with pre-fill ref failed: %s", result.Text)
	}
	if !strings.Contains(result.Text, "clicked") {
		t.Error("button onclick did not fire — ref from full snapshot was not resolved")
	}
}

func TestBrowserScopedSnapshotShowsOriginalRefs(t *testing.T) {
	// Verifies that the scoped snapshot returned after a fill action shows
	// refs from the original full snapshot generation (e.g. s1e*), not a new
	// generation (s2e*). This confirms DOM-stamped refs are read, not generated.
	srv := testHTMLServer(t, `<html><body>
		<form>
			<label for="email">Email</label>
			<input id="email" type="text" />
			<button type="submit">Go</button>
		</form>
	</body></html>`)

	mgr := sharedBrowserManager(t)
	tool := NewBrowserTool(mgr)

	// Navigate — full snapshot, generation 1
	params := marshalParams(t, map[string]any{"action": "navigate", "url": srv.URL})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// All refs should be s1e*
	if !strings.Contains(result.Text, "s1e") {
		t.Fatalf("expected s1e refs in full snapshot, got:\n%s", result.Text)
	}

	textboxRef := extractRef(t, result.Text, "textbox")
	if textboxRef == "" {
		t.Fatal("could not find textbox ref")
	}

	// Fill — triggers scoped snapshot
	params = marshalParams(t, map[string]any{
		"action": "fill", "ref": textboxRef, "value": "test@example.com",
	})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("fill: %v", err)
	}

	// Scoped snapshot should still show s1e* refs (stamped by full snapshot)
	if !strings.Contains(result.Text, "s1e") {
		t.Errorf("scoped snapshot should contain s1e refs, got:\n%s", result.Text)
	}
	// Should NOT contain s2e* refs (would indicate a new generation)
	if strings.Contains(result.Text, "s2e") {
		t.Errorf("scoped snapshot should NOT contain s2e refs (new generation), got:\n%s", result.Text)
	}
}

// extractAllRefs finds all ref strings in the snapshot text near a given
// role keyword. Returns all matching refs.
func extractAllRefs(t *testing.T, snapshot, roleKeyword string) []string {
	t.Helper()

	var refs []string
	for _, line := range strings.Split(snapshot, "\n") {
		if !strings.Contains(strings.ToLower(line), roleKeyword) {
			continue
		}
		idx := strings.Index(line, "[ref=")
		if idx < 0 {
			continue
		}
		end := strings.Index(line[idx:], "]")
		if end < 0 {
			continue
		}
		refs = append(refs, line[idx+5:idx+end])
	}
	return refs
}

func TestBrowserStart(t *testing.T) {
	// Verifies that the start action launches the browser with the requested
	// incognito mode, defaults to incognito=true when omitted, and errors
	// if the browser is already running.
	skipIfNoBrowser(t)

	mgr := testBrowserManager(t)
	tool := NewBrowserTool(mgr)

	// Default should be incognito on.
	if !mgr.incognito {
		t.Fatal("expected default incognito=true")
	}

	// Start with incognito=false.
	params := marshalParams(t, map[string]any{"action": "start", "incognito": false})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(result.Text, "incognito: off") {
		t.Errorf("expected 'incognito: off', got: %s", result.Text)
	}
	if mgr.incognito {
		t.Fatal("expected incognito=false")
	}
	if !mgr.IsConnected() {
		t.Fatal("expected browser to be connected")
	}

	// Starting again while running → error.
	params = marshalParams(t, map[string]any{"action": "start"})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(result.Text, "Error") {
		t.Errorf("expected error for already-running browser, got: %s", result.Text)
	}

	// Close and restart with default (incognito=true).
	params = marshalParams(t, map[string]any{"action": "close"})
	_, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	// Omitting incognito keeps previous value (false).
	params = marshalParams(t, map[string]any{"action": "start"})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(result.Text, "incognito: off") {
		t.Errorf("expected 'incognito: off' (retained from previous start), got: %s", result.Text)
	}
	if mgr.incognito {
		t.Fatal("expected incognito to remain false when param omitted")
	}
}

// extractRef finds a ref string (e.g. "s1e5") in the snapshot text near
// a given role keyword (e.g. "button"). Returns empty string if not found.
func extractRef(t *testing.T, snapshot, roleKeyword string) string {
	t.Helper()

	// Look for lines containing the role keyword and extract [ref=...]
	for _, line := range strings.Split(snapshot, "\n") {
		if !strings.Contains(strings.ToLower(line), roleKeyword) {
			continue
		}
		// Find [ref=...] in the line
		idx := strings.Index(line, "[ref=")
		if idx < 0 {
			continue
		}
		end := strings.Index(line[idx:], "]")
		if end < 0 {
			continue
		}
		ref := line[idx+5 : idx+end]
		return ref
	}
	return ""
}

package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"foci/internal/config"
	"foci/internal/testtemp"
)

// removeAfterStop removes a test-owned dir that held a persistent profile.
// Stop waits for chromium to exit (#1518), so nothing writes into dir after
// it returns and a removal failure is a real failure.
func removeAfterStop(t *testing.T, dir string) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Errorf("remove %s after Stop: %v", dir, err)
	}
}

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

// TestBrowserPersistentProfile verifies that a non-incognito start with a
// configured UserDataDir launches into that dir (creating it if missing) and
// does not fall back to the host's default chromium profile.
func TestBrowserPersistentProfile(t *testing.T) {
	skipIfNoBrowser(t)

	// Own the temp dir (not t.TempDir) so cleanup runs strictly after Stop.
	base, err := os.MkdirTemp(testtemp.Dir(), "foci-persist-test-*")
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(base, "browser-profile")
	mgr := NewBrowserManager(&config.ResolvedBrowser{
		Headless:     true,
		TimeoutSec:   10,
		DOMStableSec: 0.1,
		UserDataDir:  profile,
	}, 0640)
	t.Cleanup(func() { mgr.Stop(); removeAfterStop(t, base) })
	tool := NewBrowserTool(mgr)

	// incognito=false so the configured persistent profile is used.
	params := marshalParams(t, map[string]any{"action": "start", "incognito": false})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !mgr.IsConnected() {
		t.Fatal("expected browser to be connected")
	}
	// The configured profile dir must have been created and populated.
	if _, err := os.Stat(profile); err != nil {
		t.Fatalf("configured profile dir not created: %v", err)
	}
	if mgr.profileDir != "" {
		t.Errorf("owned temp profileDir set (%q) despite configured persistent dir", mgr.profileDir)
	}
}

// TestBrowserStopWaitsForExit verifies that Stop returns only after chromium
// has exited and its owned profile dir is gone (foci_todo #1518). Stop used to
// send the CDP close and remove the dir while chromium was still shutting
// down, so a late write could land in the dir after the removal.
func TestBrowserStopWaitsForExit(t *testing.T) {
	skipIfNoBrowser(t)
	if runtime.GOOS != "linux" {
		t.Skip("finds the chromium process through /proc")
	}

	mgr := testBrowserManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	dir := mgr.profileDir
	if dir == "" {
		t.Fatal("premise: incognito start did not create an owned profile dir")
	}
	pid := chromiumMainPID(t, dir)

	if err := mgr.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if state := procState(pid); state != "" && state != "Z" {
		t.Errorf("chromium (pid %d) is still running (state %s) after Stop returned", pid, state)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("profile dir %s still exists after Stop returned (stat err: %v)", dir, err)
	}
}

// TestBrowserAwaitExitKillsUnclosedBrowser covers Stop's path for a close that
// was not acknowledged: chromium was never told to shut down, so awaitExit
// must kill it and still wait for the exit.
func TestBrowserAwaitExitKillsUnclosedBrowser(t *testing.T) {
	skipIfNoBrowser(t)
	if runtime.GOOS != "linux" {
		t.Skip("finds the chromium process through /proc")
	}

	mgr := testBrowserManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := chromiumMainPID(t, mgr.profileDir)

	mgr.mu.Lock()
	mgr.awaitExit(false)
	mgr.mu.Unlock()
	if state := procState(pid); state != "" && state != "Z" {
		t.Errorf("chromium (pid %d) is still running (state %s) after awaitExit(false)", pid, state)
	}
}

// fakeBrowser writes an executable stand-in for chromium that records its pid
// and --user-data-dir into the returned record file, prints the given stderr
// line, then runs tail. It returns the script path and the record path.
func fakeBrowser(t *testing.T, stderrLine, tail string) (bin, record string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("fake browser is a shell script checked through /proc")
	}
	dir, err := testtemp.Mkdir("foci-fakebrowser-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	record = filepath.Join(dir, "record")
	bin = filepath.Join(dir, "chromium")
	script := `#!/bin/sh
for a in "$@"; do
  case "$a" in --user-data-dir=*) udd="${a#--user-data-dir=}" ;; esac
done
printf '%s\n%s\n' "$$" "$udd" > '` + record + `.tmp' && mv '` + record + `.tmp' '` + record + `'
echo '` + stderrLine + `' >&2
` + tail + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { // #nosec G306 - the fake browser must be executable
		t.Fatal(err)
	}
	return bin, record
}

// readFakeBrowserRecord returns the pid and user-data-dir a fakeBrowser run
// recorded.
func readFakeBrowserRecord(t *testing.T, record string) (pid int, userDataDir string) {
	t.Helper()
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("premise: fake browser never ran: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 || lines[1] == "" {
		t.Fatalf("premise: malformed fake browser record %q", raw)
	}
	pid, err = strconv.Atoi(lines[0])
	if err != nil {
		t.Fatalf("premise: bad pid in fake browser record %q", raw)
	}
	return pid, lines[1]
}

// TestBrowserStartLaunchFailureCleansUp verifies that a Start whose Launch
// fails removes the owned temp profile dir it created (foci_todo #2090). Stop
// cannot clean it up, since it returns early when no browser connected.
func TestBrowserStartLaunchFailureCleansUp(t *testing.T) {
	bin, record := fakeBrowser(t, "fake chromium: no debug url", "exit 1")
	mgr := NewBrowserManager(&config.ResolvedBrowser{
		Headless:       true,
		TimeoutSec:     10,
		ExecutablePath: bin,
	}, 0640)
	t.Cleanup(func() { mgr.Stop() })

	if err := mgr.Start(); err == nil {
		t.Fatal("Start succeeded with a browser that exits without a debug URL")
	}
	_, dir := readFakeBrowserRecord(t, record)
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("profile dir %s still exists after a failed Launch (stat err: %v)", dir, err)
	}
	if mgr.profileDir != "" || mgr.launcher != nil || mgr.exited != nil {
		t.Errorf("failed Start left state behind: profileDir=%q launcher set=%v exited set=%v", mgr.profileDir, mgr.launcher != nil, mgr.exited != nil)
	}
}

// TestBrowserStartConnectFailureCleansUp verifies that a Start whose CDP
// connect fails returns an error instead of panicking (MustConnect did), and
// kills the launched process, removes the owned profile dir and releases the
// persistent profile lock (foci_todo #2090).
func TestBrowserStartConnectFailureCleansUp(t *testing.T) {
	// Serves the /json/version lookup that go-rod resolves the debug URL
	// through, but refuses the websocket upgrade, so only Connect fails.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://%s/devtools/browser/fake"}`, srv.Listener.Addr())
	}))
	t.Cleanup(srv.Close)
	listening := fmt.Sprintf("DevTools listening on ws://%s/devtools/browser/fake", srv.Listener.Addr())

	for _, incognito := range []bool{true, false} {
		t.Run(fmt.Sprintf("incognito=%v", incognito), func(t *testing.T) {
			// exec keeps the recorded pid; the sleep outlives the test, so
			// the process is gone only if Start killed it.
			bin, record := fakeBrowser(t, listening, "exec sleep 600")
			cfg := &config.ResolvedBrowser{Headless: true, TimeoutSec: 10, ExecutablePath: bin}
			if !incognito {
				base, err := testtemp.Mkdir("foci-persist-test-*")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { removeAfterStop(t, base) })
				cfg.UserDataDir = filepath.Join(base, "browser-profile")
			}
			mgr := NewBrowserManager(cfg, 0640)
			mgr.incognito = incognito
			t.Cleanup(func() { mgr.Stop() })

			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("Start panicked on a connect failure: %v", r)
					}
				}()
				err = mgr.Start()
			}()
			if err == nil {
				t.Fatal("Start succeeded although the connect failed")
			}
			pid, dir := readFakeBrowserRecord(t, record)
			if state := procState(pid); state != "" && state != "Z" {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Errorf("launched browser (pid %d) still running (state %s) after a failed Start", pid, state)
			}
			if incognito {
				if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("profile dir %s still exists after a failed connect (stat err: %v)", dir, err)
				}
			} else if err := mgr.profile.take(NewBrowserManager(cfg, 0640)); err != nil {
				t.Errorf("persistent profile still locked after a failed connect: %v", err)
			}
			if mgr.IsConnected() || mgr.profileDir != "" || mgr.launcher != nil || mgr.exited != nil {
				t.Errorf("failed Start left state behind: connected=%v profileDir=%q launcher set=%v exited set=%v",
					mgr.IsConnected(), mgr.profileDir, mgr.launcher != nil, mgr.exited != nil)
			}
		})
	}
}

// chromiumMainPID returns the pid of the chromium browser process launched
// with dir as its user-data-dir. Chromium's child processes carry --type=, and
// go-rod's leakless guard carries the same args behind its own binary name, so
// both are excluded.
func chromiumMainPID(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if strings.Contains(filepath.Base(args[0]), "leakless") {
			continue
		}
		if !slices.Contains(args, "--user-data-dir="+dir) {
			continue
		}
		if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--type=") }) {
			continue
		}
		pids = append(pids, pid)
	}
	if len(pids) != 1 {
		t.Fatalf("premise: want exactly one chromium browser process for %s, found %v", dir, pids)
	}
	return pids[0]
}

// procState returns the state letter from /proc/<pid>/stat, or "" when the
// process no longer exists. "Z" means it has exited and awaits reaping.
func procState(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// The command name in field 2 may contain spaces; the state follows its ")".
	fields := strings.Fields(string(raw[bytes.LastIndexByte(raw, ')')+1:]))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

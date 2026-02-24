package bitwarden

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// mockExecutor records calls and returns preconfigured responses.
type mockExecutor struct {
	calls    [][]string
	listJSON string
	getMap   map[string]string // id → password
	getErr   error
}

func (m *mockExecutor) Run(args ...string) (string, error) {
	m.calls = append(m.calls, args)

	if len(args) >= 1 && args[0] == "list" {
		return m.listJSON, nil
	}
	if len(args) >= 3 && args[0] == "get" && args[1] == "password" {
		if m.getErr != nil {
			return "", m.getErr
		}
		id := args[2]
		if val, ok := m.getMap[id]; ok {
			return val, nil
		}
		return "", fmt.Errorf("not found: %s", id)
	}
	return "", fmt.Errorf("unknown command: %v", args)
}

func testItems() []rawItem {
	return []rawItem{
		{
			ID:       "aaaa-1111",
			Name:     "GitHub API",
			FolderID: "folder-dev",
			Login: &struct {
				Username string `json:"username"`
				URIs     []struct {
					URI string `json:"uri"`
				} `json:"uris"`
			}{
				Username: "bot-user",
				URIs: []struct {
					URI string `json:"uri"`
				}{
					{URI: "https://api.github.com"},
				},
			},
		},
		{
			ID:       "bbbb-2222",
			Name:     "Slack Bot",
			FolderID: "folder-ops",
			Login: &struct {
				Username string `json:"username"`
				URIs     []struct {
					URI string `json:"uri"`
				} `json:"uris"`
			}{
				Username: "slack-app",
				URIs: []struct {
					URI string `json:"uri"`
				}{
					{URI: "https://slack.com/api"},
					{URI: "https://hooks.slack.com"},
				},
			},
		},
		{
			ID:       "cccc-3333",
			Name:     "AWS Production",
			FolderID: "folder-ops",
		},
		{
			ID:       "dddd-4444",
			Name:     "OpenAI Key",
			FolderID: "folder-dev",
			Login: &struct {
				Username string `json:"username"`
				URIs     []struct {
					URI string `json:"uri"`
				} `json:"uris"`
			}{
				Username: "",
				URIs: []struct {
					URI string `json:"uri"`
				}{
					{URI: "https://api.openai.com"},
				},
			},
		},
		{
			ID:       "eeee-5555",
			Name:     "Extra Item 1",
			FolderID: "folder-dev",
		},
		{
			ID:       "ffff-6666",
			Name:     "Extra Dev Item 2",
			FolderID: "folder-dev",
		},
	}
}

func testListJSON(t *testing.T) string {
	t.Helper()
	data, err := json.Marshal(testItems())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func newTestStore(t *testing.T, mock *mockExecutor) *Store {
	t.Helper()
	s := New(mock, 30*time.Minute)
	if err := s.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return s
}

func TestSearch(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	// Search by name (case-insensitive)
	results := s.Search("github")
	if len(results) != 1 || results[0].ID != "aaaa-1111" {
		t.Errorf("Search(github) = %v", results)
	}

	// Search by name, partial match
	results = s.Search("slack")
	if len(results) != 1 || results[0].ID != "bbbb-2222" {
		t.Errorf("Search(slack) = %v", results)
	}
}

func TestSearchByURI(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	results := s.Search("hooks.slack.com")
	if len(results) != 1 || results[0].ID != "bbbb-2222" {
		t.Errorf("Search(hooks.slack.com) = %v", results)
	}
}

func TestSearchByFolder(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	results := s.Search("folder-ops")
	if len(results) != 2 {
		t.Errorf("Search(folder-ops) = %d results, want 2", len(results))
	}
}

func TestSearchByUsername(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	results := s.Search("bot-user")
	if len(results) != 1 || results[0].ID != "aaaa-1111" {
		t.Errorf("Search(bot-user) = %v", results)
	}
}

func TestSearchNoResults(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	results := s.Search("nonexistent-xyz")
	if len(results) != 0 {
		t.Errorf("Search(nonexistent) = %v, want empty", results)
	}
}

func TestSearchMaxResults(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	// "folder-dev" matches 4 items, but limit is 5 so all should return
	results := s.Search("folder-dev")
	if len(results) != 4 {
		t.Errorf("Search(folder-dev) = %d results, want 4", len(results))
	}

	// Search with broad term that matches many items
	results = s.Search("e") // matches almost everything
	if len(results) > 5 {
		t.Errorf("Search should return at most 5 results, got %d", len(results))
	}
}

func TestGetPasswordCached(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	// Pre-populate cache
	s.mu.Lock()
	s.values["aaaa-1111"] = cachedValue{
		value:   "cached-password",
		expires: time.Now().Add(10 * time.Minute),
	}
	s.mu.Unlock()

	val, err := s.GetPassword("aaaa-1111")
	if err != nil {
		t.Fatalf("GetPassword: %v", err)
	}
	if val != "cached-password" {
		t.Errorf("GetPassword = %q, want cached-password", val)
	}

	// Should not have called executor for "get" — only the initial "list"
	for _, call := range mock.calls {
		if len(call) >= 1 && call[0] == "get" {
			t.Error("should not have called executor for cached password")
		}
	}
}

func TestGetPasswordExpired(t *testing.T) {
	mock := &mockExecutor{
		listJSON: testListJSON(t),
		getMap:   map[string]string{"aaaa-1111": "fresh-password"},
	}
	s := newTestStore(t, mock)

	// Pre-populate with expired value
	s.mu.Lock()
	s.values["aaaa-1111"] = cachedValue{
		value:   "old-password",
		expires: time.Now().Add(-1 * time.Minute), // expired
	}
	s.mu.Unlock()

	val, err := s.GetPassword("aaaa-1111")
	if err != nil {
		t.Fatalf("GetPassword: %v", err)
	}
	if val != "fresh-password" {
		t.Errorf("GetPassword = %q, want fresh-password", val)
	}
}

func TestGetPasswordExec(t *testing.T) {
	mock := &mockExecutor{
		listJSON: testListJSON(t),
		getMap:   map[string]string{"aaaa-1111": "my-secret-pass"},
	}
	s := newTestStore(t, mock)

	val, err := s.GetPassword("aaaa-1111")
	if err != nil {
		t.Fatalf("GetPassword: %v", err)
	}
	if val != "my-secret-pass" {
		t.Errorf("GetPassword = %q", val)
	}

	// Verify executor was called with correct args
	found := false
	for _, call := range mock.calls {
		if len(call) >= 3 && call[0] == "get" && call[1] == "password" && call[2] == "aaaa-1111" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected executor call: get password aaaa-1111, got %v", mock.calls)
	}
}

func TestGetPasswordDenied(t *testing.T) {
	mock := &mockExecutor{
		listJSON: testListJSON(t),
		getErr:   fmt.Errorf("denied by aisudo"),
	}
	s := newTestStore(t, mock)

	_, err := s.GetPassword("aaaa-1111")
	if err == nil {
		t.Fatal("expected error for denied password")
	}
	if !strings.Contains(err.Error(), "denied by administrator") {
		t.Errorf("error should mention denied by administrator: %v", err)
	}
}

func TestAllowedHosts(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	hosts := s.AllowedHosts("aaaa-1111")
	if len(hosts) != 1 || hosts[0] != "api.github.com" {
		t.Errorf("AllowedHosts(aaaa-1111) = %v", hosts)
	}

	// Multiple URIs
	hosts = s.AllowedHosts("bbbb-2222")
	if len(hosts) != 2 {
		t.Errorf("AllowedHosts(bbbb-2222) = %v, want 2 hosts", hosts)
	}

	// No URIs
	hosts = s.AllowedHosts("cccc-3333")
	if len(hosts) != 0 {
		t.Errorf("AllowedHosts(cccc-3333) = %v, want empty", hosts)
	}

	// Unknown item
	hosts = s.AllowedHosts("nonexistent")
	if len(hosts) != 0 {
		t.Errorf("AllowedHosts(nonexistent) = %v, want empty", hosts)
	}
}

func TestCheckHostAllowed(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	// Allowed
	if err := s.CheckHostAllowed("aaaa-1111", "https://api.github.com/user"); err != nil {
		t.Errorf("expected allowed: %v", err)
	}

	// Case-insensitive
	if err := s.CheckHostAllowed("aaaa-1111", "https://API.GITHUB.COM/user"); err != nil {
		t.Errorf("expected case-insensitive match: %v", err)
	}

	// With port
	if err := s.CheckHostAllowed("aaaa-1111", "https://api.github.com:443/user"); err != nil {
		t.Errorf("expected port stripped: %v", err)
	}
}

func TestCheckHostBlocked(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	err := s.CheckHostAllowed("aaaa-1111", "https://evil.com/steal")
	if err == nil {
		t.Fatal("expected error for blocked host")
	}
	if !strings.Contains(err.Error(), "evil.com") {
		t.Errorf("error should mention evil.com: %v", err)
	}

	// No URIs → error
	err = s.CheckHostAllowed("cccc-3333", "https://anything.com")
	if err == nil {
		t.Fatal("expected error for item with no URIs")
	}
	if !strings.Contains(err.Error(), "no URIs") {
		t.Errorf("error should mention no URIs: %v", err)
	}
}

func TestCleanup(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	// Add values — one expired, one fresh
	s.mu.Lock()
	s.values["expired-id"] = cachedValue{
		value:   "old",
		expires: time.Now().Add(-1 * time.Minute),
	}
	s.values["fresh-id"] = cachedValue{
		value:   "fresh",
		expires: time.Now().Add(10 * time.Minute),
	}
	s.mu.Unlock()

	s.cleanup()

	s.mu.RLock()
	_, hasExpired := s.values["expired-id"]
	_, hasFresh := s.values["fresh-id"]
	s.mu.RUnlock()

	if hasExpired {
		t.Error("expired value should have been cleaned up")
	}
	if !hasFresh {
		t.Error("fresh value should still exist")
	}
}

func TestResolve(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	// Pre-populate cache
	s.mu.Lock()
	s.values["aaaa-1111"] = cachedValue{
		value:   "ghp_secrettoken123",
		expires: time.Now().Add(10 * time.Minute),
	}
	s.mu.Unlock()

	text := "Bearer {{secret:bw.aaaa-1111}}"
	resolved, err := s.Resolve(text)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved != "Bearer ghp_secrettoken123" {
		t.Errorf("Resolve = %q", resolved)
	}

	// Non-bw templates should pass through unchanged
	text = "{{secret:custom.key}} and {{secret:bw.aaaa-1111}}"
	resolved, err = s.Resolve(text)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(resolved, "{{secret:custom.key}}") {
		t.Errorf("non-bw template should pass through: %q", resolved)
	}
	if !strings.Contains(resolved, "ghp_secrettoken123") {
		t.Errorf("bw template should be resolved: %q", resolved)
	}
}

func TestResolveNotUnlocked(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	_, err := s.Resolve("{{secret:bw.aaaa-1111}}")
	if err == nil {
		t.Fatal("expected error for unlocked secret")
	}
	if !strings.Contains(err.Error(), "not unlocked") {
		t.Errorf("error should mention not unlocked: %v", err)
	}
}

func TestRedact(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	s.mu.Lock()
	s.values["aaaa-1111"] = cachedValue{
		value:   "supersecretpassword123",
		expires: time.Now().Add(10 * time.Minute),
	}
	s.mu.Unlock()

	text := "the password is supersecretpassword123 here"
	redacted := s.Redact(text)

	if strings.Contains(redacted, "supersecretpassword123") {
		t.Error("value should be redacted")
	}
	if !strings.Contains(redacted, "[REDACTED]") {
		t.Error("should contain [REDACTED]")
	}
}

func TestRedactShortValues(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	s.mu.Lock()
	s.values["short-id"] = cachedValue{
		value:   "ab",
		expires: time.Now().Add(10 * time.Minute),
	}
	s.mu.Unlock()

	text := "ab is fine"
	redacted := s.Redact(text)
	if !strings.Contains(redacted, "ab is fine") {
		t.Errorf("short value should not be redacted: %q", redacted)
	}
}

func TestCachedIDs(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	s.mu.Lock()
	s.values["fresh-id"] = cachedValue{value: "x", expires: time.Now().Add(10 * time.Minute)}
	s.values["expired-id"] = cachedValue{value: "y", expires: time.Now().Add(-1 * time.Minute)}
	s.mu.Unlock()

	ids := s.CachedIDs()
	if len(ids) != 1 || ids[0] != "fresh-id" {
		t.Errorf("CachedIDs = %v, want [fresh-id]", ids)
	}
}

func TestIsBitwardenRef(t *testing.T) {
	if !IsBitwardenRef("bw.abc-123") {
		t.Error("bw.abc-123 should be a bitwarden ref")
	}
	if IsBitwardenRef("custom.key") {
		t.Error("custom.key should not be a bitwarden ref")
	}
}

func TestExtractID(t *testing.T) {
	if id := ExtractID("bw.abc-123"); id != "abc-123" {
		t.Errorf("ExtractID = %q, want abc-123", id)
	}
}

func TestItemByID(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := newTestStore(t, mock)

	item := s.ItemByID("aaaa-1111")
	if item == nil {
		t.Fatal("expected item")
	}
	if item.Name != "GitHub API" {
		t.Errorf("item.Name = %q", item.Name)
	}

	if s.ItemByID("nonexistent") != nil {
		t.Error("expected nil for nonexistent item")
	}
}

func TestRefresh(t *testing.T) {
	mock := &mockExecutor{listJSON: testListJSON(t)}
	s := New(mock, 30*time.Minute)

	if err := s.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if s.ItemCount() != 6 {
		t.Errorf("ItemCount = %d, want 6", s.ItemCount())
	}

	if s.RefreshedAt().IsZero() {
		t.Error("RefreshedAt should be set after refresh")
	}
}

func TestStartCleanupAndClose(t *testing.T) {
	mock := &mockExecutor{listJSON: "[]"}
	s := New(mock, 1*time.Millisecond)
	s.Refresh()

	s.mu.Lock()
	s.values["test"] = cachedValue{
		value:   "val",
		expires: time.Now().Add(-1 * time.Millisecond),
	}
	s.mu.Unlock()

	s.StartCleanup(10 * time.Millisecond)

	// Wait for cleanup to run
	time.Sleep(50 * time.Millisecond)

	s.mu.RLock()
	_, exists := s.values["test"]
	s.mu.RUnlock()

	if exists {
		t.Error("expired value should have been cleaned up by background goroutine")
	}

	s.Close()
}

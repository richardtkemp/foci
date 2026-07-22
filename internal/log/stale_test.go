package log

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// replaceFileExternally simulates the exact on-disk failure mode behind
// foci_todo #1479: something outside the app (not our own rotation, which
// uses os.Rename and always pairs it with log.Reopen()) removes the file at
// path and creates a fresh, empty one in its place — while a process still
// holds an open, append-mode fd from before the swap. That old fd keeps
// writing into the now-unlinked, invisible inode; nothing sees those bytes
// again. Confirmed via the running foci-gw's fd table on 2026-07-22
// (api.jsonl/api-payload.jsonl fds pointed at "(deleted)" inodes growing at
// several MB/hour while the visible files stayed 0 bytes).
func replaceFileExternally(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("recreate %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close recreated %s: %v", path, err)
	}
}

func TestStaleFile(t *testing.T) {
	// Verifies the core detection primitive: staleFile compares dev+inode
	// between an open fd and a fresh stat of its path.
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")
	os.WriteFile(path, []byte("hello\n"), 0644)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	if staleFile(f, path) {
		t.Error("staleFile should be false immediately after opening the current file")
	}

	replaceFileExternally(t, path)

	if !staleFile(f, path) {
		t.Error("staleFile should be true after the path was replaced underneath the open fd")
	}

	os.Remove(path)
	if !staleFile(f, path) {
		t.Error("staleFile should be true when the path no longer exists at all")
	}

	if staleFile(nil, path) {
		t.Error("staleFile should be false for a nil fd (nothing to detect)")
	}
	if staleFile(f, "") {
		t.Error("staleFile should be false for an empty path (not configured)")
	}
}

func TestEventLogReopensOnExternalReplace(t *testing.T) {
	// Regression test for foci_todo #1479: verifies that if something external
	// (not our own rotate+Reopen pair) replaces foci.log out from under the
	// open writer, the NEXT event() call detects the stale inode, reopens the
	// file at the current path, and logs a WARN about it — instead of
	// silently continuing to write into the orphaned, unlinked inode forever.
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	eventPath := filepath.Join(dir, "foci.log")

	if err := Init(Config{Level: "INFO", EventFile: eventPath}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	Infof("test", "before replace")

	replaceFileExternally(t, eventPath)

	Infof("test", "after replace")

	data, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	content := string(data)

	if strings.Contains(content, "before replace") {
		t.Errorf("the file at eventPath is the NEW inode created by replaceFileExternally — it should not contain data written to the old (now orphaned) fd:\n%s", content)
	}
	if !strings.Contains(content, "after replace") {
		t.Errorf("event log should contain the post-reopen write:\n%s", content)
	}
	if !strings.Contains(content, "stale inode") {
		t.Errorf("event log should contain a WARN about the detected stale inode / reopen:\n%s", content)
	}
}

func TestAPILogReopensOnExternalReplace(t *testing.T) {
	// api.jsonl analogue of TestEventLogReopensOnExternalReplace — this is
	// the file that was actually observed 0 bytes and silently discarding
	// writes for #1479.
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	eventPath := filepath.Join(dir, "foci.log") // to capture the WARN
	apiPath := filepath.Join(dir, "api.jsonl")

	if err := Init(Config{Level: "INFO", EventFile: eventPath, APIFile: apiPath}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	API(APIEntry{Session: "before", Model: "test"})

	replaceFileExternally(t, apiPath)

	API(APIEntry{Session: "after", Model: "test"})

	data, err := os.ReadFile(apiPath)
	if err != nil {
		t.Fatalf("read api log: %v", err)
	}
	content := string(data)
	if strings.Contains(content, "before") {
		t.Errorf("api.jsonl should not contain the pre-replace entry (it went to the orphaned inode):\n%s", content)
	}
	if !strings.Contains(content, "after") {
		t.Errorf("api.jsonl should contain the post-reopen entry:\n%s", content)
	}
	// Exactly one well-formed JSON line, from after the reopen.
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 line in api.jsonl after reopen, got %d: %v", len(lines), lines)
	}
	var entry APIEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal api entry: %v", err)
	}
	if entry.Session != "after" {
		t.Errorf("Session = %q, want %q", entry.Session, "after")
	}

	eventData, _ := os.ReadFile(eventPath)
	if !strings.Contains(string(eventData), "stale inode") {
		t.Errorf("event log should contain a WARN about the api.jsonl stale inode:\n%s", string(eventData))
	}
}

func TestPayloadLogReopensOnExternalReplace(t *testing.T) {
	// api-payload.jsonl analogue — the exact file named in foci_todo #1479.
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	eventPath := filepath.Join(dir, "foci.log")
	payloadPath := filepath.Join(dir, "api-payload.jsonl")

	if err := Init(Config{
		Level:       "INFO",
		EventFile:   eventPath,
		PayloadFile: payloadPath,
		FullPayload: true,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	Payload(PayloadEntry{Session: "before", Model: "test"})

	replaceFileExternally(t, payloadPath)

	Payload(PayloadEntry{Session: "after", Model: "test"})

	data, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("read payload log: %v", err)
	}
	content := string(data)
	if strings.Contains(content, "before") {
		t.Errorf("api-payload.jsonl should not contain the pre-replace entry (it went to the orphaned inode):\n%s", content)
	}
	if !strings.Contains(content, "after") {
		t.Errorf("api-payload.jsonl should contain the post-reopen entry:\n%s", content)
	}

	eventData, _ := os.ReadFile(eventPath)
	if !strings.Contains(string(eventData), "stale inode") {
		t.Errorf("event log should contain a WARN about the payload log stale inode:\n%s", string(eventData))
	}
}

func TestReopenIfStaleDoesNotRecurseForever(t *testing.T) {
	// Regression test for a bug caught while implementing #1479's fix: if
	// reopen keeps failing (e.g. the replacement path became permanently
	// unwritable), naively logging the failure via the normal event()/Warnf()
	// path re-enters the very staleness check that produced it, recursing
	// without bound and crashing the process with a stack overflow. This
	// exercises exactly the scenario TestReopenEventError uses (an
	// intentionally broken eventPath) but through the write path (event())
	// rather than direct Reopen(), and simply requires that the call returns
	// instead of blowing the stack.
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	eventPath := filepath.Join(dir, "foci.log")

	if err := Init(Config{Level: "INFO", EventFile: eventPath}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	// Break the path so every reopen attempt fails, without changing the
	// still-open, still-valid eventFile fd — staleFile() compares against the
	// (now nonexistent) path and will keep reporting stale forever.
	std.mu.Lock()
	std.eventPath = "/nonexistent/dir/foci.log"
	std.mu.Unlock()

	for i := 0; i < 20; i++ {
		Infof("test", "attempt %d", i)
	}
	// No panic/stack overflow = pass.
}

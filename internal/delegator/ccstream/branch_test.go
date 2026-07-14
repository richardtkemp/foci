package ccstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/delegator"
)

const (
	testParentUUID = "862f8727-9059-4821-87a4-5c248a1648e6"
	testWorkDir    = "/home/foci/clutch"
)

// writeParentTranscript creates a fake CC transcript under a temp HOME and
// returns (home, parentPath). Each line carries sessionId=parentUUID plus a
// DISTINCT per-message uuid, mirroring a real CC .jsonl.
func writeParentTranscript(t *testing.T) (home, parentPath string) {
	t.Helper()
	home = t.TempDir()
	dir := filepath.Join(home, ccProjectsDir, projectSlug(testWorkDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	parentPath = filepath.Join(dir, testParentUUID+".jsonl")
	lines := []string{
		`{"type":"queue-operation","sessionId":"` + testParentUUID + `","uuid":null,"parentUuid":null}`,
		`{"type":"user","sessionId":"` + testParentUUID + `","uuid":"f503bab1-3c20-4b2e-863b-f2ec5b545b7c","parentUuid":null}`,
		`{"type":"assistant","sessionId":"` + testParentUUID + `","uuid":"a1111111-2222-3333-4444-555555555555","parentUuid":"f503bab1-3c20-4b2e-863b-f2ec5b545b7c"}`,
	}
	if err := os.WriteFile(parentPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, parentPath
}

func TestForkSession(t *testing.T) {
	home, parentPath := writeParentTranscript(t)
	t.Setenv("HOME", home)

	b := &Backend{}
	res, err := b.ForkSession(context.Background(), delegator.ForkRequest{
		ParentSessionID: testParentUUID,
		WorkDir:         testWorkDir,
	})
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if res.SessionID == "" || res.SessionID == testParentUUID {
		t.Fatalf("bad forked session id: %q", res.SessionID)
	}

	newPath := filepath.Join(home, ccProjectsDir, projectSlug(testWorkDir), res.SessionID+".jsonl")
	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read forked transcript: %v", err)
	}
	content := string(data)

	// sessionId fully rewritten — parent UUID must not survive anywhere.
	if strings.Contains(content, testParentUUID) {
		t.Errorf("parent UUID still present in fork:\n%s", content)
	}
	// Every line's sessionId is the new id (3 lines).
	if got := strings.Count(content, `"sessionId":"`+res.SessionID+`"`); got != 3 {
		t.Errorf("expected 3 rewritten sessionId fields, got %d", got)
	}
	// Per-message uuid/parentUuid values preserved (not rewritten).
	for _, keep := range []string{
		"f503bab1-3c20-4b2e-863b-f2ec5b545b7c",
		"a1111111-2222-3333-4444-555555555555",
	} {
		if !strings.Contains(content, keep) {
			t.Errorf("per-message uuid %s was lost in fork", keep)
		}
	}
	// Parent transcript untouched.
	orig, _ := os.ReadFile(parentPath)
	if !strings.Contains(string(orig), testParentUUID) {
		t.Errorf("parent transcript was mutated")
	}
}

func TestForkSessionErrors(t *testing.T) {
	home, _ := writeParentTranscript(t)
	t.Setenv("HOME", home)
	b := &Backend{}

	cases := []struct {
		name string
		req  delegator.ForkRequest
	}{
		{"empty parent", delegator.ForkRequest{WorkDir: testWorkDir}},
		{"empty workdir", delegator.ForkRequest{ParentSessionID: testParentUUID}},
		{"truncate unsupported", delegator.ForkRequest{ParentSessionID: testParentUUID, WorkDir: testWorkDir, TruncateAfter: 5}},
		{"missing parent file", delegator.ForkRequest{ParentSessionID: "does-not-exist", WorkDir: testWorkDir}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := b.ForkSession(context.Background(), tc.req); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestCleanupSession(t *testing.T) {
	home, _ := writeParentTranscript(t)
	t.Setenv("HOME", home)
	b := &Backend{}

	// Fork, then clean up the fork — the file must be gone, parent untouched.
	res, err := b.ForkSession(context.Background(), delegator.ForkRequest{
		ParentSessionID: testParentUUID, WorkDir: testWorkDir,
	})
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	forkPath := filepath.Join(home, ccProjectsDir, projectSlug(testWorkDir), res.SessionID+".jsonl")
	if _, err := os.Stat(forkPath); err != nil {
		t.Fatalf("fork not created: %v", err)
	}
	if err := b.CleanupSession(context.Background(), delegator.CleanupRequest{
		SessionID: res.SessionID, WorkDir: testWorkDir,
	}); err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}
	if _, err := os.Stat(forkPath); !os.IsNotExist(err) {
		t.Errorf("fork transcript not deleted: stat err = %v", err)
	}
	// Parent still present.
	parentPath := filepath.Join(home, ccProjectsDir, projectSlug(testWorkDir), testParentUUID+".jsonl")
	if _, err := os.Stat(parentPath); err != nil {
		t.Errorf("parent transcript was deleted: %v", err)
	}

	// Deleting an already-absent session is not an error.
	if err := b.CleanupSession(context.Background(), delegator.CleanupRequest{
		SessionID: "nonexistent-uuid", WorkDir: testWorkDir,
	}); err != nil {
		t.Errorf("cleanup of missing session should be nil, got %v", err)
	}

	// Missing args error.
	if err := b.CleanupSession(context.Background(), delegator.CleanupRequest{WorkDir: testWorkDir}); err == nil {
		t.Error("expected error for empty session id")
	}
}

// TestForkSessionExclusive ensures a UUID collision fails loudly rather than
// clobbering an existing transcript.
func TestForkSessionExclusive(t *testing.T) {
	home, _ := writeParentTranscript(t)
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ccProjectsDir, projectSlug(testWorkDir))

	// Pre-create a transcript, then force forkTranscript to that exact dst.
	collide := filepath.Join(dir, "collision.jsonl")
	if err := os.WriteFile(collide, []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, testParentUUID+".jsonl")
	if err := forkTranscript(src, collide, testParentUUID, "new"); err == nil {
		t.Fatal("expected O_EXCL collision error, got nil")
	}
	// Existing file must be intact (not clobbered/removed).
	if data, _ := os.ReadFile(collide); string(data) != "existing\n" {
		t.Errorf("collision target was modified: %q", data)
	}
}

// assertAllValidJSONLines fails if any line in data isn't valid JSON or if data
// ends mid-line (no trailing newline) — the invariant every fork must uphold.
func assertAllValidJSONLines(t *testing.T, tag string, data []byte) int {
	t.Helper()
	if len(data) == 0 {
		return 0
	}
	if data[len(data)-1] != '\n' {
		t.Fatalf("%s: fork ended mid-line (no trailing newline):\n%q", tag, data)
	}
	n := 0
	for _, ln := range bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n")) {
		if len(ln) == 0 {
			continue
		}
		if !json.Valid(ln) {
			t.Fatalf("%s: fork emitted an invalid-JSON line: %q", tag, ln)
		}
		n++
	}
	return n
}

// TestForkTranscriptTailHandling covers the append-safe cut: a fork copies only
// whole, well-formed records and stops at a half-appended or torn trailing line.
func TestForkTranscriptTailHandling(t *testing.T) {
	const oldID, newID = "OLD", "NEW"
	l1 := `{"sessionId":"OLD","n":1}`
	l2 := `{"sessionId":"OLD","n":2}`

	cases := []struct {
		name      string
		raw       string
		wantLines int
	}{
		{"clean newline-terminated", l1 + "\n" + l2 + "\n", 2},
		{"partial trailing record (no newline)", l1 + "\n" + l2 + "\n" + `{"sessionId":"OLD","n":3`, 2},
		{"torn boundary: complete but invalid-JSON tail", l1 + "\n" + l2 + "\n" + "half-written-garbage\n", 2},
		{"single clean record", l1 + "\n", 1},
		{"empty file", "", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src.jsonl")
			dst := filepath.Join(dir, "dst.jsonl")
			if err := os.WriteFile(src, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := forkTranscript(src, dst, oldID, newID); err != nil {
				t.Fatalf("forkTranscript: %v", err)
			}
			data, err := os.ReadFile(dst)
			if err != nil {
				t.Fatalf("read dst: %v", err)
			}
			if got := assertAllValidJSONLines(t, tc.name, data); got != tc.wantLines {
				t.Errorf("fork emitted %d records, want %d\ncontent:\n%s", got, tc.wantLines, data)
			}
			if bytes.Contains(data, []byte(oldID)) {
				t.Errorf("parent id %q survived in fork:\n%s", oldID, data)
			}
			if tc.wantLines > 0 && !bytes.Contains(data, []byte(newID)) {
				t.Errorf("new id %q missing from fork:\n%s", newID, data)
			}
		})
	}
}

// TestForkTranscriptConcurrentAppend forks a transcript repeatedly WHILE a writer
// appends records in chunks (content split from its newline, to maximise the odds
// of catching a mid-line tail). Every fork must be an all-complete, all-valid-JSON
// prefix — never a torn line. This is the property that lets a fork run without
// quiescing the parent's writer.
func TestForkTranscriptConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jsonl")

	var seed bytes.Buffer
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&seed, `{"sessionId":"OLD","n":%d}`+"\n", i)
	}
	if err := os.WriteFile(src, seed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 200; i < 500; i++ {
			rec := fmt.Sprintf(`{"sessionId":"OLD","n":%d}`, i)
			// Content and newline in separate writes → an interleaved fork can
			// observe the record without its terminating newline yet.
			_, _ = f.WriteString(rec[:len(rec)/2])
			_, _ = f.WriteString(rec[len(rec)/2:])
			_, _ = f.WriteString("\n")
		}
	}()

	for i := 0; i < 60; i++ {
		dst := filepath.Join(dir, fmt.Sprintf("dst-%d.jsonl", i))
		if err := forkTranscript(src, dst, "OLD", "NEW"); err != nil {
			t.Fatalf("fork %d: %v", i, err)
		}
		data, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read fork %d: %v", i, err)
		}
		assertAllValidJSONLines(t, fmt.Sprintf("fork-%d", i), data)
	}
	<-done
}

package ccstream

import (
	"context"
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

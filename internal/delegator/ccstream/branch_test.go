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
	"foci/internal/log"
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
	if err := forkTranscript(src, collide, testParentUUID, "new", log.NewComponentLogger("test")); err == nil {
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
			if err := forkTranscript(src, dst, oldID, newID, log.NewComponentLogger("test")); err != nil {
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

// TestForkTranscriptSessionIDReplaceScopedToEnvelope is the red test for #1432:
// forkTranscript's session-id rewrite must touch ONLY the envelope's top-level
// "sessionId" field, never a session-id substring embedded inside historical
// tool-result text (e.g. an output_file path baked in at the time a background
// agent was launched). Verified live against a real branch: diffing the same
// task's output_file path across 5 sibling forks of one root session showed each
// fork's copy pointing at ITS OWN (wrong) session directory instead of the
// original, because the old blanket bytes.ReplaceAll rewrote that embedded
// substring too.
func TestForkTranscriptSessionIDReplaceScopedToEnvelope(t *testing.T) {
	const oldID, newID = "862f8727-9059-4821-87a4-5c248a1648e6", "NEWSESSIONID"
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jsonl")
	dst := filepath.Join(dir, "dst.jsonl")

	// A realistic async-launch tool_result envelope: the top-level "sessionId"
	// field should be rewritten, but the output_file path embedded in
	// toolUseResult (which happens to contain the same UUID, exactly as in the
	// real bug) must survive untouched — it names a real path outside this fork.
	line := `{"type":"user","sessionId":"` + oldID + `","uuid":"u1","parentUuid":null,` +
		`"toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"agent1",` +
		`"description":"Delegate todo 1400","outputFile":"/tmp/claude-994/-slug/` + oldID + `/tasks/agent1.output"}}` + "\n"
	if err := os.WriteFile(src, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := forkTranscript(src, dst, oldID, newID, log.NewComponentLogger("test")); err != nil {
		t.Fatalf("forkTranscript: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, `"sessionId":"`+newID+`"`) {
		t.Errorf("envelope sessionId was not rewritten to the new id:\n%s", content)
	}
	if strings.Contains(content, `"sessionId":"`+oldID+`"`) {
		t.Errorf("envelope sessionId still shows the old id:\n%s", content)
	}
	wantPath := "/tmp/claude-994/-slug/" + oldID + "/tasks/agent1.output"
	if !strings.Contains(content, wantPath) {
		t.Errorf("output_file path embedding the old session id was corrupted; want it preserved verbatim as %q, got:\n%s", wantPath, content)
	}
}

// TestForkTranscriptSynthesizesEndForOpenAsyncTask is the red test for #1431
// (option b, "synthesize-END"): a background (isAsync) subagent launch that
// never resolves within the copied prefix — the real completion, if any, only
// ever lands in the PARENT's future — must get a synthetic closing
// task-notification appended after the copy, so Claude Code's own resume-time
// reconciliation finds it already resolved instead of injecting its own stale
// stopped/failed notification (#1429). Traced live: the launch's structured
// marker is toolUseResult.status=="async_launched" with an "agentId"; a real
// resolution (completed/stopped/failed) is a task-notification-shaped message
// whose content embeds "<task-id>ID</task-id>".
func TestForkTranscriptSynthesizesEndForOpenAsyncTask(t *testing.T) {
	const oldID, newID = "OLD", "NEW"
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jsonl")
	dst := filepath.Join(dir, "dst.jsonl")

	lines := []string{
		`{"type":"user","sessionId":"OLD","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hi"}}`,
		// The Agent-tool async launch: dangling — no resolution follows.
		`{"type":"user","sessionId":"OLD","uuid":"u2","parentUuid":"u1",` +
			`"message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully. agentId: agent-open"}]}]},` +
			`"toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"agent-open","description":"Delegate todo 9999"}}`,
	}
	if err := os.WriteFile(src, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := forkTranscript(src, dst, oldID, newID, log.NewComponentLogger("test")); err != nil {
		t.Fatalf("forkTranscript: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	n := assertAllValidJSONLines(t, "synth-open", data)
	if n != len(lines)+1 {
		t.Fatalf("want %d lines (%d copied + 1 synthetic close), got %d:\n%s", len(lines)+1, len(lines), n, data)
	}

	recs := splitJSONLines(t, data)
	last := recs[len(recs)-1]
	if last["type"] != "user" {
		t.Errorf("synthetic close should be a %q message, got %v", "user", last["type"])
	}
	msg, _ := last["message"].(map[string]any)
	contentStr, _ := msg["content"].(string)
	if !strings.Contains(contentStr, "agent-open") {
		t.Errorf("synthetic close doesn't mention the open task id agent-open:\n%s", contentStr)
	}
	if !strings.Contains(contentStr, "<task-notification>") {
		t.Errorf("synthetic close isn't shaped like a task-notification:\n%s", contentStr)
	}
	// Must chain off the true last real line's uuid (u2), not float disconnected.
	if last["parentUuid"] != "u2" {
		t.Errorf("synthetic close parentUuid = %v, want it chained off the last real line's uuid u2", last["parentUuid"])
	}
	if last["sessionId"] != newID {
		t.Errorf("synthetic close sessionId = %v, want the FORK's new id %q", last["sessionId"], newID)
	}
	if u, _ := last["uuid"].(string); u == "" || u == "u2" {
		t.Errorf("synthetic close needs its own fresh uuid, got %q", u)
	}
}

// TestForkTranscriptSkipsClosedAsyncTask ensures the synthesis is additive-only:
// an async launch that DID resolve within the copied prefix (a task-notification
// for its task-id already present) must NOT get a redundant synthetic close.
func TestForkTranscriptSkipsClosedAsyncTask(t *testing.T) {
	const oldID, newID = "OLD", "NEW"
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jsonl")
	dst := filepath.Join(dir, "dst.jsonl")

	lines := []string{
		`{"type":"user","sessionId":"OLD","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hi"}}`,
		`{"type":"user","sessionId":"OLD","uuid":"u2","parentUuid":"u1",` +
			`"message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":[{"type":"text","text":"Async agent launched"}]}]},` +
			`"toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"agent-closed","description":"Delegate todo 1"}}`,
		// Resolution arrives before the fork point — already closed.
		`{"type":"user","sessionId":"OLD","uuid":"u3","parentUuid":"u2","message":{"role":"user","content":"<task-notification>\n<task-id>agent-closed</task-id>\n<status>completed</status>\n</task-notification>"}}`,
	}
	if err := os.WriteFile(src, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := forkTranscript(src, dst, oldID, newID, log.NewComponentLogger("test")); err != nil {
		t.Fatalf("forkTranscript: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	n := assertAllValidJSONLines(t, "synth-closed", data)
	if n != len(lines) {
		t.Errorf("closed task should get NO synthetic append; want %d lines, got %d:\n%s", len(lines), n, data)
	}
}

// TestForkTranscriptSynthesizesMultipleEndsChained covers >1 dangling task: each
// synthetic close must chain sequentially (one thread), not fan out as siblings
// of the same parent.
func TestForkTranscriptSynthesizesMultipleEndsChained(t *testing.T) {
	const oldID, newID = "OLD", "NEW"
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jsonl")
	dst := filepath.Join(dir, "dst.jsonl")

	lines := []string{
		`{"type":"user","sessionId":"OLD","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hi"}}`,
		`{"type":"user","sessionId":"OLD","uuid":"u2","parentUuid":"u1","toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"agentA","description":"A"}}`,
		`{"type":"user","sessionId":"OLD","uuid":"u3","parentUuid":"u2","toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"agentB","description":"B"}}`,
	}
	if err := os.WriteFile(src, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := forkTranscript(src, dst, oldID, newID, log.NewComponentLogger("test")); err != nil {
		t.Fatalf("forkTranscript: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	n := assertAllValidJSONLines(t, "synth-multi", data)
	if n != len(lines)+2 {
		t.Fatalf("want %d lines (%d copied + 2 synthetic closes), got %d:\n%s", len(lines)+2, len(lines), n, data)
	}
	recs := splitJSONLines(t, data)
	first, second := recs[len(recs)-2], recs[len(recs)-1]
	if first["parentUuid"] != "u3" {
		t.Errorf("first synthetic close parentUuid = %v, want u3 (last real line)", first["parentUuid"])
	}
	if second["parentUuid"] != first["uuid"] {
		t.Errorf("second synthetic close parentUuid = %v, want it chained off the first synthetic close's uuid %v", second["parentUuid"], first["uuid"])
	}
}

// splitJSONLines decodes every line of data as a generic JSON object, in order.
func splitJSONLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n")) {
		if len(ln) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(ln, &m); err != nil {
			t.Fatalf("line isn't a JSON object: %v\n%s", err, ln)
		}
		out = append(out, m)
	}
	return out
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
		if err := forkTranscript(src, dst, "OLD", "NEW", log.NewComponentLogger("test")); err != nil {
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

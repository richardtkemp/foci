package ccstream

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// compactTranscriptLines renders the two records CC writes for one compaction,
// in file order: the compact_boundary system record, then the user record
// flagged isCompactSummary carrying the wrapped summary (shape taken from a
// real CC 2.1.280 transcript).
func compactTranscriptLines(t *testing.T, summary string) string {
	t.Helper()
	boundary := map[string]any{"type": "system", "subtype": "compact_boundary", "compactMetadata": map[string]any{"trigger": "manual", "preTokens": 1000}}
	content := "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\nSummary:\n" +
		summary +
		"\n\nIf you need specific details from before compaction (like exact code snippets), read the full transcript at: /x.jsonl\n" +
		"Continue the conversation from where it left off without asking the user any further questions."
	rec := map[string]any{"type": "user", "isCompactSummary": true, "isVisibleInTranscriptOnly": true, "message": map[string]any{"role": "user", "content": content}}
	var out string
	for _, v := range []any{boundary, rec} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out += string(b) + "\n"
	}
	return out
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// compactTestBackend returns a Backend whose SessionFilePath resolves inside
// a temp HOME, plus that transcript path.
func compactTestBackend(t *testing.T) (*Backend, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	const workDir, sid = "/work/dir", "11111111-2222-3333-4444-555555555555"
	path := filepath.Join(home, ccProjectsDir, projectSlug(workDir), sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	return &Backend{workDir: workDir, sessionID: sid}, path
}

// TestCompactionSummary_ReadsOnlyThisCompaction proves the summary comes from
// the records written after ArmCompactionWait — an earlier compaction's
// summary in the same transcript is never returned — and that CC's
// model-facing preamble and trailer are stripped (#1390).
func TestCompactionSummary_ReadsOnlyThisCompaction(t *testing.T) {
	b, path := compactTestBackend(t)
	appendFile(t, path, `{"type":"user","message":{"role":"user","content":"hi"}}`+"\n")
	appendFile(t, path, compactTranscriptLines(t, "## OLD summary"))

	b.ArmCompactionWait()
	appendFile(t, path, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"mentions \"isCompactSummary\":true in prose"}]}}`+"\n")
	appendFile(t, path, compactTranscriptLines(t, "## NEW summary\n\n- did things"))

	got, err := b.CompactionSummary(context.Background())
	if err != nil {
		t.Fatalf("CompactionSummary: %v", err)
	}
	if want := "## NEW summary\n\n- did things"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

// TestCompactionSummary_WaitsForLateTranscriptWrite proves the stream's
// compact_boundary racing ahead of the transcript write is tolerated: the
// summary is polled for until it lands.
func TestCompactionSummary_WaitsForLateTranscriptWrite(t *testing.T) {
	b, path := compactTestBackend(t)
	appendFile(t, path, `{"type":"user","message":{"role":"user","content":"hi"}}`+"\n")
	b.ArmCompactionWait()

	late := compactTranscriptLines(t, "late summary")
	go func() {
		time.Sleep(3 * compactSummaryPoll)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(late)
	}()

	got, err := b.CompactionSummary(context.Background())
	if err != nil {
		t.Fatalf("CompactionSummary: %v", err)
	}
	if got != "late summary" {
		t.Errorf("summary = %q, want %q", got, "late summary")
	}
}

// TestCompactionSummary_NoSummaryHonoursContext proves an absent summary does
// not block past the caller's context.
func TestCompactionSummary_NoSummaryHonoursContext(t *testing.T) {
	b, path := compactTestBackend(t)
	appendFile(t, path, compactTranscriptLines(t, "pre-arm only"))
	b.ArmCompactionWait()

	ctx, cancel := context.WithTimeout(context.Background(), 2*compactSummaryPoll)
	defer cancel()
	got, err := b.CompactionSummary(ctx)
	if got != "" || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got (%q, %v), want (\"\", DeadlineExceeded)", got, err)
	}
}

func TestCompactionSummary_NoTranscriptPath(t *testing.T) {
	b := &Backend{}
	b.ArmCompactionWait()
	if got, err := b.CompactionSummary(context.Background()); got != "" || err == nil {
		t.Errorf("got (%q, %v), want (\"\", error)", got, err)
	}
}

func TestTrimCompactSummary(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"unwrapped text kept whole", "plain summary", "plain summary"},
		{"header without preamble kept", "intro\nSummary:\nbody", "intro\nSummary:\nbody"},
		{"preamble and header stripped", "This session is being continued from a previous conversation blah.\n\nSummary:\nbody", "body"},
		{"continue trailer stripped", "body\nContinue the conversation from where it left off.", "body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := trimCompactSummary(tc.in); got != tc.want {
				t.Errorf("trimCompactSummary(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestContentText_Blocks(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]`)
	if got := contentText(raw); got != "a\nb" {
		t.Errorf("contentText = %q, want %q", got, "a\nb")
	}
}

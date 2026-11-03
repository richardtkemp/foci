package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeDaily(t *testing.T, ws, date, body string) {
	t.Helper()
	dir := filepath.Join(ws, "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, date+".md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDailyMemoryDigest_HeadingsOfTodayAndYesterdayOnly(t *testing.T) {
	ws := t.TempDir()
	writeDaily(t, ws, "2026-11-03", "# 2026-11-03\nbody line\n## Deploy (09:11)\ntext\n```sh\n# not a heading\n```\n#nospace\n    # indented code\n### Detail\n")
	writeDaily(t, ws, "2026-11-02", "# 2026-11-02\n## Yesterday item\n")
	writeDaily(t, ws, "2026-11-01", "# 2026-11-01\n## Too old\n")

	now := time.Date(2026, 11, 3, 0, 30, 0, 0, time.UTC)
	got := DailyMemoryDigest(ws, now)

	for _, want := range []string{
		"### memory/2026-11-03.md\n- # 2026-11-03\n- ## Deploy (09:11)\n- ### Detail\n",
		"### memory/2026-11-02.md\n- # 2026-11-02\n- ## Yesterday item",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("digest missing %q\n--- got ---\n%s", want, got)
		}
	}
	for _, bad := range []string{"Too old", "not a heading", "nospace", "indented code", "body line"} {
		if strings.Contains(got, bad) {
			t.Errorf("digest contains %q\n--- got ---\n%s", bad, got)
		}
	}
	if strings.Index(got, "2026-11-03.md") > strings.Index(got, "2026-11-02.md") {
		t.Errorf("today must come before yesterday:\n%s", got)
	}
	// Pure function of files + dates: a later clock time the same day is identical.
	if again := DailyMemoryDigest(ws, now.Add(20*time.Hour)); again != got {
		t.Errorf("digest depends on clock time, not just date:\n%s\n---\n%s", got, again)
	}
}

func TestDailyMemoryDigest_EmptyWhenNoFiles(t *testing.T) {
	if got := DailyMemoryDigest(t.TempDir(), time.Now()); got != "" {
		t.Errorf("want empty digest, got %q", got)
	}
}

func TestDailyMemoryDigest_CapKeepsLatestHeadings(t *testing.T) {
	ws := t.TempDir()
	var b strings.Builder
	n := 2 * DailyDigestMaxHeadingChars / 20
	for i := 0; i < n; i++ {
		b.WriteString("## heading number " + strings.Repeat("x", 2) + time.Duration(i).String() + "\n")
	}
	writeDaily(t, ws, "2026-11-03", b.String())
	got := DailyMemoryDigest(ws, time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))
	if !strings.Contains(got, "earlier headings omitted") {
		t.Fatalf("cap did not bite:\n%.300s", got)
	}
	last := "## heading number xx" + time.Duration(n-1).String()
	if !strings.Contains(got, last) {
		t.Errorf("latest heading %q dropped", last)
	}
	if strings.Contains(got, "## heading number xx0s\n") {
		t.Errorf("earliest heading kept despite cap")
	}
	if len(got) > DailyDigestMaxHeadingChars+n*2+500 {
		t.Errorf("digest %d chars, cap %d", len(got), DailyDigestMaxHeadingChars)
	}
}

func TestIsUserChatSession(t *testing.T) {
	cases := map[string]bool{
		"clutch/c2138195870002539221":             true,
		"clutch/c2138195870002539221/b1793729194": false, // branch / spawn / batch
		"clutch/iwork":                            false, // independent
		"garbage":                                 false,
	}
	for key, want := range cases {
		if got := IsUserChatSession(key); got != want {
			t.Errorf("IsUserChatSession(%q) = %v, want %v", key, got, want)
		}
	}
}

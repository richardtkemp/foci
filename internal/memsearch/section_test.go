package memsearch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleNote = `# 2026-11-11

intro line

## Anna's Archive working domain

Working domain from the NUC: annas-archive.pk (14:50).
The .org one is dead.

### Detail

sub detail about the domain

## Other topic

unrelated text here
`

func writeNote(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadSection_FindsHeadingAroundSnippet(t *testing.T) {
	p := writeNote(t, t.TempDir(), "n.md", sampleNote)
	sec, err := ReadSection(p, "…Working domain from the >NUC<: annas-archive.pk…", 0)
	if err != nil {
		t.Fatal(err)
	}
	if sec.Heading != "Anna's Archive working domain" {
		t.Fatalf("heading = %q", sec.Heading)
	}
	if !strings.Contains(sec.Text, "sub detail") {
		t.Errorf("a deeper sub-heading belongs to the section: %q", sec.Text)
	}
	if strings.Contains(sec.Text, "unrelated") {
		t.Errorf("section ran past the next same-level heading: %q", sec.Text)
	}
}

func TestReadSection_FallsBackToHighlightedTerm(t *testing.T) {
	p := writeNote(t, t.TempDir(), "n.md", sampleNote)
	sec, err := ReadSection(p, "snippet text not in file >unrelated< x", 0)
	if err != nil {
		t.Fatal(err)
	}
	if sec.Heading != "Other topic" {
		t.Fatalf("heading = %q", sec.Heading)
	}
}

func TestReadSection_CapsLongSectionAroundHit(t *testing.T) {
	body := "## Big\n" + strings.Repeat("filler words ", 400) + "NEEDLE here " + strings.Repeat("more words ", 400)
	p := writeNote(t, t.TempDir(), "n.md", body)
	sec, err := ReadSection(p, ">NEEDLE< here", 300)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sec.Text, "NEEDLE") {
		t.Fatalf("window lost the hit: %q", sec.Text)
	}
	if n := len([]rune(sec.Text)); n > 310 {
		t.Fatalf("section %d runes, cap 300", n)
	}
}

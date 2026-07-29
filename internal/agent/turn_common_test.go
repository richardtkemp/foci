package agent

import (
	"strings"
	"testing"

	"foci/internal/platform"
)

// TestJoinPrompt_AllFields verifies that JoinPrompt joins all non-empty fields
// with BLANK-LINE separators and formats follow-up texts with the [follow-up] prefix.
func TestJoinPrompt_AllFields(t *testing.T) {
	p := turnTextParts{
		MetaPrefix:      "[meta: test]",
		Reminders:       "reminder1",
		StateDashboard:  "state: ok",
		AttachmentPaths: "[Image saved to: /tmp/img.png]",
		UserTexts:       []string{"hello", "follow up 1", "follow up 2"},
	}

	got := p.JoinPrompt()

	for _, want := range []string{
		"[meta: test]",
		"reminder1",
		"state: ok",
		"[Image saved to: /tmp/img.png]",
		"hello",
		"[follow-up] follow up 1",
		"[follow-up] follow up 2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("JoinPrompt missing %q in:\n%s", want, got)
		}
	}

	// Seven parts separated by blank lines: 7 content lines + 6 blanks.
	lines := strings.Split(got, "\n")
	if len(lines) != 13 {
		t.Errorf("expected 13 lines (7 parts + 6 blank separators), got %d: %v", len(lines), lines)
	}
	// Every part must be preceded by a blank line — that separation IS the fix.
	for _, want := range []string{
		"[meta: test]\n\nreminder1",
		"reminder1\n\nstate: ok",
		"[Image saved to: /tmp/img.png]\n\nhello",
		"hello\n\n[follow-up] follow up 1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("parts not blank-line separated; missing %q in:\n%s", want, got)
		}
	}
}

// TestJoinPrompt_SkipsEmptyParts verifies that empty fields are omitted entirely
// rather than contributing an empty segment — an absent part must not leave a
// double blank line behind.
//
// This test previously asserted the OPPOSITE ("should not contain consecutive
// newlines"), which encoded the very bug #1627 reports: with a single-newline
// join, an agent cannot tell the injected header from the user's first line.
// The assertion was a stale contract, not a regression.
func TestJoinPrompt_SkipsEmptyParts(t *testing.T) {
	p := turnTextParts{
		MetaPrefix: "[meta]",
		// Reminders, StateDashboard, AttachmentPaths all empty
		UserTexts: []string{"hello"},
	}

	got := p.JoinPrompt()
	if got != "[meta]\n\nhello" {
		t.Errorf("got %q, want %q", got, "[meta]\n\nhello")
	}
	// Exactly ONE blank line: three empty parts must contribute nothing.
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("empty parts leaked blank lines: %q", got)
	}
}

// TestJoinPrompt_SinglePart verifies that a single non-empty part is returned
// as-is without any separator.
func TestJoinPrompt_SinglePart(t *testing.T) {
	p := turnTextParts{
		UserTexts: []string{"only text"},
	}

	got := p.JoinPrompt()
	if got != "only text" {
		t.Errorf("expected %q, got %q", "only text", got)
	}
}

// TestJoinPrompt_Empty verifies that a completely empty turnTextParts produces
// an empty string.
func TestJoinPrompt_Empty(t *testing.T) {
	p := turnTextParts{}

	got := p.JoinPrompt()
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// TestJoinPrompt_FollowUpsWithoutPrimary verifies that an empty UserTexts
// slice produces no user text section at all.
func TestJoinPrompt_EmptyUserTexts(t *testing.T) {
	p := turnTextParts{
		MetaPrefix: "[meta]",
		UserTexts:  []string{},
	}

	got := p.JoinPrompt()
	if got != "[meta]" {
		t.Errorf("expected %q, got %q", "[meta]", got)
	}
}

// TestJoinPrompt_OnlyFollowUps verifies that when there's a primary text and
// a single follow-up, the follow-up gets the [follow-up] prefix.
func TestJoinPrompt_PrimaryAndOneFollowUp(t *testing.T) {
	p := turnTextParts{
		UserTexts: []string{"primary", "extra"},
	}

	got := p.JoinPrompt()
	want := "primary\n\n[follow-up] extra"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAttachmentPaths_MultipleAttachments verifies that multiple attachments
// with SavedPath are joined with newlines.
func TestAttachmentPaths_MultipleAttachments(t *testing.T) {
	// We can't call composeTurnText (needs full Agent wiring), but we can
	// verify the attachment path logic by building turnTextParts manually
	// and checking JoinPrompt output.
	p := turnTextParts{
		AttachmentPaths: "[Image saved to: /tmp/a.png]\n[PDF saved to: /tmp/b.pdf]",
		UserTexts:       []string{"check these"},
	}

	got := p.JoinPrompt()
	if !strings.Contains(got, "[Image saved to: /tmp/a.png]") {
		t.Error("missing first attachment path")
	}
	if !strings.Contains(got, "[PDF saved to: /tmp/b.pdf]") {
		t.Error("missing second attachment path")
	}
}

// TestAttachmentPathBuilding verifies the attachment path annotation logic
// that would normally run inside composeTurnText. This tests the same
// algorithm in isolation: only attachments with non-empty SavedPath produce
// annotations, and the MIME label comes from labelForMIME.
func TestAttachmentPathBuilding(t *testing.T) {
	tests := []struct {
		name        string
		attachments []platform.Attachment
		want        string
	}{
		{
			name:        "nil attachments",
			attachments: nil,
			want:        "",
		},
		{
			name:        "empty slice",
			attachments: []platform.Attachment{},
			want:        "",
		},
		{
			name: "no saved paths",
			attachments: []platform.Attachment{
				{MimeType: "image/png", SavedPath: ""},
			},
			want: "",
		},
		{
			name: "single attachment",
			attachments: []platform.Attachment{
				{MimeType: "image/png", SavedPath: "/tmp/img.png"},
			},
			want: "[Image saved to: /tmp/img.png]",
		},
		{
			name: "multiple attachments mixed",
			attachments: []platform.Attachment{
				{MimeType: "image/jpeg", SavedPath: "/tmp/photo.jpg"},
				{MimeType: "text/plain", SavedPath: ""},
				{MimeType: "application/pdf", SavedPath: "/tmp/doc.pdf"},
			},
			want: "[Image saved to: /tmp/photo.jpg]\n[PDF saved to: /tmp/doc.pdf]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Replicate the attachment path logic from composeTurnText.
			var attachParts []string
			for _, att := range tt.attachments {
				if att.SavedPath != "" {
					label := labelForMIME(att.MimeType)
					attachParts = append(attachParts, "["+label+" saved to: "+att.SavedPath+"]")
				}
			}
			var got string
			if len(attachParts) > 0 {
				got = strings.Join(attachParts, "\n")
			}

			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestJoinPrompt_HeaderIsSeparatedFromUserText is the #1627 regression: coach
// received Dick's habit line as if it were part of the [state] block and
// reported the message as EMPTY, twice.
//
// The inputs are the real ones. The default statusline drops an all-empty
// placeholder line, so with no pending ask the rendered header ENDS on
// "[state] …" — putting the user's first line immediately beneath it. This test
// asserts the property that actually matters (the user's text begins a new
// block) rather than the exact byte layout, so a future statusline change
// cannot quietly reintroduce the ambiguity.
func TestJoinPrompt_HeaderIsSeparatedFromUserText(t *testing.T) {
	const userText = "Yesterday: social yes, drive 3, self-directed no, feeds low no, cold no, walk yes, drink no"
	p := turnTextParts{
		MetaPrefix: "[meta] time=2026-07-29T11:00:02+01:00 gap=none model=opus via=app\n" +
			"[state] todos: 6 open (1 high)",
		UserTexts: []string{userText},
	}

	got := p.JoinPrompt()

	// The bug, stated directly: the user's text must not be the line straight
	// after the state line.
	if strings.Contains(got, "[state] todos: 6 open (1 high)\n"+userText) {
		t.Fatalf("user text abuts the [state] line with no boundary:\n%s", got)
	}
	if !strings.Contains(got, "\n\n"+userText) {
		t.Errorf("user text is not preceded by a blank line:\n%s", got)
	}

	// Everything above the first blank line is foci's; everything below is the
	// human's. That split is the contract the environment block documents.
	header, body, found := strings.Cut(got, "\n\n")
	if !found {
		t.Fatal("no blank line separating header from body")
	}
	if body != userText {
		t.Errorf("body = %q, want %q", body, userText)
	}
	if strings.Contains(header, "Yesterday:") {
		t.Errorf("user text leaked into the header block: %q", header)
	}
}

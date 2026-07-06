package agent

import (
	"strings"
	"testing"

	"foci/internal/platform"
)

// TestJoinPrompt_AllFields verifies that JoinPrompt joins all non-empty fields
// with newline separators and formats follow-up texts with the [follow-up] prefix.
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

	// Verify parts are separated by newlines.
	lines := strings.Split(got, "\n")
	if len(lines) != 7 {
		t.Errorf("expected 7 lines, got %d: %v", len(lines), lines)
	}
}

// TestJoinPrompt_SkipsEmptyParts verifies that empty fields are omitted,
// producing no extra newlines or blank segments.
func TestJoinPrompt_SkipsEmptyParts(t *testing.T) {
	p := turnTextParts{
		MetaPrefix: "[meta]",
		// Reminders, StateDashboard, AttachmentPaths all empty
		UserTexts: []string{"hello"},
	}

	got := p.JoinPrompt()
	if strings.Contains(got, "\n\n") {
		t.Errorf("should not contain consecutive newlines: %q", got)
	}

	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 lines (meta + user text), got %d: %v", len(lines), lines)
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
	want := "primary\n[follow-up] extra"
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

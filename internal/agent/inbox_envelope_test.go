package agent

import (
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/platform"
)

// TestEnvelopeFromQueuedMessage pins the ONE QueuedMessage→Envelope mapping
// both bots' handoff paths use: every sender-visible field rides across
// verbatim — the #1318 per-message steer choice and the #1436 voice marker
// included — bound to the session key and Driver the caller resolved. A
// dropped field here is dropped for both platforms at once, which is exactly
// what the unification is for.
func TestEnvelopeFromQueuedMessage(t *testing.T) {
	recv := time.Date(2026, 10, 14, 1, 2, 3, 0, time.UTC)
	atts := []platform.Attachment{{Type: "photo", Data: []byte{1, 2}, MimeType: "image/png"}}
	original := "raw-platform-original"

	for _, tc := range []struct {
		name  string
		steer SteerPreference
	}{
		{"queue choice", SteerNever},
		{"steer choice", SteerAlways},
		{"no choice", SteerDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := &recordingDriver{}
			qm := platform.QueuedMessage{
				UserID:      "u1",
				SenderName:  "Uno",
				Text:        "do it next",
				Attachments: atts,
				ChatID:      42,
				IsGroupChat: true,
				Original:    original,
				ReceivedAt:  recv,
				Voice:       true,
				Steer:       tc.steer,
			}

			env := EnvelopeFromQueuedMessage("a/c42", qm, driver)

			if env.SessionKey != "a/c42" {
				t.Errorf("SessionKey = %q, want the caller-resolved key", env.SessionKey)
			}
			if env.Text != qm.Text {
				t.Errorf("Text = %q, want %q", env.Text, qm.Text)
			}
			if env.UserID != qm.UserID || env.SenderName != qm.SenderName {
				t.Errorf("sender = %q/%q, want %q/%q", env.UserID, env.SenderName, qm.UserID, qm.SenderName)
			}
			if len(env.Attachments) != 1 || env.Attachments[0].SavedPath != atts[0].SavedPath || env.Attachments[0].Type != atts[0].Type {
				t.Errorf("Attachments = %+v, want the queued attachments verbatim", env.Attachments)
			}
			if env.ChatID != qm.ChatID || !env.IsGroupChat {
				t.Errorf("chat = %d group=%v, want %d group=true", env.ChatID, env.IsGroupChat, qm.ChatID)
			}
			if env.ReceivedAt != recv {
				t.Errorf("ReceivedAt = %v, want the platform receipt time %v", env.ReceivedAt, recv)
			}
			if !env.Voice {
				t.Error("Voice lost — a transcribed voice note would render as typed text (#1436)")
			}
			if env.Steer != tc.steer {
				t.Errorf("Steer = %v, want the sender's choice %v (#1318)", env.Steer, tc.steer)
			}
			if env.Original != original {
				t.Errorf("Original = %v, want the raw platform message", env.Original)
			}
			if env.Driver != driver {
				t.Error("Driver = another driver, want the originating bot")
			}
			if env.Inject != nil || env.Ref != (delegator.InputRef{}) {
				t.Errorf("handoff-set fields polluted: Inject=%v Ref=%+v", env.Inject, env.Ref)
			}
		})
	}
}

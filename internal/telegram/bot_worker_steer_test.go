package telegram

import (
	"context"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/platform"
)

// TestHandoffToAgent_CarriesQueuedMessageFields proves the worker handoff's
// half of #1318 end to end: a queued message handed to the agent inbox (the
// only route from the platform queue to a turn) reaches the session worker
// as the SAME message — text, per-message delivery choice, voice marker,
// attachments, raw original — with this bot as the Driver. The minimal live
// agent drives a real turn cycle; Original is deliberately not a telegram
// message so NewTurnSink declines and RunTurn no-ops, leaving the observed
// envelope exactly what the handoff produced.
func TestHandoffToAgent_CarriesQueuedMessageFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steer platform.SteerPreference
	}{
		{"queue choice (a /queue prefix)", platform.SteerNever},
		{"steer choice (a /steer prefix)", platform.SteerAlways},
		{"no choice (plain message)", platform.SteerDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := testBot(nil, command.NewRegistry())
			ag := &agent.Agent{AgentID: "test"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ag.StartInbox(ctx)

			envs := make(chan agent.Envelope, 4)
			ag.SetTurnObserver(func(_ string, batch []agent.Envelope) {
				for _, e := range batch {
					envs <- e
				}
			})
			b.agentRef = ag

			recv := time.Date(2026, 10, 14, 1, 2, 3, 0, time.UTC)
			qm := platform.QueuedMessage{
				UserID:      "111",
				SenderName:  "owner",
				Text:        "do it next",
				Attachments: []platform.Attachment{{Type: "photo", Data: []byte{1}}},
				ChatID:      12345,
				Original:    "not-a-telegram-message",
				ReceivedAt:  recv,
				Voice:       true,
				Steer:       tc.steer,
			}
			b.handoffToAgent(qm)

			select {
			case env := <-envs:
				if env.SessionKey != b.sessionKeyForQueuedMessage(qm) {
					t.Errorf("SessionKey = %q, want the bot-resolved key %q", env.SessionKey, b.sessionKeyForQueuedMessage(qm))
				}
				if env.Text != qm.Text {
					t.Errorf("Text = %q, want %q", env.Text, qm.Text)
				}
				if env.UserID != qm.UserID || env.SenderName != qm.SenderName {
					t.Errorf("sender = %q/%q, want %q/%q", env.UserID, env.SenderName, qm.UserID, qm.SenderName)
				}
				if len(env.Attachments) != 1 {
					t.Errorf("Attachments = %d item(s), want the queued one", len(env.Attachments))
				}
				if env.ChatID != qm.ChatID {
					t.Errorf("ChatID = %d, want %d", env.ChatID, qm.ChatID)
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
				if env.Original != qm.Original {
					t.Errorf("Original = %v, want the raw platform message", env.Original)
				}
				if env.Driver != agent.Driver(b) {
					t.Error("Driver = another driver, want this bot")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("handoff never reached a turn within 2s")
			}
		})
	}
}

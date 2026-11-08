package app

import (
	"context"
	"testing"

	"foci/internal/agent"
	"foci/internal/fap"
	"foci/internal/turnevent"
)

// #2985: a voice-mode clip names the text message it reads aloud
// (Media.textMessageId), so the app can draw it inside that message's bubble.
// Both synthesis paths: the per-block clip of an intermediate TextBlock (#1444)
// and the whole-reply clip on TurnComplete. Each clip must arrive AFTER its
// text frame and carry that frame's messageId.
func TestAppConn_VoiceModeClipNamesItsTextMessage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []turnevent.Event
	}{
		{"turn complete", []turnevent.Event{turnevent.TurnComplete{FinalText: "spoken reply"}}},
		{"intermediate blocks", []turnevent.Event{
			turnevent.TextBlock{Text: "first bubble", Phase: turnevent.PhaseIntermediate},
			turnevent.TextBlock{Text: "second bubble", Phase: turnevent.PhaseIntermediate},
			turnevent.TurnComplete{FinalText: "first bubble\n\nsecond bubble"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHub()
			b := h.ensureBinding(nil, "arnix", "conv-1")
			client := fakeClient()
			b.attach(client)
			conn := &appConn{hub: h, agentID: "arnix", tts: &mockVoiceTTS{data: []byte("synth")}}
			sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: b.key()})
			defer cleanup()
			ctx := agent.WithTrigger(context.Background(), "voice")
			for _, ev := range tc.events {
				sink.Emit(ctx, ev)
			}

			var lastText string
			clips := 0
			for _, f := range drain(t, client) {
				switch f.t {
				case fap.TypeMessage:
					lastText, _ = f.d["messageId"].(string)
				case fap.TypeMedia:
					clips++
					link, _ := f.d["textMessageId"].(string)
					if lastText == "" {
						t.Fatalf("clip %d arrived before any text message", clips)
					}
					if link != lastText {
						t.Errorf("clip %d textMessageId = %q, want the text just before it %q", clips, link, lastText)
					}
					if id, _ := f.d["messageId"].(string); id == link {
						t.Errorf("clip %d reuses its text's messageId %q; it must keep its own", clips, id)
					}
					lastText = ""
				}
			}
			want := 1
			if tc.name == "intermediate blocks" {
				want = 2
			}
			if clips != want {
				t.Fatalf("clips = %d, want %d", clips, want)
			}
		})
	}
}

// A clip whose text event delivered nothing goes out unlinked, so a client
// draws it as its own row, as before #2985.
func TestAppSink_VoiceClipWithoutDeliveredTextIsUnlinked(t *testing.T) {
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{fakeClient(): {}}}
	s := newAppSink(b)
	var links []string
	s.attachVoice = func(_ []byte, link string) error { links = append(links, link); return nil }
	s.backend.lastMsgID = "stale-from-an-earlier-bubble"
	s.attachVoiceClip([]byte("clip"))
	s.backend.lastMsgID = ""
	s.attachVoiceClip([]byte("clip"))
	s.attachVoiceClip(nil)
	if len(links) != 2 || links[1] != "" {
		t.Fatalf("links = %q, want 2 sends, the second unlinked", links)
	}
}

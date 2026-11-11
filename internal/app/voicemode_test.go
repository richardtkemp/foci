package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/fap"
	"foci/internal/turnevent"
)

// The persistent voice-mode report is per socket (#3047): a put turns a chat
// on, a later full-replace put without it turns it off, and closing the socket
// forgets it, so a reconnecting client must report again.
func TestVoiceModePut_PerSocketState(t *testing.T) {
	h := newTestHub()
	phone, mac := deviceClient(h, "phone"), deviceClient(h, "mac")

	if h.voiceModeOn("c1") {
		t.Fatal("c1 in voice mode before any report")
	}
	h.handleVoiceModePut(phone, fap.VoiceModePut{ConversationIDs: []string{"c1", "c2"}})
	if !h.voiceModeOn("c1") || !h.voiceModeOn("c2") {
		t.Fatal("c1/c2 not in voice mode after the phone reported them")
	}
	if h.voiceModeOn("c3") || h.voiceModeOn("") {
		t.Fatal("an unreported chat is in voice mode")
	}
	h.handleVoiceModePut(phone, fap.VoiceModePut{ConversationIDs: []string{"c2"}})
	if h.voiceModeOn("c1") {
		t.Error("c1 still in voice mode after a put without it (full replace)")
	}
	h.handleVoiceModePut(mac, fap.VoiceModePut{ConversationIDs: []string{"c2"}})
	h.removeClient(phone)
	if !h.voiceModeOn("c2") {
		t.Error("c2 off after the phone closed, but the mac still reports it")
	}
	h.removeClient(mac)
	if h.voiceModeOn("c2") {
		t.Error("c2 still in voice mode after every reporting socket closed")
	}
}

// voiceClips counts the voiceMode Media frames queued for c, and reports
// whether each named the text message sent just before it.
func voiceClips(t *testing.T, c *wsClient) (clips int, linked bool) {
	t.Helper()
	linked = true
	var lastText string
	for _, f := range drain(t, c) {
		switch f.t {
		case fap.TypeMessage:
			lastText, _ = f.d["messageId"].(string)
		case fap.TypeMedia:
			if vm, _ := f.d["voiceMode"].(bool); !vm {
				continue
			}
			clips++
			if link, _ := f.d["textMessageId"].(string); link == "" || link != lastText {
				linked = false
			}
			lastText = ""
		}
	}
	return clips, linked
}

// While a device has the chat in persistent voice mode, a turn that is not a
// voice turn (a wake, a spawn result, a reminder: any background turn) is
// spoken too (#3047). Without the mode it stays text-only, as before.
func TestAppConn_PersistentVoiceModeSpeaksEveryTurn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		on     bool
		events []turnevent.Event
		want   int
	}{
		{"off: background turn stays text", false, []turnevent.Event{turnevent.TurnComplete{FinalText: "spawn finished"}}, 0},
		{"on: background turn is spoken", true, []turnevent.Event{turnevent.TurnComplete{FinalText: "spawn finished"}}, 1},
		{"on: one clip per bubble", true, []turnevent.Event{
			turnevent.TextBlock{Text: "first bubble", Phase: turnevent.PhaseIntermediate},
			turnevent.TextBlock{Text: "second bubble", Phase: turnevent.PhaseIntermediate},
			turnevent.TurnComplete{FinalText: "first bubble\n\nsecond bubble"},
		}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHub()
			b := h.ensureBinding(nil, "arnix", "conv-1")
			client := deviceClient(h, "phone")
			b.attach(client)
			if tc.on {
				h.handleVoiceModePut(client, fap.VoiceModePut{ConversationIDs: []string{b.convID}})
			}
			conn := &appConn{hub: h, agentID: "arnix", tts: &mockVoiceTTS{data: []byte("synth")}}
			sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: b.key()})
			defer cleanup()
			ctx := agent.WithTrigger(context.Background(), "wake")
			for _, ev := range tc.events {
				sink.Emit(ctx, ev)
			}
			clips, linked := voiceClips(t, client)
			if clips != tc.want {
				t.Fatalf("voice clips = %d, want %d", clips, tc.want)
			}
			if !linked {
				t.Error("a clip did not name the text message just before it")
			}
		})
	}
}

// Turning the mode on mid-turn speaks from that moment on (#3047 req 2), and
// never the turn-end accumulation of bubbles already delivered unspoken: the
// ccstream FinalText repeats every delivered bubble (#1444).
func TestAppSink_VoiceModeOnMidTurnSpeaksOnlyNewBubbles(t *testing.T) {
	h := newTestHub()
	b := h.ensureBinding(nil, "arnix", "conv-1")
	client := deviceClient(h, "phone")
	b.attach(client)
	tts := &mockVoiceTTS{data: []byte("synth")}
	conn := &appConn{hub: h, agentID: "arnix", tts: tts}
	sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: b.key()})
	defer cleanup()
	ctx := agent.WithTrigger(context.Background(), "wake")

	sink.Emit(ctx, turnevent.TextBlock{Text: "before", Phase: turnevent.PhaseIntermediate})
	h.handleVoiceModePut(client, fap.VoiceModePut{ConversationIDs: []string{b.convID}})
	sink.Emit(ctx, turnevent.TextBlock{Text: "after", Phase: turnevent.PhaseIntermediate})
	sink.Emit(ctx, turnevent.TurnComplete{FinalText: "before\n\nafter"})

	if clips, _ := voiceClips(t, client); clips != 1 {
		t.Fatalf("voice clips = %d, want 1 (only the bubble after the mode started)", clips)
	}
	if len(tts.got) != 1 || tts.got[0] != "after" {
		t.Errorf("synthesized %q, want only [after]", tts.got)
	}
}

// An agent message that bypasses the turn sink (a broadcast /send reply goes
// out through SendToSession) is spoken too while the chat is in voice mode,
// its clip after its text and naming it (#3047).
func TestAppConn_SendToSessionSpokenInVoiceMode(t *testing.T) {
	for _, on := range []bool{false, true} {
		h := newTestHub()
		b := h.ensureBinding(nil, "arnix", "conv-1")
		client := deviceClient(h, "phone")
		b.attach(client)
		if on {
			h.handleVoiceModePut(client, fap.VoiceModePut{ConversationIDs: []string{b.convID}})
		}
		conn := &appConn{hub: h, agentID: "arnix", tts: &mockVoiceTTS{data: []byte("synth")}}
		if err := conn.SendToSession(b.key(), "broadcast reply"); err != nil {
			t.Fatal(err)
		}
		clips, linked := voiceClips(t, client)
		want := 0
		if on {
			want = 1
		}
		if clips != want || !linked {
			t.Errorf("voice mode %v: clips = %d (linked %v), want %d linked", on, clips, linked, want)
		}
	}
}

// A file the agent sends to a chat in voice mode is announced aloud (#3048,
// Dick 2026-11-11: "File sends should say something like 'received file
// $name'"): a voiceMode clip after the file, reading its name and caption.
// Outside voice mode a file stays silent.
func TestAppConn_FileSendAnnouncedInVoiceMode(t *testing.T) {
	for _, on := range []bool{false, true} {
		h := newTestHub()
		b := h.ensureBinding(nil, "arnix", "conv-1")
		client := deviceClient(h, "phone")
		b.attach(client)
		if on {
			h.handleVoiceModePut(client, fap.VoiceModePut{ConversationIDs: []string{b.convID}})
		}
		tts := &mockVoiceTTS{data: []byte("synth")}
		conn := &appConn{hub: h, agentID: "arnix", tts: tts, bound: b.key()}
		path := filepath.Join(t.TempDir(), "report.pdf")
		if err := os.WriteFile(path, []byte("%PDF"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := conn.SendDocumentOwnChat(path, "the tax summary"); err != nil {
			t.Fatal(err)
		}
		var kinds []string
		for _, f := range drain(t, client) {
			if f.t != fap.TypeMedia {
				continue
			}
			if vm, _ := f.d["voiceMode"].(bool); vm {
				kinds = append(kinds, "clip")
			} else {
				kinds = append(kinds, "file")
			}
		}
		if !on {
			if len(kinds) != 1 || len(tts.got) != 0 {
				t.Errorf("voice mode off: media %v, spoken %q; want the file alone, nothing spoken", kinds, tts.got)
			}
			continue
		}
		if len(kinds) != 2 || kinds[0] != "file" || kinds[1] != "clip" {
			t.Fatalf("voice mode on: media %v, want [file clip]", kinds)
		}
		if len(tts.got) != 1 || !strings.Contains(tts.got[0], "Received file report.pdf") || !strings.Contains(tts.got[0], "the tax summary") {
			t.Errorf("spoken %q, want 'Received file report.pdf' and the caption", tts.got)
		}
	}
}

// The rendered PNG that follows a mermaid source (send_to_chat, #2195) is a
// second view of a file already announced: SendRenderedPhoto sends it without a
// second announcement (#3048: "announce a mermaid send once").
func TestAppConn_RenderedPhotoNotAnnounced(t *testing.T) {
	h := newTestHub()
	b := h.ensureBinding(nil, "arnix", "conv-1")
	client := deviceClient(h, "phone")
	b.attach(client)
	h.handleVoiceModePut(client, fap.VoiceModePut{ConversationIDs: []string{b.convID}})
	tts := &mockVoiceTTS{data: []byte("synth")}
	conn := &appConn{hub: h, agentID: "arnix", tts: tts, bound: b.key()}
	path := filepath.Join(t.TempDir(), "flow.png")
	if err := os.WriteFile(path, []byte("\x89PNG"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := conn.SendRenderedPhoto(0, path); err != nil {
		t.Fatal(err)
	}
	files, clips := 0, 0
	for _, f := range drain(t, client) {
		if f.t != fap.TypeMedia {
			continue
		}
		if vm, _ := f.d["voiceMode"].(bool); vm {
			clips++
		} else {
			files++
		}
	}
	if files != 1 || clips != 0 || len(tts.got) != 0 {
		t.Errorf("files %d, clips %d, spoken %q; want the photo alone, unannounced", files, clips, tts.got)
	}
}

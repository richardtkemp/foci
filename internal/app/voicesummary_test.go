package app

import (
	"context"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/epdispatch"
	"foci/internal/fap"
	"foci/internal/turnevent"
	"foci/internal/voice"
)

const voiceBlockMsg = "Here is the flow.\n\n```mermaid\ngraph TD\n  Alpha-->Beta\n```\n\nThat is all."

// A voice-mode bubble with a diagram speaks the fast model's sentence in the
// diagram's place, never its source (#3050). The call goes to the pin label's
// model (useShared) with the voice-block prompt.
func TestAppSink_VoiceModeSpeaksBlockSummary(t *testing.T) {
	stub := &openRouterStub{reply: "A flowchart of two steps."}
	srv := stub.serve(t)
	h := newTestHub()
	h.pinSum = testSummariser(srv.URL, nil)
	b := h.ensureBinding(nil, "arnix", "conv-1")
	client := deviceClient(h, "phone")
	b.attach(client)
	tts := &mockVoiceTTS{data: []byte("synth")}
	conn := &appConn{hub: h, agentID: "arnix", tts: tts}
	sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: b.key()})
	defer cleanup()
	sink.Emit(agent.WithTrigger(context.Background(), "voice"), turnevent.TurnComplete{FinalText: voiceBlockMsg})

	if len(tts.got) != 1 {
		t.Fatalf("synthesized %d clips, want 1", len(tts.got))
	}
	spoken := tts.got[0]
	if strings.Contains(spoken, "Alpha") || strings.Contains(spoken, "graph") {
		t.Errorf("spoken text reads the diagram source: %q", spoken)
	}
	if want := "Here is the flow.\n\nA flowchart of two steps.\n\nThat is all."; spoken != want {
		t.Errorf("spoken = %q, want %q", spoken, want)
	}
	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d model calls, want 1", len(reqs))
	}
	msgs, _ := reqs[0]["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)["content"].(string)
	user, _ := msgs[1].(map[string]any)["content"].(string)
	if !strings.Contains(sys, "mermaid") || !strings.Contains(sys, "FIFTEEN WORDS") {
		t.Errorf("system prompt = %q", sys)
	}
	if !strings.Contains(user, "Alpha-->Beta") {
		t.Errorf("model input = %q, want the diagram source", user)
	}
}

// With no usable summary path, or a failing one, the marker is spoken
// (#3050 req 3): the source is never read.
func TestAppSink_VoiceModeBlockMarkerFallback(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, h *Hub)
	}{
		{"no summariser", func(*testing.T, *Hub) {}},
		{"model fails", func(t *testing.T, h *Hub) {
			stub := &openRouterStub{status: 502}
			h.pinSum = testSummariser(stub.serve(t).URL, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHub()
			tc.setup(t, h)
			b := h.ensureBinding(nil, "arnix", "conv-1")
			b.attach(deviceClient(h, "phone"))
			tts := &mockVoiceTTS{data: []byte("synth")}
			conn := &appConn{hub: h, agentID: "arnix", tts: tts}
			sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: b.key()})
			defer cleanup()
			sink.Emit(agent.WithTrigger(context.Background(), "voice"),
				turnevent.TextBlock{Text: voiceBlockMsg, Phase: turnevent.PhaseIntermediate})
			if len(tts.got) != 1 {
				t.Fatalf("synthesized %d clips, want 1", len(tts.got))
			}
			if want := "Here is the flow.\n\nDiagram omitted.\n\nThat is all."; tts.got[0] != want {
				t.Errorf("spoken = %q, want %q", tts.got[0], want)
			}
		})
	}
}

// A message sent outside a turn into a chat in voice mode (speakInVoiceMode)
// takes the same path.
func TestSpeakInVoiceMode_BlockSummary(t *testing.T) {
	stub := &openRouterStub{reply: "A small table of prices"}
	h := newTestHub()
	h.pinSum = testSummariser(stub.serve(t).URL, nil)
	b := h.ensureBinding(nil, "arnix", "conv-1")
	client := deviceClient(h, "phone")
	b.attach(client)
	h.handleVoiceModePut(client, fap.VoiceModePut{ConversationIDs: []string{b.convID}})
	tts := &mockVoiceTTS{data: []byte("synth")}
	conn := &appConn{hub: h, agentID: "arnix", tts: tts}
	conn.speakInVoiceMode(b, "m1", "Prices:\n| a | b |\n|---|---|\n| 1 | 2 |")
	if len(tts.got) != 1 || tts.got[0] != "Prices:\nA small table of prices." {
		t.Errorf("spoken = %q", tts.got)
	}
}

// The block summaries are the pin label's task (Dick, 2026-11-11), so they take
// the High tier on a local endpoint's queue (#3019).
func TestVoiceBlockAsk_HighTier(t *testing.T) {
	for _, k := range []voice.BlockKind{voice.BlockDiagram, voice.BlockTable, voice.BlockCode} {
		a := voiceBlockAsk(k)
		if a.use != useShared || a.dispatchTier() != epdispatch.High {
			t.Errorf("%s: use=%v tier=%v, want the shared target at High", k, a.use, a.dispatchTier())
		}
	}
}

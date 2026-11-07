package app

import (
	"testing"

	"foci/internal/fap"
)

func TestDeliverPromptSuggestion_BoundSessionGetsFrame(t *testing.T) {
	h := newTestHub()
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })

	b := h.ensureBinding(nil, "scout", "conv-1")
	client := fakeClient()
	b.attach(client)

	if !DeliverPromptSuggestion(b.key(), "run the tests") {
		t.Error("delivery to a bound conversation reported false")
	}

	got := drain(t, client)
	if len(got) != 1 || got[0].t != fap.TypePromptSuggestion {
		t.Fatalf("frames = %v, want exactly one %s", types(got), fap.TypePromptSuggestion)
	}
	if got[0].d["text"] != "run the tests" || got[0].d["conversationId"] != "conv-1" {
		t.Errorf("frame = %v, want the text on conv-1", got[0].d)
	}
}

func TestDeliverPromptSuggestion_NoHubOrBindingIsSilent(t *testing.T) {
	setActiveHub(nil)
	if DeliverPromptSuggestion("scout/c1", "x") { // no hub: no panic
		t.Error("delivery with no hub reported true")
	}

	h := newTestHub()
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })
	if DeliverPromptSuggestion("scout/c999", "x") { // no binding: no panic
		t.Error("delivery with no binding reported true")
	}
}

// A suggestion is not news: it must not buzz the phone or bump the unread count.
func TestPromptSuggestion_NoPushNoUnread(t *testing.T) {
	if preview, visible := pushPreview(fap.PromptSuggestion{ConversationID: "c", Text: "x"}); visible || preview != "" {
		t.Fatalf("pushPreview = (%q, %v), want invisible", preview, visible)
	}
}

// CanShowPromptSuggestion asks what delivery asks, so an app conversation that
// adopted another platform's session counts, and an unbound session does not.
func TestCanShowPromptSuggestion_FollowsBinding(t *testing.T) {
	setActiveHub(nil)
	if CanShowPromptSuggestion("scout/c1") {
		t.Error("no hub: want false")
	}
	h := newTestHub()
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })
	if CanShowPromptSuggestion("scout/c999") {
		t.Error("no binding: want false")
	}
	b := h.ensureBinding(nil, "scout", "conv-can-show")
	if !CanShowPromptSuggestion(b.key()) {
		t.Errorf("bound session %q: want true", b.key())
	}
	h.adoptSession(b, "scout/c42")
	if !CanShowPromptSuggestion("scout/c42") {
		t.Error("adopted session, the one a Telegram chat claims: want true")
	}
}

// The clear is an empty suggestion frame, sent once for each suggestion, so a
// conversation with no chip gets no frame on an ordinary user turn (#2924).
func TestClearPromptSuggestion_OneEmptyFramePerSuggestion(t *testing.T) {
	setActiveHub(nil)
	ClearPromptSuggestion("scout/c1") // no hub: no panic

	h := newTestHub()
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })
	ClearPromptSuggestion("scout/c999") // no binding: no panic

	b := h.ensureBinding(nil, "scout", "conv-clear")
	client := fakeClient()
	b.attach(client)

	ClearPromptSuggestion(b.key())
	if got := drain(t, client); len(got) != 0 {
		t.Fatalf("no suggestion sent: frames = %v, want none", types(got))
	}

	DeliverPromptSuggestion(b.key(), "run the tests")
	drain(t, client)
	ClearPromptSuggestion(b.key())
	got := drain(t, client)
	if len(got) != 1 || got[0].t != fap.TypePromptSuggestion {
		t.Fatalf("frames = %v, want exactly one %s", types(got), fap.TypePromptSuggestion)
	}
	if text, present := got[0].d["text"]; !present || text != "" || got[0].d["conversationId"] != "conv-clear" {
		t.Errorf("frame = %v, want empty text on conv-clear", got[0].d)
	}

	ClearPromptSuggestion(b.key())
	if got := drain(t, client); len(got) != 0 {
		t.Fatalf("second clear: frames = %v, want none", types(got))
	}
}

package app

import (
	"context"
	"testing"

	"foci/internal/app/fap"
	"foci/internal/command"
	"foci/internal/platform"
)

// TestMintFacetConversation proves the app-side facet surface: a new conversation
// is bound to the branch session key, added to the shared open-set (tab opens on
// every device), and advertised to connected clients via roster + openSync.
func TestMintFacetConversation(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	registerBareAgent(h, "ag")
	c := fakeClient()
	h.clients[c] = struct{}{}

	convID, err := h.mintFacetConversation("ag", "ag/cbranch")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if convID == "" {
		t.Fatal("empty convID")
	}

	// Binding exists and is bound to the branch session key (not the default).
	if b := h.bindingForSession("ag/cbranch"); b == nil || b.convID != convID {
		t.Fatalf("binding for ag/cbranch = %v, want convID %s", b, convID)
	}

	// Open-set now contains the new conversation → tab opens on all devices.
	if got := h.loadOpenChats(); len(got) != 1 || got[0] != convID {
		t.Errorf("open-set = %v, want [%s]", got, convID)
	}

	// The connected client learned the conversation (roster) and its open-state.
	ds := drain(t, c)
	var sawSync, sawRoster bool
	for _, d := range ds {
		switch d.t {
		case fap.TypeConversationOpenSync:
			sawSync = true
		case fap.TypeHello:
			sawRoster = true
		}
	}
	if !sawSync || !sawRoster {
		t.Errorf("client frames = %v, want openSync + hello roster", types(ds))
	}
}

// TestMintFacetConversation_RefusesCrossAgentKey proves a session key that does
// not belong to the agent is refused rather than silently minting a conversation
// pointed at the wrong (default) session.
func TestMintFacetConversation_RefusesCrossAgentKey(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	registerBareAgent(h, "ag")

	if _, err := h.mintFacetConversation("ag", "other/cbranch"); err == nil {
		t.Fatal("want error for cross-agent session key, got nil")
	}
}

// TestDispatchCommand_ForegroundsRequester proves a command response carrying
// OpenConversationID emits a conversation.foreground frame to the requesting
// socket only (focus is device-local).
func TestDispatchCommand_ForegroundsRequester(t *testing.T) {
	h := newTestHub()
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "facet",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "forked", OpenConversationID: "cnew"}, nil
		},
	})
	conn := &appConn{hub: h, agentID: "ag", commands: reg}
	convClient := fakeClientFor(h)
	b := &convBinding{convID: "c1", sessionKey: "ag/c1", clients: map[*wsClient]struct{}{convClient: {}}}
	requester := fakeClientFor(h)

	h.dispatchCommand(requester, conn, b, command.Request{Name: "facet", Source: "app"})

	// The requesting socket gets the foreground directive.
	ds := drain(t, requester)
	var fg *decoded
	for i := range ds {
		if ds[i].t == fap.TypeConversationForeground {
			fg = &ds[i]
		}
	}
	if fg == nil {
		t.Fatalf("requester frames = %v, want a conversation.foreground", types(ds))
	}
	if fg.d["conversationId"] != "cnew" {
		t.Errorf("foreground convId = %v, want cnew", fg.d["conversationId"])
	}

	// The conversation's other client must NOT receive the foreground directive.
	for _, d := range drain(t, convClient) {
		if d.t == fap.TypeConversationForeground {
			t.Error("conversation client received a foreground directive; focus must be requester-only")
		}
	}
}

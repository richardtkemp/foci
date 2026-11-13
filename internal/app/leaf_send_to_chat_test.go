package app

import (
	"context"
	"encoding/json"
	"testing"

	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tools"
)

// TestSendToChatTool_DepthLeafOwnConversation (#3087, Dick 2026-11-13: "An
// image sent by a depth leaf arrives in the parent rather than the leaf. It
// should go to the leaf!"): send_to_chat from an open depth leaf lands in the
// leaf's own conversation, file and text alike, not in the root chat whose id
// its session key carries.
func TestSendToChatTool_DepthLeafOwnConversation(t *testing.T) {
	h, root, sock, file := mediaHub(t)
	branch := root.key() + "/b1"
	leaf := registerLeaf(t, h, root, branch, "leaf-1", "One")
	leaf.attach(sock)
	drain(t, sock)

	tool := tools.NewSendToChatTool(func(sk string) platform.Sender {
		return h.BotForSession(sk)
	}, nil, func(sk string) session.SessionType {
		if sk == branch {
			return session.SessionTypeDepth
		}
		return session.SessionTypeChat
	}, nil)
	ctx := tools.WithSessionKey(context.Background(), branch)

	for _, sendAs := range []string{"document", "photo"} {
		params, err := json.Marshal(map[string]string{"file": file, "send_as": sendAs})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tool.Execute(ctx, params); err != nil {
			t.Fatalf("%s: %v", sendAs, err)
		}
		got := mediaFrames(t, sock)
		if len(got) != 1 || got[0].d["conversationId"] != leaf.convID {
			t.Errorf("%s: frames = %v, want one in the leaf's conversation %s (root is %s)", sendAs, got, leaf.convID, root.convID)
		}
	}

	params, err := json.Marshal(map[string]string{"text": "hello from the leaf"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(ctx, params); err != nil {
		t.Fatalf("text: %v", err)
	}
	var msgs []decoded
	for _, d := range drain(t, sock) {
		if d.t == "message" {
			msgs = append(msgs, d)
		}
	}
	if len(msgs) != 1 || msgs[0].d["conversationId"] != leaf.convID {
		t.Fatalf("text: frames = %v, want one in the leaf's conversation %s", msgs, leaf.convID)
	}
	if txt, _ := msgs[0].d["text"].(string); txt != "hello from the leaf" {
		t.Errorf("text = %q, want it unheaded (the leaf is the sender's own session)", txt)
	}
}

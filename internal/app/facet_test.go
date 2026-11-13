package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"foci/internal/command"
	"foci/internal/fap"
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

	convID, err := h.mintFacetConversation("ag", "ag/cbranch", command.FirstMessage{})
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

	if _, err := h.mintFacetConversation("ag", "other/cbranch", command.FirstMessage{}); err == nil {
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
	b := withIdent(&convBinding{convID: "c1", clients: map[*wsClient]struct{}{convClient: {}}}, "", "ag/c1")
	requester := fakeClientFor(h)

	h.dispatchCommand(requester, conn, b, command.Request{Name: "facet", Source: "app"}, "")

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

// bufferedUserMessages decodes b's buffered frames and returns the user-role
// messages among them.
func bufferedUserMessages(t *testing.T, b *convBinding) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, f := range b.buffer {
		var env map[string]any
		if err := json.Unmarshal([]byte(f.wire), &env); err != nil {
			t.Fatalf("decode buffered frame: %v", err)
		}
		if env["t"] != fap.TypeMessage {
			continue
		}
		body, _ := env["d"].(map[string]any)
		if body["role"] == "user" {
			out = append(out, body)
		}
	}
	return out
}

// TestMintFacetConversation_FirstMessage: a first message is sent into the NEW
// conversation as if typed there (#2780): a user-role echo in that conversation
// and a turn enqueued on the branch session, driven by the app connection and
// reconciled by the echo's id.
func TestMintFacetConversation_FirstMessage(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	fa := registerFakeAgent(h, "ag")

	convID, err := h.mintFacetConversation("ag", "ag/cbranch", command.FirstMessage{Text: "check the deploy", UserID: "dev1"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if fa.env == nil {
		t.Fatal("no turn enqueued for the first message")
	}
	env := fa.env
	if env.SessionKey != "ag/cbranch" {
		t.Errorf("turn session = %q, want the branch ag/cbranch", env.SessionKey)
	}
	if env.Text != "check the deploy" || env.UserID != "dev1" {
		t.Errorf("turn text/user = %q/%q, want check the deploy/dev1", env.Text, env.UserID)
	}
	if env.Original != convID || env.Ref.ConversationID != convID {
		t.Errorf("turn bound to conversation %v / ref %q, want %s", env.Original, env.Ref.ConversationID, convID)
	}
	if env.Driver == nil {
		t.Error("turn has no driver, so its reply cannot stream to the new chat")
	}
	b := h.bindingForSession("ag/cbranch")
	if b == nil || b.convID != convID {
		t.Fatalf("binding for ag/cbranch = %v, want convID %s", b, convID)
	}
	msgs := bufferedUserMessages(t, b)
	if len(msgs) != 1 {
		t.Fatalf("user messages in the new conversation = %v, want 1", msgs)
	}
	if msgs[0]["text"] != "check the deploy" || msgs[0]["messageId"] != env.Ref.MessageID {
		t.Errorf("echo = %v, want text 'check the deploy' with messageId %q (the turn's ref)", msgs[0], env.Ref.MessageID)
	}
}

// TestMintFacetConversation_NoFirstMessage: with no text nothing is enqueued and
// nothing is echoed; the new chat waits for the user.
func TestMintFacetConversation_NoFirstMessage(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	fa := registerFakeAgent(h, "ag")

	if _, err := h.mintFacetConversation("ag", "ag/cbranch", command.FirstMessage{}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if fa.env != nil {
		t.Errorf("a turn was enqueued with no first message: %+v", fa.env)
	}
	if msgs := bufferedUserMessages(t, h.bindingForSession("ag/cbranch")); len(msgs) != 0 {
		t.Errorf("user messages echoed with no first message: %v", msgs)
	}
}

// TestMintFacetConversation_FirstMessageNoAgent: a first message with no agent
// connection to run it is refused before minting, so no empty chat is left
// open on the user's devices.
func TestMintFacetConversation_FirstMessageNoAgent(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	registerBareAgent(h, "ag") // no agentRef

	if _, err := h.mintFacetConversation("ag", "ag/cbranch", command.FirstMessage{Text: "hi"}); err == nil {
		t.Fatal("want an error with no agent to run the first message")
	}
	if got := h.loadOpenChats(); len(got) != 0 {
		t.Errorf("open-set = %v after a refused mint, want empty", got)
	}
	if b := h.bindingForSession("ag/cbranch"); b != nil {
		t.Errorf("a refused mint bound conversation %s", b.convID)
	}
}

// TestMintFacetConversation_FirstReplyReachesConnectedClient (#3051): the
// agent's reply to a facet's first message must reach a device that is already
// connected, live. The minted binding used to start with no sockets attached,
// and nothing attached one: the device mirrors the server's openSync without
// sending its own open-set back (so the open-set attach pass never ran), and it
// had sent nothing in the new chat. The echo was recovered by HTTP backfill off
// the roster, but the reply streamed into a binding with no fan-out target, so
// the first reply never showed; the user's next send attached the socket and
// every later reply showed.
func TestMintFacetConversation_FirstReplyReachesConnectedClient(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	registerFakeAgent(h, "ag")
	c := fakeClient()
	h.clients[c] = struct{}{}

	convID, err := h.mintFacetConversation("ag", "ag/cbranch", command.FirstMessage{Text: "check the deploy", UserID: "dev1"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	b := h.bindingForSession("ag/cbranch")
	if b == nil || b.convID != convID {
		t.Fatalf("binding for ag/cbranch = %v, want convID %s", b, convID)
	}
	drain(t, c) // roster, openSync, echo: not what this test is about

	// The turn's reply, as the driver streams it into the new chat.
	final := "the deploy is fine"
	b.send(fap.TextEnd{ConversationID: convID, TurnID: "t1", MessageID: "m1", FinalText: &final})

	var sawReply bool
	for _, d := range drain(t, c) {
		if d.t == fap.TypeTextEnd && d.d["conversationId"] == convID {
			sawReply = true
		}
	}
	if !sawReply {
		t.Fatal("the connected device did not receive the facet's first reply live")
	}
}

// TestDeliverBinding_ServerCreatedReachesConnectedClient (#3051): the same
// shape on the other server-minted conversation — a session-blind send to an
// agent with no conversations mints one, and the send itself must reach a
// device that is connected now, not only after its next reconnect.
func TestDeliverBinding_ServerCreatedReachesConnectedClient(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	registerBareAgent(h, "ag")
	c := fakeClient()
	h.clients[c] = struct{}{}

	b, via := h.deliverBinding("ag")
	if via != "server-created" {
		t.Fatalf("deliverBinding rung = %q, want server-created", via)
	}
	drain(t, c)
	b.send(fap.ServerMessage{ConversationID: b.convID, MessageID: "m1", Role: "assistant", Text: "good morning"})

	var saw bool
	for _, d := range drain(t, c) {
		if d.t == fap.TypeMessage && d.d["conversationId"] == b.convID {
			saw = true
		}
	}
	if !saw {
		t.Fatal("the connected device did not receive the first send into a server-created conversation")
	}
}

// TestOpenSessionConversation proves #3097's app half: a session with no
// conversation gets one, bound to its key and opened on every device; a
// second call for the same session reuses it (no duplicate chat); an agent
// with no app connection is refused.
func TestOpenSessionConversation(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	registerBareAgent(h, "ag")
	c := fakeClient()
	h.clients[c] = struct{}{}

	convID, created, err := h.openSessionConversation("ag", "ag/inightly")
	if err != nil || !created || convID == "" {
		t.Fatalf("open = (%q, %v, %v), want a new conversation", convID, created, err)
	}
	if b := h.bindingForSession("ag/inightly"); b == nil || b.convID != convID {
		t.Fatalf("binding for ag/inightly = %v, want convID %s", b, convID)
	}
	if got := h.loadOpenChats(); len(got) != 1 || got[0] != convID {
		t.Errorf("open-set = %v, want [%s]", got, convID)
	}
	var sawSync bool
	for _, d := range drain(t, c) {
		if d.t == fap.TypeConversationOpenSync {
			sawSync = true
		}
	}
	if !sawSync {
		t.Error("client got no conversation.openSync: the chat would not open on the device")
	}

	again, created, err := h.openSessionConversation("ag", "ag/inightly")
	if err != nil || created || again != convID {
		t.Errorf("second open = (%q, %v, %v), want (%q, false, nil)", again, created, err, convID)
	}
	if got := h.loadOpenChats(); len(got) != 1 {
		t.Errorf("open-set after second open = %v, want one chat", got)
	}

	if _, _, err := h.openSessionConversation("noapp", "noapp/inightly"); err == nil {
		t.Error("agent with no app connection: want an error, got nil")
	}
}

// TestOpenSessionConversation_Concurrent proves concurrent opens of one new
// session mint exactly one conversation.
func TestOpenSessionConversation_Concurrent(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	registerBareAgent(h, "ag")

	const n = 8
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, _, err := h.openSessionConversation("ag", "ag/irace")
			if err != nil {
				t.Errorf("open: %v", err)
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Errorf("conversations minted = %v, want exactly one", seen)
	}
	if got := h.loadOpenChats(); len(got) != 1 {
		t.Errorf("open-set = %v, want one chat", got)
	}
}

// TestOpenSessionConversation_ReadableNameTitle proves #3097's title rule on a
// name with spaces, punctuation and an emoji: the new chat is named after the
// session (saved as its alias and sent in the roster every device gets), and
// a reply sent on the session lands in that chat.
func TestOpenSessionConversation_ReadableNameTitle(t *testing.T) {
	const name = "Tax 2026 Q&A 🥔"
	key := "ag/i" + name
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	registerBareAgent(h, "ag")
	c := fakeClient()
	h.clients[c] = struct{}{}

	convID, created, err := h.openSessionConversation("ag", key)
	if err != nil || !created {
		t.Fatalf("open = (%q, %v, %v), want a new conversation", convID, created, err)
	}
	b := h.bindingForSession(key)
	if b == nil || b.convID != convID {
		t.Fatalf("binding for %q = %v, want convID %s", key, b, convID)
	}
	if got := h.aliasFor(b); got != name {
		t.Errorf("chat name = %q, want %q", got, name)
	}

	// The roster the device receives names the chat and its session key.
	var rosterTitle, rosterKey string
	for _, d := range drain(t, c) {
		if d.t != fap.TypeHello {
			continue
		}
		raw, _ := json.Marshal(d.d)
		var hello fap.HelloServer
		if err := json.Unmarshal(raw, &hello); err != nil {
			t.Fatalf("decode hello: %v", err)
		}
		for _, a := range hello.Agents {
			for _, ci := range a.Conversations {
				if ci.ID == convID {
					rosterTitle, rosterKey = ci.Title, ci.SessionKey
				}
			}
		}
	}
	if rosterTitle != name || rosterKey != key {
		t.Errorf("roster row = (title %q, key %q), want (%q, %q)", rosterTitle, rosterKey, name, key)
	}

	// A reply on the session lands in the new chat, not a fallback.
	conn := h.BotForSession(key)
	if conn == nil {
		t.Fatal("BotForSession: nil")
	}
	if err := conn.SendText("reply for the potato"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	var landed bool
	for _, d := range drain(t, c) {
		if d.t == fap.TypeMessage && d.d["conversationId"] == convID && strings.Contains(fmt.Sprint(d.d["text"]), "reply for the potato") {
			if strings.Contains(fmt.Sprint(d.d["text"]), "Addressed to session") {
				t.Errorf("reply carries the misdelivery banner: %v", d.d["text"])
			}
			landed = true
		}
	}
	if !landed {
		t.Error("reply did not reach the new chat")
	}
}

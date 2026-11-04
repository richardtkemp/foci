package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/platform"
	"foci/internal/tools"
)

// mediaSends is every media send on appConn. chat is the chat a ...ToChat
// method addresses, for a connection bound to no session of its own.
func mediaSends(path string, chat int64) map[string]func(c *appConn) error {
	audio := []byte("x")
	return map[string]func(c *appConn) error{
		"SendDocument":        func(c *appConn) error { return c.SendDocument(path, "cap") },
		"SendVoice":           func(c *appConn) error { return c.SendVoice(path) },
		"SendVideo":           func(c *appConn) error { return c.SendVideo(path, "cap") },
		"SendPhoto":           func(c *appConn) error { return c.SendPhoto(path, "cap") },
		"SendAudio":           func(c *appConn) error { return c.SendAudio(path, "cap") },
		"SendAnimation":       func(c *appConn) error { return c.SendAnimation(path, "cap") },
		"SendVoiceData":       func(c *appConn) error { return c.SendVoiceData(audio) },
		"SendDocumentToChat":  func(c *appConn) error { return c.SendDocumentToChat(chat, path, "cap") },
		"SendVoiceToChat":     func(c *appConn) error { return c.SendVoiceToChat(chat, path) },
		"SendVideoToChat":     func(c *appConn) error { return c.SendVideoToChat(chat, path, "cap") },
		"SendPhotoToChat":     func(c *appConn) error { return c.SendPhotoToChat(chat, path, "cap") },
		"SendAudioToChat":     func(c *appConn) error { return c.SendAudioToChat(chat, path, "cap") },
		"SendAnimationToChat": func(c *appConn) error { return c.SendAnimationToChat(chat, path, "cap") },
		"SendVoiceDataToChat": func(c *appConn) error { return c.SendVoiceDataToChat(chat, audio) },
	}
}

// mediaHub is a real hub with one live conversation (returned with its socket)
// and a file to send.
func mediaHub(t *testing.T) (h *Hub, live *convBinding, sock *wsClient, file string) {
	t.Helper()
	h = newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: newTestIndex(t)}
	h.blobs = newBlobStore(t.TempDir())
	h.agents["ag"] = &appConn{hub: h, agentID: "ag"}
	live = h.ensureBinding(nil, "ag", "conv-live")
	sock = fakeClient()
	live.attach(sock)
	file = filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	return h, live, sock, file
}

// walkTo installs a family-tree walk that returns targets for every session,
// and records the sessions it was asked about.
func walkTo(t *testing.T, targets ...string) *[]string {
	t.Helper()
	asked := &[]string{}
	SetSessionChats(func(agentID, sessionKey string) []string {
		if agentID != "ag" {
			t.Errorf("walk asked for agent %q, want ag", agentID)
		}
		*asked = append(*asked, sessionKey)
		return targets
	})
	t.Cleanup(func() { SetSessionChats(nil) })
	return asked
}

func mediaFrames(t *testing.T, sock *wsClient) []decoded {
	t.Helper()
	var out []decoded
	for _, d := range drain(t, sock) {
		if d.t == "media" {
			out = append(out, d)
		}
	}
	return out
}

// TestMediaSend_NoChatAnywhereIsAnError: a media send that no chat can show
// returns errNoBinding and emits nothing, on every send method, with and
// without the walk installed. Before #2755 each of these returned nil.
func TestMediaSend_NoChatAnywhereIsAnError(t *testing.T) {
	const branch, chat = "ag/c7/b1", int64(99)
	for _, walk := range []string{"no walk", "walk finds nothing"} {
		h, _, sock, file := mediaHub(t)
		if walk == "walk finds nothing" {
			walkTo(t, branch, "ag/c7")
		}
		conn := h.BotForSession(branch)
		for name, send := range mediaSends(file, chat) {
			err := send(conn)
			if !errors.Is(err, errNoBinding) {
				t.Errorf("%s, %s: err = %v, want errNoBinding", walk, name, err)
			}
			if got := mediaFrames(t, sock); len(got) != 0 {
				t.Errorf("%s, %s: emitted %d media frames, want 0", walk, name, len(got))
			}
		}
	}
}

// TestMediaSend_GoesUpTheFamilyTree: with no conversation of its own, each send
// method lands in the first chat of the walk that has one.
func TestMediaSend_GoesUpTheFamilyTree(t *testing.T) {
	const branch, chat = "ag/c7/b1", int64(99)
	h, live, sock, file := mediaHub(t)
	asked := walkTo(t, branch, "ag/c7", live.key(), "ag/c8")
	conn := h.BotForSession(branch)
	chatKey := conn.SessionKeyForChat(chat)
	for name, send := range mediaSends(file, chat) {
		*asked = nil
		if err := send(conn); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		frames := mediaFrames(t, sock)
		if len(frames) != 1 {
			t.Errorf("%s: emitted %d media frames, want 1", name, len(frames))
			continue
		}
		if got := frames[0].d["conversationId"]; got != "conv-live" {
			t.Errorf("%s: landed in %v, want conv-live", name, got)
		}
		want := branch
		if strings.HasSuffix(name, "ToChat") {
			want = chatKey
		}
		if len(*asked) != 1 || (*asked)[0] != want {
			t.Errorf("%s: walked from %v, want [%s]", name, *asked, want)
		}
	}
}

// TestMediaSend_OwnChatFirst: a session with a conversation uses it, whatever
// the walk would say.
func TestMediaSend_OwnChatFirst(t *testing.T) {
	h, live, sock, file := mediaHub(t)
	asked := walkTo(t, "ag/c8")
	conn := h.BotForSession(live.key())
	for name, send := range mediaSends(file, live.chatID) {
		if err := send(conn); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if got := mediaFrames(t, sock); len(got) != 1 || got[0].d["conversationId"] != "conv-live" {
			t.Errorf("%s: frames = %v, want one in conv-live", name, got)
		}
	}
	if len(*asked) != 0 {
		t.Errorf("walk consulted for %v, want never", *asked)
	}
}

// TestMediaSend_FileErrorStillReported: a bad file is its own error, not a
// missing conversation, on the walked path too.
func TestMediaSend_FileErrorStillReported(t *testing.T) {
	h, live, sock, _ := mediaHub(t)
	walkTo(t, live.key())
	err := h.BotForSession("ag/c7/b1").SendDocument("/nonexistent/x", "")
	if err == nil || errors.Is(err, errNoBinding) {
		t.Errorf("err = %v, want the file's error", err)
	}
	if got := mediaFrames(t, sock); len(got) != 0 {
		t.Errorf("emitted %d media frames, want 0", len(got))
	}
}

// TestSendDocumentOwnChat_Strict: a prompt attachment does not go up the tree.
// The prompt walk retries chat by chat, so it would appear once per retry.
func TestSendDocumentOwnChat_Strict(t *testing.T) {
	h, live, sock, file := mediaHub(t)
	asked := walkTo(t, live.key())
	if err := h.BotForSession("ag/c7/b1").SendDocumentOwnChat(file, ""); !errors.Is(err, errNoBinding) {
		t.Errorf("unbound: err = %v, want errNoBinding", err)
	}
	if got := mediaFrames(t, sock); len(got) != 0 {
		t.Errorf("unbound: emitted %d media frames, want 0", len(got))
	}
	if len(*asked) != 0 {
		t.Errorf("walk consulted for %v, want never", *asked)
	}
	if err := h.BotForSession(live.key()).SendDocumentOwnChat(file, "cap"); err != nil {
		t.Errorf("bound: %v", err)
	}
	if got := mediaFrames(t, sock); len(got) != 1 || got[0].d["conversationId"] != "conv-live" {
		t.Errorf("bound: frames = %v, want one in conv-live", got)
	}
}

// TestCommandDoc_BranchGoesUp: the /command document of a branch session, which
// reaches the app through SendDocAndRemove with chat 0 (#2760), lands in a chat
// up the tree; with none, the caller gets the error.
func TestCommandDoc_BranchGoesUp(t *testing.T) {
	h, live, sock, file := mediaHub(t)
	conn := h.BotForSession("ag/c7/b1")
	if err := platform.SendDocAndRemove(conn, 0, file, ""); !errors.Is(err, errNoBinding) {
		t.Errorf("no chat: err = %v, want errNoBinding", err)
	}
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	walkTo(t, live.key())
	if err := platform.SendDocAndRemove(conn, 0, file, ""); err != nil {
		t.Errorf("chat above: %v", err)
	}
	if got := mediaFrames(t, sock); len(got) != 1 || got[0].d["conversationId"] != "conv-live" {
		t.Errorf("frames = %v, want one in conv-live", got)
	}
}

// TestSendToChatTool_TellsTheTruth: the agent reads "Sent" only for a file that
// emitted a media frame.
func TestSendToChatTool_TellsTheTruth(t *testing.T) {
	const key = "ag/ispawn-123"
	h, live, sock, file := mediaHub(t)
	tool := tools.NewSendToChatTool(func(sk string) platform.Sender {
		return h.BotForSession(sk)
	}, nil, nil, nil)
	params, err := json.Marshal(map[string]string{"file": file})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tools.WithSessionKey(context.Background(), key)

	if res, err := tool.Execute(ctx, params); !errors.Is(err, errNoBinding) {
		t.Errorf("no chat: result = %+v, err = %v, want errNoBinding", res, err)
	}
	if got := mediaFrames(t, sock); len(got) != 0 {
		t.Errorf("no chat: emitted %d media frames, want 0", len(got))
	}

	walkTo(t, live.key())
	if _, err := tool.Execute(ctx, params); err != nil {
		t.Errorf("chat above: %v", err)
	}
	if got := mediaFrames(t, sock); len(got) != 1 || got[0].d["conversationId"] != "conv-live" {
		t.Errorf("chat above: frames = %v, want one in conv-live", got)
	}
}

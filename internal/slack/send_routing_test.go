package slack

// send_routing_test.go: requirement 5 — every send kind reaches its own
// session's conversation (the DM, or the thread for a channel session), and
// never the other live session, another platform, or a guessed "default"
// conversation. The other-shaped session is registered and TOUCHED FIRST,
// so any "most recently active" / "last chat" heuristic would route there.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/route"
	"foci/internal/tools"
	"foci/internal/turn"
	"foci/internal/turnevent"
)

// routingFixture is two live Slack sessions of different shapes on one
// agent, plus the call-index mark of the current step's first API call.
type routingFixture struct {
	h         *harness
	dm        Target
	thread    Target
	dmKey     string
	threadKey string
	mark      int
}

// dmShape and threadShape are the fixture's two conversations (one DM, one
// channel thread of the same agent) — shared by the fixture builder and the
// tests that parametrise registration order.
func dmShape() Target {
	return Target{Kind: TargetDM, Channel: "D100", User: "U1"}
}

func threadShape() Target {
	return Target{Kind: TargetThread, Channel: "C200", ThreadTS: "1700000000.000200"}
}

// newRoutingFixture builds two live Slack sessions of different shapes on
// one agent. registrationOrder decides which target registers FIRST, so a
// misroute keyed on "the most recently registered target" is exercised under
// both orders (review F7): every shape must route correctly whichever
// conversation registered last.
func newRoutingFixture(t *testing.T, registrationOrder ...Target) *routingFixture {
	h := newHarness(t, "helen", nil)
	h.allow("helen", true, "U1")
	f := &routingFixture{
		h:      h,
		dm:     dmShape(),
		thread: threadShape(),
	}
	f.dmKey = sessionKeyFor("helen", f.dm)
	f.threadKey = sessionKeyFor("helen", f.thread)
	order := registrationOrder
	if len(order) == 0 {
		order = []Target{f.dm, f.thread}
	}
	for _, target := range order {
		if _, err := h.mgr.targets.register("helen", target); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// touchOther sends into the session of the OTHER shape, so it is the most
// recently active conversation before the step under test runs.
func (f *routingFixture) touchOther(t *testing.T, shape string) {
	t.Helper()
	other := f.dmKey
	if shape == "dm" {
		other = f.threadKey
	}
	if err := f.h.mgr.BotForSession(other).SendToSession(other, "warm"); err != nil {
		t.Fatal(err)
	}
	f.mark = f.h.fake.totalCalls()
}

// stepCalls returns the fake's recorded calls since the mark.
func (f *routingFixture) stepCalls(method string) []fakeCall {
	var out []fakeCall
	for i, c := range f.h.fake.callsSnapshot() {
		if i >= f.mark && c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// assertOnePost asserts exactly one chat.postMessage (since the mark)
// naming channel with the exact thread_ts, and zero posts elsewhere.
func (f *routingFixture) assertOnePost(t *testing.T, channel, threadTS, label string) {
	t.Helper()
	posts := f.stepCalls("chat.postMessage")
	if len(posts) != 1 {
		t.Fatalf("%s made %d posts, want exactly 1 to %s", label, len(posts), channel)
	}
	body := posts[0].Body
	if body["channel"] != channel {
		t.Errorf("%s posted to %v, want %s", label, body["channel"], channel)
	}
	gotTS, _ := body["thread_ts"].(string)
	if threadTS == "" && gotTS != "" {
		t.Errorf("%s set thread_ts=%q; a DM send is never threaded", label, gotTS)
	}
	if threadTS != "" && gotTS != threadTS {
		t.Errorf("%s thread_ts=%q, want %q", label, gotTS, threadTS)
	}
}

// assertOneUpdate asserts exactly one chat.update (since the mark) in
// channel, and returns it.
func (f *routingFixture) assertOneUpdate(t *testing.T, channel, label string) fakeCall {
	t.Helper()
	updates := f.stepCalls("chat.update")
	if len(updates) != 1 {
		t.Fatalf("%s made %d edits, want 1", label, len(updates))
	}
	if updates[0].Body["channel"] != channel {
		t.Errorf("%s edited %v, want %s", label, updates[0].Body["channel"], channel)
	}
	return updates[0]
}

// assertUploadsTo asserts exactly one completed upload (since the mark)
// belonging to channel with the exact thread_ts, and that the completion
// carries Slack's required wire shape: a "files" array naming the uploaded
// file, never a top-level file_id (docs.slack.dev makes files required; the
// real API rejects the other shape with missing_argument).
func (f *routingFixture) assertUploadsTo(t *testing.T, channel, threadTS, label string) {
	t.Helper()
	uploads := f.stepCalls("files.completeUploadExternal")
	if len(uploads) != 1 {
		t.Fatalf("%s: %d uploads, want 1", label, len(uploads))
	}
	body := uploads[0].Body
	if body["channels"] != channel {
		t.Errorf("%s uploaded to %v, want %s", label, body["channels"], channel)
	}
	gotTS, _ := body["thread_ts"].(string)
	if threadTS == "" && gotTS != "" {
		t.Errorf("%s threaded a DM upload (thread_ts=%q)", label, gotTS)
	}
	if threadTS != "" && gotTS != threadTS {
		t.Errorf("%s upload thread_ts=%q, want %q", label, gotTS, threadTS)
	}
	if _, hasTopLevel := body["file_id"]; hasTopLevel {
		t.Errorf("%s sent a top-level file_id; the file belongs in the files array: %v", label, body)
	}
	files, _ := body["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("%s files array = %v, want exactly one entry", label, body["files"])
	}
	entry, _ := files[0].(map[string]any)
	if entry["id"] != "F123" {
		t.Errorf("%s files[0].id = %v, want the id files.getUploadURLExternal handed out (F123)", label, entry["id"])
	}
}

// assertNoForeignTraffic fails when any call since the mark names a channel
// other than the session's own.
func (f *routingFixture) assertNoForeignTraffic(t *testing.T, ownChannel, label string) {
	t.Helper()
	for _, method := range []string{"chat.postMessage", "chat.update", "chat.delete", "files.completeUploadExternal"} {
		for _, c := range f.stepCalls(method) {
			if ch := channelOfCall(c); ch != "" && ch != ownChannel {
				t.Errorf("%s: %s touched channel %s (the other conversation)", label, method, ch)
			}
		}
	}
}

func channelOfCall(c fakeCall) string {
	if v, ok := c.Body["channel"].(string); ok {
		return v
	}
	if v, ok := c.Body["channels"].(string); ok {
		return v
	}
	return ""
}

// TestSend_Routing walks every send kind over both session shapes; each
// subtest gets a fresh fixture so per-kind call counts start from zero.
func TestSend_Routing(t *testing.T) {
	type stepFn func(t *testing.T, f *routingFixture, conn *Conn, target Target, sessionKey string)
	steps := []struct {
		name string
		run  stepFn
	}{
		{"turn reply through NewTurnSink", func(t *testing.T, f *routingFixture, conn *Conn, target Target, sessionKey string) {
			sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: sessionKey, ChatID: target.ChatID()})
			if sink == nil {
				t.Fatal("no sink for the session")
			}
			defer cleanup()
			sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "the reply"})
			f.assertOnePost(t, target.Channel, target.threadTS(), "turn reply")
		}},
		{"notification", func(t *testing.T, f *routingFixture, conn *Conn, target Target, sessionKey string) {
			conn.SendNotification("a notice")
			f.assertOnePost(t, target.Channel, target.threadTS(), "notification")
		}},
		{"SendNotificationDirect + EditMessageText (compaction ⏳→✅)", func(t *testing.T, f *routingFixture, conn *Conn, target Target, sessionKey string) {
			id := conn.SendNotificationDirect("⏳ compacting")
			if id == "" {
				t.Fatal("SendNotificationDirect returned no id")
			}
			if err := conn.EditMessageText(id, "✅ compacted"); err != nil {
				t.Fatal(err)
			}
			f.assertOnePost(t, target.Channel, target.threadTS(), "compaction notice")
			edit := f.assertOneUpdate(t, target.Channel, "compaction edit")
			if edit.Body["ts"] == "" || edit.Body["ts"] == nil {
				t.Errorf("compaction edit has no ts: %+v", edit.Body)
			}
		}},
		{"ask question buttons + cancel edit", func(t *testing.T, f *routingFixture, conn *Conn, target Target, sessionKey string) {
			_, err := platform.SendInteractiveMessageWithID(
				func() platform.Connection { return conn },
				"rt-ask", "", "Which colour?", []platform.ButtonChoice{{Label: "Red", Data: "qa:0"}}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			f.assertOnePost(t, target.Channel, target.threadTS(), "ask question")
			posts := f.stepCalls("chat.postMessage")
			if blob, _ := json.Marshal(posts[0].Body["blocks"]); !strings.Contains(string(blob), `"block_id":"foci:0:`+sessionKey+`"`) {
				t.Errorf("ask blocks carry wrong session in block_id: %s", blob)
			}
			if err := platform.CancelInteractiveMessage("rt-ask", "resolved"); err != nil {
				t.Fatal(err)
			}
			f.assertOneUpdate(t, target.Channel, "cancel edit")
		}},
		{"quiz reveal (route.NotifySessionChat)", func(t *testing.T, f *routingFixture, conn *Conn, target Target, sessionKey string) {
			cm := platform.NewConnectionManagerAdapter[*Conn](f.h.mgr)
			route.NotifySessionChat(cm, "helen", sessionKey, "the reveal")
			f.assertOnePost(t, target.Channel, target.threadTS(), "quiz reveal")
		}},
		{"SendToSession (foci send)", func(t *testing.T, f *routingFixture, conn *Conn, target Target, sessionKey string) {
			if err := conn.SendToSession(sessionKey, "agent reply"); err != nil {
				t.Fatal(err)
			}
			f.assertOnePost(t, target.Channel, target.threadTS(), "send_to_session")
		}},
		{"SendInjectedMessage (wake result)", func(t *testing.T, f *routingFixture, conn *Conn, target Target, sessionKey string) {
			if err := conn.SendInjectedMessage(sessionKey, "reminder fired"); err != nil {
				t.Fatal(err)
			}
			f.assertOnePost(t, target.Channel, target.threadTS(), "injected message")
		}},
		{"wake via turn.SessionSink", func(t *testing.T, f *routingFixture, conn *Conn, target Target, sessionKey string) {
			sink := turn.NewSessionSink(conn, sessionKey, "scheduled_wake")
			sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "wake result"})
			f.assertOnePost(t, target.Channel, target.threadTS(), "wake result")
		}},
		{"rate-limit notice (route.ConnFor default arm)", func(t *testing.T, f *routingFixture, conn *Conn, target Target, sessionKey string) {
			cm := platform.NewConnectionManagerAdapter[*Conn](f.h.mgr)
			c, outcome := route.ConnFor(cm, "helen", sessionKey, route.PolicyFallback)
			if c == nil || outcome != route.DeliveredToSession {
				t.Fatalf("ConnFor = %v, %v; want the session's own connection", c, outcome)
			}
			c.SendNotification("⚡ rate limited")
			f.assertOnePost(t, target.Channel, target.threadTS(), "rate-limit notice")
		}},
		{"SendEditableInjected + EditInjectedMessage", func(t *testing.T, f *routingFixture, conn *Conn, target Target, sessionKey string) {
			id, err := conn.SendEditableInjected(sessionKey, "sign-in link v1")
			if err != nil {
				t.Fatal(err)
			}
			if err := conn.EditInjectedMessage(sessionKey, id, "sign-in link v2"); err != nil {
				t.Fatal(err)
			}
			f.assertOnePost(t, target.Channel, target.threadTS(), "editable message")
			f.assertOneUpdate(t, target.Channel, "editable edit")
		}},
		{"SendTextToChat", func(t *testing.T, f *routingFixture, conn *Conn, target Target, sessionKey string) {
			if err := conn.SendTextToChat(target.ChatID(), "by chat id"); err != nil {
				t.Fatal(err)
			}
			f.assertOnePost(t, target.Channel, target.threadTS(), "SendTextToChat")
		}},
		{"SendDocument (bound)", func(t *testing.T, f *routingFixture, conn *Conn, target Target, sessionKey string) {
			path := filepath.Join(t.TempDir(), "report.txt")
			if err := os.WriteFile(path, []byte("body"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := conn.SendDocument(path, "the report"); err != nil {
				t.Fatal(err)
			}
			f.assertUploadsTo(t, target.Channel, target.threadTS(), "SendDocument")
		}},
		{"SendDocumentToChat", func(t *testing.T, f *routingFixture, conn *Conn, target Target, sessionKey string) {
			path := filepath.Join(t.TempDir(), "data.csv")
			if err := os.WriteFile(path, []byte("1,2"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := conn.SendDocumentToChat(target.ChatID(), path, ""); err != nil {
				t.Fatal(err)
			}
			f.assertUploadsTo(t, target.Channel, target.threadTS(), "SendDocumentToChat")
		}},
	}

	// Both registration orders (review F7): whichever conversation
	// registered last, each shape must reach its own conversation — a
	// misroute to "the most recently registered target" fails one shape
	// under every order.
	orders := []struct {
		name    string
		targets []Target
	}{
		{"dm-registered-last", []Target{threadShape(), dmShape()}},
		{"thread-registered-last", []Target{dmShape(), threadShape()}},
	}
	for _, shape := range []string{"dm", "thread"} {
		for _, order := range orders {
			for _, step := range steps {
				t.Run(shape+"/"+order.name+"/"+step.name, func(t *testing.T) {
					f := newRoutingFixture(t, order.targets...)
					f.touchOther(t, shape)
					target := f.dm
					sessionKey := f.dmKey
					if shape == "thread" {
						target = f.thread
						sessionKey = f.threadKey
					}
					conn := f.h.mgr.BotForSession(sessionKey)
					if conn == nil {
						t.Fatalf("no bound conn for %s", sessionKey)
					}
					step.run(t, f, conn, target, sessionKey)
					f.assertNoForeignTraffic(t, target.Channel, step.name)
				})
			}
		}
	}
}

// TestSend_ChannelReplyNotBroadcast proves a thread-targeted post never sets
// reply_broadcast: the channel itself is not notified.
func TestSend_ChannelReplyNotBroadcast(t *testing.T) {
	f := newRoutingFixture(t)
	conn := f.h.mgr.BotForSession(f.threadKey)
	if err := conn.SendToSession(f.threadKey, "thread only"); err != nil {
		t.Fatal(err)
	}
	for _, post := range f.h.fake.postCalls() {
		if post.Body["reply_broadcast"] != nil {
			t.Error("post set reply_broadcast; a thread reply must stay in the thread")
		}
	}
}

// TestSend_EditByIdStaysInItsChannel proves edit-by-id safety on every
// surface: a bound view edits its own conversation's id, refuses one naming
// a different conversation, and the UNBOUND connection — with no session
// whose conversation an edit could be checked against — refuses every
// edit-by-id, however well-formed (every legitimate caller edits through a
// session's own bound view; #2459 req 5).
func TestSend_EditByIdStaysInItsChannel(t *testing.T) {
	f := newRoutingFixture(t)
	bound := f.h.mgr.BotForSession(f.dmKey)

	// Own-conversation id: the edit lands on that message.
	own := msgRef{Channel: f.dm.Channel, TS: "7"}
	if err := bound.EditMessageText(own.String(), "edited"); err != nil {
		t.Fatal(err)
	}
	edit := f.assertOneUpdate(t, f.dm.Channel, "bound edit by id")
	if edit.Body["ts"] != "7" {
		t.Errorf("edit ts = %v, want 7", edit.Body["ts"])
	}

	// Bound view: an id naming another conversation is refused.
	if err := bound.EditMessageText(msgRef{Channel: "C999", TS: "9"}.String(), "nope"); err == nil {
		t.Error("bound view edited a foreign channel's message; want refusal")
	}
	if got := len(f.h.fake.callsFor("chat.update")); got != 1 {
		t.Errorf("refused edit still made %d updates", got)
	}

	// Unbound connection: no session to confine the edit to, so every
	// id — even one of the agent's own conversations — is refused, on
	// both edit surfaces, with no API call. The refusal names the real
	// reason (the unbound connection), not a phantom empty session.
	unbound := f.h.mgr.PrimaryBot("helen")
	for name, err := range map[string]error{
		"EditMessageText":        unbound.EditMessageText(own.String(), "edited"),
		"EditMessageWithButtons": unbound.EditMessageWithButtons(own.String(), "edited", []platform.ButtonChoice{{Label: "Allow", Data: "allow"}}, "im:"),
	} {
		if err == nil || !strings.Contains(err.Error(), "unbound connection") {
			t.Errorf("%s on the unbound conn = %v, want the unbound-connection refusal", name, err)
		}
	}
	if got := len(f.h.fake.callsFor("chat.update")); got != 1 {
		t.Errorf("unbound edits made %d more updates, want 0", got-1)
	}
}

// TestSend_StreamedEditsUpdateOneMessage proves the streaming surface (when
// enabled) posts one message and then chat.updates THAT message — the edit
// carries the posted message's ts in its own channel, and a second post
// never happens, however many deltas arrive. The interval is shrunk so the
// pump really fires between deltas (the default 2s would only ever exercise
// the terminal edit).
func TestSend_StreamedEditsUpdateOneMessage(t *testing.T) {
	f := newRoutingFixture(t)
	core := f.h.core("helen")
	core.streamOutput = true
	core.streamInterval = time.Millisecond
	conn := f.h.mgr.BotForSession(f.dmKey)

	sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: f.dmKey, ChatID: f.dm.ChatID()})
	if sink == nil {
		t.Fatal("no sink")
	}
	defer cleanup()

	sink.Emit(context.Background(), turnevent.TurnStart{})
	sink.Emit(context.Background(), turnevent.TextDelta{Delta: "part one "})
	time.Sleep(20 * time.Millisecond) // let the pump tick: a mid-stream edit
	sink.Emit(context.Background(), turnevent.TextDelta{Delta: "part two"})
	time.Sleep(20 * time.Millisecond)
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "part one part two"})

	posts := f.h.fake.postsTo("D100")
	if len(posts) != 1 {
		t.Fatalf("streamed turn made %d posts, want exactly 1 (edits, not reposts): %+v", len(posts), posts)
	}
	// The fake stamps a deterministic ts per channel; the one message the
	// stream posted is therefore the only ts any edit may carry.
	streamedTS := bodyTS(map[string]any{"channel": "D100"}) + ".000001"
	updates := f.h.fake.callsFor("chat.update")
	if len(updates) == 0 {
		t.Fatal("stream never edited the message; the cadence must produce chat.update calls")
	}
	for _, u := range updates {
		if u.Body["channel"] != "D100" {
			t.Errorf("stream edit channel = %v, want D100", u.Body["channel"])
		}
		if u.Body["ts"] != streamedTS {
			t.Errorf("stream edit ts = %v, want the posted message's ts %q", u.Body["ts"], streamedTS)
		}
	}
}

// TestNewTurnSinkUnregisteredSessionDroppedVisibly pins the driver contract
// for a session with no registered Slack conversation: NewTurnSink returns
// (nil, nil) — the agent's RunTurn then skips the whole turn before the
// model runs — and the refusal is LOGGED, never silent (#2459 req 5, 6:
// refusing to guess a conversation must be visible). Inbound messages
// register before enqueueing, so this is the defensive arm.
func TestNewTurnSinkUnregisteredSessionDroppedVisibly(t *testing.T) {
	f := newRoutingFixture(t)
	logs := captureLogLines(t, log.WARN)
	unregistered := "helen/c424242"
	conn := (&Conn{core: f.h.core("helen")}).boundTo(unregistered)

	sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: unregistered})
	if sink != nil || cleanup != nil {
		t.Fatalf("NewTurnSink(unregistered) sink=%v cleanup=%v; want (nil, nil) so the agent skips the turn", sink, cleanup != nil)
	}
	if got := f.h.fake.totalCalls(); got != 0 {
		t.Errorf("unregistered turn made %d API calls, want 0", got)
	}
	dropped := 0
	for _, line := range logs() {
		if strings.Contains(line, unregistered) && strings.Contains(line, "no Slack conversation registered") {
			dropped++
		}
	}
	if dropped != 1 {
		t.Errorf("unregistered-turn drop logged %d matching WARNs, want 1:\n%s", dropped, strings.Join(logs(), "\n"))
	}
}

// TestSend_ButtonsCarrySessionAndRows proves the button payload shape: one
// actions block per row, the value prefixed "im:", the block_id carrying the
// session, toggles dropped.
func TestSend_ButtonsCarrySessionAndRows(t *testing.T) {
	f := newRoutingFixture(t)
	conn := f.h.mgr.BotForSession(f.dmKey)

	buttons := []platform.ButtonChoice{
		{Label: "Allow", Data: "allow", Row: 0},
		{Label: "Deny", Data: "deny", Row: 0},
		{Label: "Details", Data: "det", Row: 1},
		{Label: "Show diff", Data: "show", Row: 2, Toggle: &platform.ButtonToggle{ExtraBody: "diff", ShowLabel: "Show diff", HideLabel: "Hide diff"}},
	}
	if _, err := conn.SendTextWithButtons("Approve?", buttons, "im:"); err != nil {
		t.Fatal(err)
	}
	posts := f.h.fake.postsTo("D100")
	if len(posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(posts))
	}
	blocks, _ := posts[0].Body["blocks"].([]any)
	var actionsBlocks []map[string]any
	for _, b := range blocks {
		if blk, ok := b.(map[string]any); ok && blk["type"] == "actions" {
			actionsBlocks = append(actionsBlocks, blk)
		}
	}
	if len(actionsBlocks) != 2 {
		t.Fatalf("actions blocks = %d, want 2 (one per row, toggle dropped)", len(actionsBlocks))
	}
	elements0, _ := actionsBlocks[0]["elements"].([]any)
	if len(elements0) != 2 {
		t.Errorf("row 0 buttons = %d, want 2", len(elements0))
	}
	first, _ := elements0[0].(map[string]any)
	if first["value"] != "im:allow" {
		t.Errorf("button value = %v, want the im:-prefixed data", first["value"])
	}
	for i, blk := range actionsBlocks {
		id, _ := blk["block_id"].(string)
		want := "foci:" + string(rune('0'+i)) + ":" + f.dmKey
		if id != want {
			t.Errorf("block %d id = %q, want %q", i, id, want)
		}
	}
}

// TestButtonBlockID_LengthCap proves an over-long block_id (a session key
// long enough to pass Slack's 255-character cap) refuses the send rather
// than truncating the session away.
func TestButtonBlockID_LengthCap(t *testing.T) {
	if id, ok := buttonBlockID(0, "a/c1"); !ok || id != "foci:0:a/c1" {
		t.Errorf("buttonBlockID = %q, %v", id, ok)
	}
	longKey := strings.Repeat("k", 300)
	if _, ok := buttonBlockID(0, longKey); ok {
		t.Error("over-long block_id accepted; want refused")
	}

	// End to end: a bound view whose session key exceeds the cap makes zero
	// API calls and returns the refusal.
	h := newHarness(t, "helen", nil)
	longAgent := strings.Repeat("a", 299)
	target := Target{Kind: TargetDM, Channel: "D100", User: "U1"}
	if _, err := h.mgr.targets.register(longAgent, target); err != nil {
		t.Fatal(err)
	}
	core := h.newCore(longAgent, configAllowed("U1"))
	conn := (&Conn{core: core}).boundTo(sessionKeyFor(longAgent, target))
	before := h.fake.totalCalls()
	if _, err := conn.SendTextWithButtons("q", []platform.ButtonChoice{{Label: "x", Data: "y"}}, "im:"); err == nil {
		t.Fatal("send with an over-long block_id succeeded; want refusal")
	}
	if got := h.fake.totalCalls(); got != before {
		t.Errorf("refused send made %d API calls, want 0", got-before)
	}
}

// TestSend_SessionBlindWithoutBoundSessionDropped proves requirement 6's
// drop rule: every session-blind method on the UNBOUND connection drops
// with errNoSessionAddress (or its void equivalent) and posts nothing.
func TestSend_SessionBlindWithoutBoundSessionDropped(t *testing.T) {
	f := newRoutingFixture(t)
	unbound := f.h.mgr.PrimaryBot("helen")

	if err := unbound.SendText("hello"); !errors.Is(err, errNoSessionAddress) {
		t.Errorf("SendText err = %v, want errNoSessionAddress", err)
	}
	if _, err := unbound.SendTextWithButtons("q", []platform.ButtonChoice{{Label: "x", Data: "y"}}, "im:"); !errors.Is(err, errNoSessionAddress) {
		t.Errorf("SendTextWithButtons err = %v, want errNoSessionAddress", err)
	}
	if err := unbound.SendDocument("/nonexistent", ""); !errors.Is(err, errNoSessionAddress) {
		t.Errorf("SendDocument err = %v, want errNoSessionAddress", err)
	}
	unbound.SendNotification("notice") // void: dropped, logged
	if id := unbound.SendNotificationDirect("notice"); id != "" {
		t.Errorf("SendNotificationDirect = %q, want \"\"", id)
	}
	unbound.SetTyping(true) // no-op, no panic

	if got := f.h.fake.totalCalls(); got != 0 {
		t.Errorf("unbound session-blind sends made %d API calls, want 0", got)
	}
}

// TestSend_SendToChatToolEndToEnd drives the real send_to_chat tool for a
// Slack session: the file lands in the session's own conversation and the
// text rides as the upload's comment with NO "[[ message from" header (the
// bound view reports its own session key).
func TestSend_SendToChatToolEndToEnd(t *testing.T) {
	f := newRoutingFixture(t)
	path := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(path, []byte("# plan"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := tools.NewSendToChatTool(
		func(sessionKey string) platform.Sender { return f.h.mgr.BotForSession(sessionKey) },
		nil, nil, nil)
	ctx := tools.WithSessionKey(context.Background(), f.dmKey)
	if _, err := tool.Execute(ctx, mustJSON(map[string]any{
		"text": "the plan",
		"file": path,
	})); err != nil {
		t.Fatal(err)
	}

	f.assertUploadsTo(t, f.dm.Channel, "", "send_to_chat")
	for _, up := range f.h.fake.uploads() {
		if comment, _ := up.Body["initial_comment"].(string); strings.Contains(comment, "[[ message from") {
			t.Errorf("send_to_chat prepended a cross-session header (%q); the session's own send needs none", comment)
		} else if comment != "the plan" {
			t.Errorf("upload comment = %q, want the caption", comment)
		}
		files, _ := up.Body["files"].([]any)
		entry, _ := files[0].(map[string]any)
		if entry["title"] != "plan.md" {
			t.Errorf("upload files[0].title = %v, want the filename", entry["title"])
		}
	}
	if posts := f.h.fake.postCalls(); len(posts) != 0 {
		t.Errorf("captioned file produced %d extra posts, want 0", len(posts))
	}
}

// TestSend_EditInjectedMessageRefusesForeignConversation proves requirement 5's
// guard on the session-addressed edit: a bound DM view refuses to edit a
// message id naming another conversation (a channel thread), makes no API
// call, and still edits its own conversation's messages.
func TestSend_EditInjectedMessageRefusesForeignConversation(t *testing.T) {
	f := newRoutingFixture(t)
	dmConn := f.h.mgr.BotForSession(f.dmKey)
	threadConn := f.h.mgr.BotForSession(f.threadKey)

	// An id from the thread's conversation, handed to the DM session's view.
	threadID, err := threadConn.SendEditableInjected(f.threadKey, "thread message")
	if err != nil {
		t.Fatal(err)
	}
	mark := f.h.fake.totalCalls()
	if err := dmConn.EditInjectedMessage(f.dmKey, threadID, "hijacked"); err == nil || !strings.Contains(err.Error(), "different conversation") {
		t.Fatalf("a DM session's view edited a thread message; want the cross-conversation refusal, got %v", err)
	}
	if got := f.h.fake.totalCalls(); got != mark {
		t.Errorf("refused edit made %d API calls, want 0", got-mark)
	}

	// The same view edits its own conversation's message.
	dmID, err := dmConn.SendEditableInjected(f.dmKey, "dm message")
	if err != nil {
		t.Fatal(err)
	}
	if err := dmConn.EditInjectedMessage(f.dmKey, dmID, "dm message v2"); err != nil {
		t.Fatalf("edit of the session's own message failed: %v", err)
	}
	edits := f.h.fake.callsFor("chat.update")
	if len(edits) != 1 || edits[0].Body["channel"] != f.dm.Channel {
		t.Errorf("edits = %+v, want exactly one in the DM channel %s", edits, f.dm.Channel)
	}
}

// TestSend_SendToSessionUnroutableReportsError proves an unroutable
// SendToSession reports failure (like Telegram) instead of claiming success:
// a session with no registered Slack conversation returns an error, so
// callers (send_to_session, deliverToSessionChat, wake results) record the
// truth.
func TestSend_SendToSessionUnroutableReportsError(t *testing.T) {
	f := newRoutingFixture(t)
	unbound := f.h.mgr.PrimaryBot("helen")
	mark := f.h.fake.totalCalls()

	if err := unbound.SendToSession("helen/c424242", "anyone?"); !errors.Is(err, errNoSessionAddress) {
		t.Errorf("SendToSession(unregistered) err = %v, want errNoSessionAddress (the send was not delivered)", err)
	}
	// The injected wrapper inherits the truthful error.
	if err := unbound.SendInjectedMessage("helen/c424242", "wake?"); !errors.Is(err, errNoSessionAddress) {
		t.Errorf("SendInjectedMessage(unregistered) err = %v, want errNoSessionAddress", err)
	}
	if got := f.h.fake.totalCalls(); got != mark {
		t.Errorf("unroutable sends made %d API calls, want 0", got-mark)
	}

	// Positive control: the registered session still delivers.
	if err := unbound.SendToSession(f.dmKey, "hello"); err != nil {
		t.Fatalf("SendToSession(registered) = %v", err)
	}
	if posts := f.h.fake.postsTo(f.dm.Channel); len(posts) != 1 {
		t.Errorf("posts to the DM = %d, want 1", len(posts))
	}
}

// TestSend_EditInjectedMessageRefusesUnregisteredSession proves the edit guard
// fails closed on the SessionMessageEditor surface: a session key with no
// resolvable Slack conversation — never registered, or another agent's — is
// refused before any API call with the no-conversation diagnosis (not the
// different-conversation one: the session has nothing to compare, which is
// its own refusal), never edited on the strength of having nothing to
// compare.
func TestSend_EditInjectedMessageRefusesUnregisteredSession(t *testing.T) {
	f := newRoutingFixture(t)
	unbound := f.h.mgr.PrimaryBot("helen")

	dmID, err := unbound.SendEditableInjected(f.dmKey, "dm message")
	if err != nil {
		t.Fatal(err)
	}
	mark := f.h.fake.totalCalls()
	for _, key := range []string{
		"helen/c424242",               // this agent, never registered
		sessionKeyFor("zz", f.dm),     // the DM's conversation under another agent
		sessionKeyFor("zz", f.thread), // the thread's conversation under another agent
	} {
		if err := unbound.EditInjectedMessage(key, dmID, "hijacked"); err == nil || !strings.Contains(err.Error(), "no Slack conversation registered") {
			t.Errorf("EditInjectedMessage(%s) = %v; want the no-conversation refusal (the unregistered-session branch, not the mismatch one)", key, err)
		}
	}
	if got := f.h.fake.totalCalls(); got != mark {
		t.Errorf("refused edits made %d API calls, want 0", got-mark)
	}
}

// TestSend_BoundViewToChatRefusesForeignChat proves the chat-addressed sends
// of a BOUND view refuse any chat other than the view's own session's
// conversation (text, media and media data alike): a session's sends never
// land in another session's conversation. The unbound connection keeps the
// cross-session shape — any registered chat of the agent.
func TestSend_BoundViewToChatRefusesForeignChat(t *testing.T) {
	f := newRoutingFixture(t)
	dmConn := f.h.mgr.BotForSession(f.dmKey)
	threadChat := f.thread.ChatID()
	mark := f.h.fake.totalCalls()

	if err := dmConn.SendTextToChat(threadChat, "into the thread"); !errors.Is(err, errNoSessionAddress) {
		t.Errorf("SendTextToChat(foreign chat) err = %v, want errNoSessionAddress", err)
	}
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := dmConn.SendDocumentToChat(threadChat, path, ""); !errors.Is(err, errNoSessionAddress) {
		t.Errorf("SendDocumentToChat(foreign chat) err = %v, want errNoSessionAddress", err)
	}
	if err := dmConn.SendVoiceDataToChat(threadChat, []byte("x")); !errors.Is(err, errNoSessionAddress) {
		t.Errorf("SendVoiceDataToChat(foreign chat) err = %v, want errNoSessionAddress", err)
	}
	if got := f.h.fake.totalCalls(); got != mark {
		t.Fatalf("refused …ToChat sends made %d API calls, want 0", got-mark)
	}

	// The view's own session's chat still works.
	if err := dmConn.SendTextToChat(f.dm.ChatID(), "own conversation"); err != nil {
		t.Fatalf("SendTextToChat(own chat) = %v", err)
	}
	if posts := f.h.fake.postsTo(f.dm.Channel); len(posts) != 1 {
		t.Errorf("posts to the DM = %d, want 1", len(posts))
	}

	// The unbound connection is not session-bound: it may still address any
	// registered chat of the agent (the cross-session send_to_chat shape).
	unbound := f.h.mgr.PrimaryBot("helen")
	if err := unbound.SendTextToChat(threadChat, "cross-session"); err != nil {
		t.Fatalf("unbound SendTextToChat(thread chat) = %v", err)
	}
	posts := f.h.fake.postsTo(f.thread.Channel)
	if len(posts) != 1 {
		t.Fatalf("posts to the thread channel = %d, want 1", len(posts))
	}
	if got, _ := posts[0].Body["thread_ts"].(string); got != f.thread.ThreadTS {
		t.Errorf("cross-session post thread_ts = %q, want %q", got, f.thread.ThreadTS)
	}
}

// TestSend_EditRefusesCrossThreadInSameChannel proves the guard's thread
// dimension (#2459 req 5): two threads of one channel are different
// conversations, so a thread session's edit of the OTHER thread's message
// id is refused on both edit surfaces (the bound view's EditMessageText and
// the session-addressed EditInjectedMessage) with no API call — while its own
// thread's ids still edit, and the ids carry their thread.
func TestSend_EditRefusesCrossThreadInSameChannel(t *testing.T) {
	h := newHarness(t, "helen", nil)
	h.allow("helen", true, "U1")
	one := Target{Kind: TargetThread, Channel: "C1", ThreadTS: "100.1"}
	two := Target{Kind: TargetThread, Channel: "C1", ThreadTS: "200.2"}
	oneKey := sessionKeyFor("helen", one)
	twoKey := sessionKeyFor("helen", two)
	for _, target := range []Target{one, two} {
		if _, err := h.mgr.targets.register("helen", target); err != nil {
			t.Fatal(err)
		}
	}
	oneConn := h.mgr.BotForSession(oneKey)
	twoConn := h.mgr.BotForSession(twoKey)

	oneID, err := oneConn.SendEditableInjected(oneKey, "thread one says")
	if err != nil {
		t.Fatal(err)
	}
	if ref, ok := parseMsgRef(oneID); !ok || ref.ThreadTS != "100.1" {
		t.Errorf("thread message id %q does not carry its thread (parsed %+v)", oneID, ref)
	}
	mark := h.fake.totalCalls()

	if err := twoConn.EditInjectedMessage(twoKey, oneID, "hijacked"); err == nil {
		t.Error("EditInjectedMessage edited another thread's message in the same channel; want refusal")
	}
	if err := twoConn.EditMessageText(oneID, "hijacked"); err == nil {
		t.Error("bound EditMessageText edited another thread's message in the same channel; want refusal")
	}
	if got := h.fake.totalCalls(); got != mark {
		t.Errorf("refused cross-thread edits made %d API calls, want 0", got-mark)
	}

	// A thread-less id (a DM shape) is equally foreign to a thread session.
	if err := twoConn.EditMessageText("D100:9", "hijacked"); err == nil {
		t.Error("thread session edited a DM message id; want refusal (different conversation)")
	}

	// The session's own thread still edits in place.
	twoID, err := twoConn.SendEditableInjected(twoKey, "thread two says")
	if err != nil {
		t.Fatal(err)
	}
	if err := twoConn.EditInjectedMessage(twoKey, twoID, "thread two v2"); err != nil {
		t.Fatalf("edit of the session's own thread failed: %v", err)
	}
	edits := h.fake.callsFor("chat.update")
	if len(edits) != 1 {
		t.Fatalf("edits = %d, want exactly 1", len(edits))
	}
	if edits[0].Body["channel"] != "C1" {
		t.Errorf("edit channel = %v, want C1", edits[0].Body["channel"])
	}
	if ref, _ := parseMsgRef(twoID); edits[0].Body["ts"] != ref.TS {
		t.Errorf("edit ts = %v, want the session's own message %q", edits[0].Body["ts"], ref.TS)
	}
}

// TestSend_MarkupControlCharactersEscaped proves every surface that carries
// text into a Slack-parsed field escapes Slack's control characters: a
// literal <!channel>, <!here> or <@U…> in agent text must render as text,
// never notify the channel or ping a user from the bot.
func TestSend_MarkupControlCharactersEscaped(t *testing.T) {
	f := newRoutingFixture(t)
	conn := f.h.mgr.BotForSession(f.threadKey)
	ping := "heads up <!channel> <!here> <@U999> a & b < c > d"
	want := "heads up &lt;!channel&gt; &lt;!here&gt; &lt;@U999&gt; a &amp; b &lt; c &gt; d"

	// Message text (chat.postMessage).
	if err := conn.SendText(ping); err != nil {
		t.Fatal(err)
	}
	posts := f.h.fake.postsTo(f.thread.Channel)
	if len(posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(posts))
	}
	if posts[0].Body["text"] != want {
		t.Errorf("post text = %q, want escaped %q", posts[0].Body["text"], want)
	}

	// Edit text (chat.update), by the id the connection itself issued.
	id := conn.SendNotificationDirect(ping)
	if id == "" {
		t.Fatal("SendNotificationDirect returned no id")
	}
	if err := conn.EditMessageText(id, ping); err != nil {
		t.Fatal(err)
	}
	edits := f.h.fake.callsFor("chat.update")
	if len(edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(edits))
	}
	if edits[0].Body["text"] != want {
		t.Errorf("edit text = %q, want escaped %q", edits[0].Body["text"], want)
	}

	// Button prompt body (mrkdwn section block).
	if _, err := conn.SendTextWithButtons(ping, []platform.ButtonChoice{{Label: "OK", Data: "ok"}}, "im:"); err != nil {
		t.Fatal(err)
	}
	promptPosts := f.h.fake.postsTo(f.thread.Channel)
	last := promptPosts[len(promptPosts)-1]
	blocks, _ := last.Body["blocks"].([]any)
	sectionText := ""
	for _, b := range blocks {
		if blk, ok := b.(map[string]any); ok && blk["type"] == "section" {
			if txt, ok := blk["text"].(map[string]any); ok {
				sectionText, _ = txt["text"].(string)
			}
		}
	}
	if sectionText != want {
		t.Errorf("prompt section text = %q, want escaped %q", sectionText, want)
	}

	// Upload comment (initial_comment).
	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, []byte("body"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := conn.SendDocument(path, ping); err != nil {
		t.Fatal(err)
	}
	for _, up := range f.h.fake.uploads() {
		if comment, _ := up.Body["initial_comment"].(string); comment != want {
			t.Errorf("upload comment = %q, want escaped %q", comment, want)
		}
	}
}

// TestSend_UnknownMessageIDNamedInRefusal proves every edit surface reports
// a malformed id as exactly that — naming the id — and NOT as
// errNoSessionAddress, which would claim the session had no conversation:
// the wrong diagnosis for an id this transport could never have issued.
func TestSend_UnknownMessageIDNamedInRefusal(t *testing.T) {
	f := newRoutingFixture(t)
	conn := f.h.mgr.BotForSession(f.dmKey)
	backend := &slackBackend{core: f.h.core("helen"), target: f.dm}

	for name, err := range map[string]error{
		"EditMessageText":     conn.EditMessageText("not-an-id", "x"),
		"EditInjectedMessage": conn.EditInjectedMessage(f.dmKey, "not-an-id", "x"),
		"EditChunk":           backend.EditChunk("not-an-id", "x"),
	} {
		if err == nil || !strings.Contains(err.Error(), "unknown message id") || errors.Is(err, errNoSessionAddress) {
			t.Errorf("%s of a malformed id = %v, want the unknown-id refusal", name, err)
		}
	}
}

func mustJSON(v any) json.RawMessage {
	blob, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return blob
}

// TestSend_BoundViewRefusesForeignSessionKey proves a bound view's pin holds
// on every method that takes an explicit session key (#2459 A1 review F2,
// req 5), as chatGuard already does for the chat-addressed sends: the thread
// session's view, handed the DM session's key, posts nothing, edits nothing
// and returns an error. A branch key of the view's own conversation is the
// same conversation and still sends; the unbound connection still addresses
// any registered session.
func TestSend_BoundViewRefusesForeignSessionKey(t *testing.T) {
	f := newRoutingFixture(t)
	threadConn := f.h.mgr.BotForSession(f.threadKey)
	dmConn := f.h.mgr.BotForSession(f.dmKey)
	dmID, err := dmConn.SendEditableInjected(f.dmKey, "dm message")
	if err != nil {
		t.Fatal(err)
	}

	mark := f.h.fake.totalCalls()
	warns := captureLogLines(t, log.WARN)
	if err := threadConn.SendToSession(f.dmKey, "leak via SendToSession"); err == nil {
		t.Error("SendToSession(foreign key) on a bound view returned nil")
	}
	if err := threadConn.SendInjectedMessage(f.dmKey, "leak via SendInjectedMessage"); err == nil {
		t.Error("SendInjectedMessage(foreign key) on a bound view returned nil")
	}
	if id, err := threadConn.SendEditableInjected(f.dmKey, "leak via SendEditableInjected"); err == nil {
		t.Errorf("SendEditableInjected(foreign key) on a bound view returned id %q, nil", id)
	}
	if err := threadConn.EditInjectedMessage(f.dmKey, dmID, "hijacked"); err == nil {
		t.Error("EditInjectedMessage(foreign key) on a bound view returned nil")
	}
	if got := f.h.fake.totalCalls(); got != mark {
		t.Errorf("refused foreign-key calls made %d API calls, want 0: %+v", got-mark, f.h.fake.callsSnapshot()[mark:])
	}
	n := 0
	for _, l := range warns() {
		if strings.Contains(l, f.threadKey) && strings.Contains(l, f.dmKey) {
			n++
		}
	}
	if n < 4 {
		t.Errorf("refusal WARNs naming the bound session = %d, want one per refused call (4)", n)
	}

	// Same conversation, different key shape: a branch of the thread
	// session lands in the thread, as BotForSession(branch) would send it.
	if err := threadConn.SendToSession(f.threadKey+"/b1700000001", "from a branch"); err != nil {
		t.Errorf("branch of the view's own session refused: %v", err)
	}
	// The unbound connection is not pinned.
	if err := f.h.mgr.PrimaryBot("helen").SendToSession(f.dmKey, "unbound to dm"); err != nil {
		t.Errorf("unbound SendToSession(dm) failed: %v", err)
	}
	if posts := f.h.fake.postsTo(f.dm.Channel); len(posts) != 2 {
		t.Errorf("posts to the DM = %d, want 2 (the editable seed + the unbound send)", len(posts))
	}
}

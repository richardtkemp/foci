package relogin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// The #3011 rules: one attempt's sign-in link goes to one chat; a user message
// in another chat gets the same link repeated there, and the code from either
// chat completes the login.

// chatTarget is a fakeChat target for one session.
func chatTarget(f *fakeChat, sessionKey string) *LinkTarget {
	t := f.target("app|x|" + sessionKey)
	t.SessionKey = sessionKey
	return t
}

// TestOfferLink_RepeatsSameLinkThenCapturesCode: a message in another session
// of the same agent, and in another agent's chat, each get the same link once;
// the next message there is the code, and the chat the code came from hears
// how the login ended.
func TestOfferLink_RepeatsSameLinkThenCapturesCode(t *testing.T) {
	g := &Gate{}
	if !g.Start() {
		t.Fatal("Start")
	}
	defer g.Release()
	g.OpenCapture("clutch", "app/a", "LINK")

	if g.ShouldCapture("clutch", "app/b") || g.ShouldCapture("scout", "app/c") {
		t.Fatal("a chat that never saw the link must not capture the code")
	}
	if !g.ShouldCapture("clutch", "app/a") {
		t.Fatal("the link's own chat must capture the code")
	}
	if g.OfferLink("clutch", "app/a", func(string) *LinkTarget { t.Fatal("repeat built for the link's own chat"); return nil }) {
		t.Fatal("the link's own chat must not get the link repeated")
	}

	chatB, chatC := newFakeChat(), newFakeChat()
	var asked []string
	targetFor := func(f *fakeChat) func(string) *LinkTarget {
		return func(sk string) *LinkTarget { asked = append(asked, sk); return chatTarget(f, sk) }
	}
	if !g.OfferLink("clutch", "app/b", targetFor(chatB)) {
		t.Fatal("another session of the triggering agent must get the link")
	}
	if !g.OfferLink("scout", "app/c", targetFor(chatC)) {
		t.Fatal("another agent's chat must get the link")
	}
	if g.OfferLink("scout", "app/c", targetFor(chatC)) {
		t.Fatal("a chat that has the link must not get it again")
	}
	if chatB.sends != 1 || chatB.text("m1") != "LINK" || chatC.sends != 1 || chatC.text("m1") != "LINK" {
		t.Fatalf("repeats: b=%d %q c=%d %q; want the same link once in each", chatB.sends, chatB.text("m1"), chatC.sends, chatC.text("m1"))
	}
	if len(asked) != 2 || asked[0] != "app/b" || asked[1] != "app/c" {
		t.Fatalf("targets built for %q, want app/b then app/c", asked)
	}

	if !g.CaptureCode("scout", "app/c", "the-code") {
		t.Fatal("the code from the second chat must be captured")
	}
	if code, ok := g.AwaitCode(time.Second); !ok || code != "the-code" {
		t.Fatalf("AwaitCode = %q,%v; want the-code,true", code, ok)
	}
	if g.ShouldCapture("clutch", "app/a") || g.CaptureCode("clutch", "app/b", "x") {
		t.Fatal("the capture window must close once a code is in")
	}
	if g.OfferLink("clutch", "app/d", targetFor(newFakeChat())) {
		t.Fatal("no repeat once a code is in")
	}
	if err := g.notifyCodeChat(completedText("")); err != nil {
		t.Fatal(err)
	}
	if chatC.sends != 2 || chatC.text("m2") != completedText("") || chatB.sends != 1 {
		t.Fatalf("outcome notice: c sends=%d %q, b sends=%d; want the notice in the code's chat only", chatC.sends, chatC.text("m2"), chatB.sends)
	}
}

// TestOfferLink_ClosedWindowNoRepeat: before the link exists (the attempt is
// starting) and on an inactive gate, a message gets no repeat.
func TestOfferLink_ClosedWindowNoRepeat(t *testing.T) {
	g := &Gate{}
	never := func(string) *LinkTarget { t.Fatal("repeat built with the window closed"); return nil }
	if g.OfferLink("scout", "app/c", never) {
		t.Fatal("inactive gate repeated a link")
	}
	g.Start()
	defer g.Release()
	if g.OfferLink("scout", "app/c", never) {
		t.Fatal("repeated a link before the attempt had one")
	}
	g.OpenCapture("clutch", "app/a", "LINK")
	if g.OfferLink("scout", "app/c", nil) {
		t.Fatal("an agent with no link target repeated the link")
	}
}

// TestOfferLink_UnknownFirstChat: when the first chat's session is not known,
// every chat of the triggering agent counts as it (the behaviour before
// #3011), and other agents still get the repeat.
func TestOfferLink_UnknownFirstChat(t *testing.T) {
	g := &Gate{}
	g.Start()
	defer g.Release()
	g.OpenCapture("clutch", "", "LINK")
	if !g.ShouldCapture("clutch", "app/any") {
		t.Fatal("unknown first chat: any chat of the agent must capture")
	}
	if g.OfferLink("clutch", "app/any", func(string) *LinkTarget { t.Fatal("repeat for the agent's own chat"); return nil }) {
		t.Fatal("unknown first chat: the agent's chats must not get a repeat")
	}
	chat := newFakeChat()
	if !g.OfferLink("scout", "app/c", func(sk string) *LinkTarget { return chatTarget(chat, sk) }) {
		t.Fatal("another agent's chat must get the link")
	}
}

// TestOfferLink_SendFailureLeavesChatWithoutLink: a failed repeat does not
// make the chat a code chat, so a later message there tries again.
func TestOfferLink_SendFailureLeavesChatWithoutLink(t *testing.T) {
	g := &Gate{}
	g.Start()
	defer g.Release()
	g.OpenCapture("clutch", "app/a", "LINK")
	failing := func(string) *LinkTarget {
		return &LinkTarget{Send: func(string) (string, error) { return "", errors.New("offline") }}
	}
	if g.OfferLink("scout", "app/c", failing) {
		t.Fatal("a failed send reported a repeat")
	}
	if g.ShouldCapture("scout", "app/c") {
		t.Fatal("a chat whose repeat failed must not capture the code")
	}
	chat := newFakeChat()
	if !g.OfferLink("scout", "app/c", func(sk string) *LinkTarget { return chatTarget(chat, sk) }) {
		t.Fatal("a later message must try the repeat again")
	}
}

// TestOfferLink_AttemptEndsDuringSend: a link repeated while the attempt
// ends is dead on arrival, so it is marked expired at once.
func TestOfferLink_AttemptEndsDuringSend(t *testing.T) {
	g := &Gate{}
	g.Start()
	g.OpenCapture("clutch", "app/a", "LINK")
	chat := newFakeChat()
	if g.OfferLink("scout", "app/c", func(sk string) *LinkTarget {
		tgt := chatTarget(chat, sk)
		send := tgt.Send
		tgt.Send = func(text string) (string, error) {
			g.Release()
			return send(text)
		}
		return tgt
	}) {
		t.Fatal("a repeat raced by the attempt's end reported success")
	}
	if got := chat.text("m1"); got != expiredLinkText {
		t.Fatalf("repeated link = %q, want it marked expired", got)
	}
}

// TestRun_CodeFromRepeatedChat drives the attempt: the link goes to one chat,
// a second chat gets it repeated, the code pasted there completes the login,
// and both chats hear it completed. A failed attempt marks the repeat
// expired too.
func TestRun_CodeFromRepeatedChat(t *testing.T) {
	g := &Gate{}
	if !g.Start() {
		t.Fatal("Start")
	}
	urlScreen := "Use the url below to sign in\nhttps://claude.ai/oauth?code=1&state=2\nPaste code here if prompted"
	fp := &fakePane{captures: []string{urlScreen, "Login successful!"}}
	var msgs []string
	var mu sync.Mutex
	cfg := newTestConfig(g, fp, &msgs, &mu)
	first, second := newFakeChat(), newFakeChat()
	cfg.Link = chatTarget(first, "app/a")

	done := make(chan struct{})
	go func() { Run(context.Background(), cfg); close(done) }()
	waitUntil(t, func() bool { return g.ShouldCapture("clutch", "app/a") })
	if !g.OfferLink("scout", "app/c", func(sk string) *LinkTarget { return chatTarget(second, sk) }) {
		t.Fatal("second chat did not get the link")
	}
	if first.sends != 1 || second.text("m1") != first.text("m1") {
		t.Fatalf("second chat got %q, first %q (sends=%d); want the same link, sent to the first chat once", second.text("m1"), first.text("m1"), first.sends)
	}
	if !g.CaptureCode("scout", "app/c", "logincode") {
		t.Fatal("code from the second chat not captured")
	}
	<-done
	if lines := fp.sentLines(); lines[len(lines)-1] != "logincode" {
		t.Fatalf("pane lines = %q, want the second chat's code submitted", lines)
	}
	if g.Pending() {
		t.Fatal("login still pending after the second chat's code completed it")
	}
	if second.text("m2") != completedText("") {
		t.Fatalf("second chat's last message = %q, want the completed notice", second.text("m2"))
	}
	mu.Lock()
	if len(msgs) == 0 || msgs[len(msgs)-1] != completedText("") {
		t.Fatalf("first chat notices = %q, want the completed notice", msgs)
	}
	mu.Unlock()

	// A failed attempt (the login never confirms) marks the repeated link
	// expired as well.
	if !g.Start() {
		t.Fatal("Start")
	}
	fp2 := &fakePane{captures: []string{urlScreen}}
	cfg2 := newTestConfig(g, fp2, &msgs, &mu)
	cfg2.CodeTimeout = time.Minute
	cfg2.Link = chatTarget(first, "app/a")
	third := newFakeChat()
	done2 := make(chan struct{})
	go func() { Run(context.Background(), cfg2); close(done2) }()
	waitUntil(t, func() bool { return g.ShouldCapture("clutch", "app/a") })
	if !g.OfferLink("scout", "app/c", func(sk string) *LinkTarget { return chatTarget(third, sk) }) {
		t.Fatal("second attempt: no repeat")
	}
	if !g.CaptureCode("clutch", "app/a", "bad-code") {
		t.Fatal("second attempt: code from the first chat not captured")
	}
	<-done2
	if got := third.text("m1"); got != expiredLinkText {
		t.Fatalf("repeated link after a failed attempt = %q, want expired", got)
	}
}

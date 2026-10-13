package relogin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeChat records the messages a LinkTarget sends and edits, by id.
type fakeChat struct {
	mu    sync.Mutex
	next  int
	msgs  map[string]string
	order []string // ids in send order
	sends int
	edits int
	hist  map[string][]string // every text each id has held
}

func newFakeChat() *fakeChat {
	return &fakeChat{msgs: map[string]string{}, hist: map[string][]string{}}
}

func (f *fakeChat) target(key string) *LinkTarget {
	return &LinkTarget{
		Key: key,
		Send: func(text string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.next++
			id := fmt.Sprintf("m%d", f.next)
			f.msgs[id] = text
			f.hist[id] = append(f.hist[id], text)
			f.order = append(f.order, id)
			f.sends++
			return id, nil
		},
		Edit: func(id, text string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if _, ok := f.msgs[id]; !ok {
				return fmt.Errorf("no message %s", id)
			}
			f.msgs[id] = text
			f.hist[id] = append(f.hist[id], text)
			f.edits++
			return nil
		},
	}
}

func (f *fakeChat) text(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.msgs[id]
}

// TestShowLink_EditsInPlaceWithinOnePendingLogin (#2384): the second attempt's
// link rewrites the first message, a failed attempt marks it expired, and a
// completed login forgets it so the next login sends a fresh message.
func TestShowLink_EditsInPlaceWithinOnePendingLogin(t *testing.T) {
	g := &Gate{}
	chat := newFakeChat()
	tgt := chat.target("app|clutch|s")

	g.Start()
	if err := g.showLink(tgt, "link-1"); err != nil {
		t.Fatal(err)
	}
	if err := g.expireLink(); err != nil {
		t.Fatal(err)
	}
	if got := chat.text("m1"); got != expiredLinkText {
		t.Fatalf("after a failed attempt m1 = %q, want the expired text", got)
	}
	g.Release()

	g.Start()
	if err := g.showLink(tgt, "link-2"); err != nil {
		t.Fatal(err)
	}
	if chat.sends != 1 || chat.text("m1") != "link-2" {
		t.Fatalf("second attempt: sends=%d m1=%q; want 1 send and m1 rewritten to link-2", chat.sends, chat.text("m1"))
	}
	g.Complete()
	g.Release()

	g.Start()
	defer g.Release()
	if err := g.showLink(tgt, "link-3"); err != nil {
		t.Fatal(err)
	}
	if chat.sends != 2 || chat.text("m2") != "link-3" || chat.text("m1") != "link-2" {
		t.Fatalf("after a completed login: sends=%d m1=%q m2=%q; want a fresh m2 and m1 untouched",
			chat.sends, chat.text("m1"), chat.text("m2"))
	}
}

// TestShowLink_OtherChatExpiresOld: a link for a different chat cannot replace
// the old message in place, so the old one is marked expired and a new one sent.
func TestShowLink_OtherChatExpiresOld(t *testing.T) {
	g := &Gate{}
	chat := newFakeChat()
	g.Start()
	defer g.Release()
	_ = g.showLink(chat.target("app|clutch|a"), "link-a")
	_ = g.showLink(chat.target("telegram|clutch|b"), "link-b")
	if chat.text("m1") != expiredLinkText || chat.text("m2") != "link-b" {
		t.Fatalf("m1=%q m2=%q; want m1 expired and m2 the new link", chat.text("m1"), chat.text("m2"))
	}
}

// TestShowLink_EditFailureSendsNew: if the old message can no longer be edited
// (deleted, too old), the new link still reaches the user as a new message.
func TestShowLink_EditFailureSendsNew(t *testing.T) {
	g := &Gate{}
	chat := newFakeChat()
	tgt := chat.target("k")
	g.Start()
	defer g.Release()
	_ = g.showLink(tgt, "link-1")
	chat.mu.Lock()
	delete(chat.msgs, "m1")
	chat.mu.Unlock()
	if err := g.showLink(tgt, "link-2"); err != nil {
		t.Fatal(err)
	}
	if chat.text("m2") != "link-2" {
		t.Fatalf("m2=%q, want the new link sent as a new message", chat.text("m2"))
	}
}

// TestRun_SecondAttemptEditsLinkInPlace drives the real driver twice: the
// first attempt times out (its link becomes the expired text), the second
// rewrites the same message with its new link.
func TestRun_SecondAttemptEditsLinkInPlace(t *testing.T) {
	g := &Gate{}
	chat := newFakeChat()
	run := func(url string) {
		if !g.Start() {
			t.Fatal("Start")
		}
		screen := "Use the url below to sign in\n" + url + "\nPaste code here if prompted"
		fp := &fakePane{captures: []string{screen}}
		var msgs []string
		var mu sync.Mutex
		cfg := newTestConfig(g, fp, &msgs, &mu)
		cfg.CodeTimeout = 20 * time.Millisecond
		cfg.Link = chat.target("app|clutch|s")
		Run(context.Background(), cfg)
	}
	run("https://claude.ai/oauth?state=one")
	if got := chat.text("m1"); got != expiredLinkText {
		t.Fatalf("after the timed-out attempt m1 = %q, want the expired text", got)
	}
	run("https://claude.ai/oauth?state=two")
	if chat.sends != 1 {
		t.Fatalf("sends = %d, want 1: the second link must edit the first message", chat.sends)
	}
	// m1 held: link one, expired, link two (edited in place), expired again.
	h := chat.hist["m1"]
	if len(h) != 4 || !strings.Contains(h[0], "state=one") || h[1] != expiredLinkText ||
		!strings.Contains(h[2], "state=two") || h[3] != expiredLinkText {
		t.Fatalf("m1 history = %q; want link one, expired, link two, expired", h)
	}
}

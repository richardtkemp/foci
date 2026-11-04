package codex

import (
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
)

// facadeCapture records everything one facade would show in its own chat:
// ordinary text and approval prompts.
type facadeCapture struct {
	mu       sync.Mutex
	chatText []string
	deltas   []string
	prompts  []string
	promptTx []string
}

func (c *facadeCapture) attach(b *Backend) {
	b.AttachSessionEvents(&delegator.SessionEvents{
		OnText:      func(s string) { c.mu.Lock(); c.chatText = append(c.chatText, s); c.mu.Unlock() },
		OnTextDelta: func(s string) { c.mu.Lock(); c.deltas = append(c.deltas, s); c.mu.Unlock() },
	})
	b.permPromptFn = func(itemID, text, _, _ string, _ []delegator.PromptChoice) {
		c.mu.Lock()
		c.prompts = append(c.prompts, itemID)
		c.promptTx = append(c.promptTx, text)
		c.mu.Unlock()
	}
}

// newSharedPair builds two facades on ONE in-memory app-server: owner launched
// it (its reader calls owner.dispatch for every line), peer attached to it.
// wire captures what foci writes back to the app-server.
func newSharedPair(t *testing.T) (owner, peer *Backend, oc, pc *facadeCapture, wire *captureCloser) {
	t.Helper()
	mk := func(key string) *Backend {
		return &Backend{
			cfg:          map[string]any{},
			lg:           log.NewComponentLogger("codex"),
			pendingRPC:   make(map[int64]chan rpcReply),
			pendingPerms: make(map[int64]*pendingApproval),
			startOpts:    delegator.StartOptions{SessionKey: key},
		}
	}
	owner = mk("codex/owner")
	wire = &captureCloser{}
	owner.writer = NewWriter(wire)
	peer = mk("codex/peer")
	peer.shared = owner
	setTestThread(owner, "thread-owner")
	setTestThread(peer, "thread-peer")
	oc, pc = &facadeCapture{}, &facadeCapture{}
	oc.attach(owner)
	pc.attach(peer)
	return owner, peer, oc, pc, wire
}

func captureWarns(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var warns []string
	log.SetWarnHook(func(level log.Level, _ string, msg string) {
		if level.String() != "WARN" {
			return
		}
		mu.Lock()
		warns = append(warns, msg)
		mu.Unlock()
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), warns...)
	}
}

// TestUnknownThreadGoesToOwnerAsLost pins ruling 2: a line for a thread that
// is no session's and no known subagent's is still handed to the owner (so a
// prompt is not stranded), with a WARN naming the method and thread. A closed
// parent's children count as unknown. The WARN is deferred until the thread
// has stayed unclaimed for unclaimedGrace (#2700). thread/started is exempt
// from the WARN: a facade's own thread/started can precede the response that
// registers it.
func TestUnknownThreadGoesToOwnerAsLost(t *testing.T) {
	owner, _, oc, _, _ := newSharedPair(t)
	stopUnclaimedTimer(t, owner)
	warns := captureWarns(t)

	owner.dispatch([]byte(`{"id":80,"method":"item/commandExecution/requestApproval","params":{"threadId":"stray-thread","turnId":"t","itemId":"cmd-4","command":"ls"}}`))
	if len(oc.prompts) != 1 || oc.prompts[0] != "cmd-4" {
		t.Errorf("owner prompts = %v, want the stray approval handed to the owner", oc.prompts)
	}
	// The WARN waits out the grace (#2700); a thread still unclaimed after it
	// is reported.
	owner.flushUnclaimed(time.Now().Add(unclaimedGrace+time.Second), false)
	var lost string
	for _, w := range warns() {
		if strings.Contains(w, "lost message") {
			lost = w
		}
	}
	if !strings.Contains(lost, "stray-thread") || !strings.Contains(lost, "item/commandExecution/requestApproval") {
		t.Errorf("lost WARN = %q, want it to name the thread and the method", lost)
	}

	owner.flushUnclaimed(time.Now().Add(unclaimedGrace+time.Second), false)
	before := len(warns())
	owner.dispatch([]byte(`{"method":"thread/started","params":{"thread":{"id":"fresh-thread"}}}`))
	owner.flushUnclaimed(time.Now().Add(unclaimedGrace+time.Second), false)
	if after := warns(); len(after) != before {
		t.Errorf("thread/started for an unregistered thread WARNed: %v", after[before:])
	}
}

// TestLostLineIsMarkedInOwnersChat pins Dick's ruling on lost lines: whatever
// the owner shows from a thread that matches no session — an
// approval prompt, a streamed message (marked once, on its first delta) and the
// completed message — leads with the shared redirected-origin marker naming
// the thread, while the owner's own text and prompts stay unmarked.
func TestLostLineIsMarkedInOwnersChat(t *testing.T) {
	owner, _, oc, _, _ := newSharedPair(t)
	marker := delegator.RedirectedOrigin("codex thread stray-thread", "it matches no session, so it is shown here")

	owner.dispatch([]byte(`{"id":90,"method":"item/commandExecution/requestApproval","params":{"threadId":"stray-thread","turnId":"t","itemId":"cmd-9","command":"ls"}}`))
	owner.dispatch([]byte(`{"method":"item/agentMessage/delta","params":{"threadId":"stray-thread","turnId":"t","delta":"Hel"}}`))
	owner.dispatch([]byte(`{"method":"item/agentMessage/delta","params":{"threadId":"stray-thread","turnId":"t","delta":"lo"}}`))
	owner.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"stray-thread","turnId":"t","item":{"type":"agentMessage","id":"m1","text":"Hello"}}}`))
	// The owner's own output after the lost line must not inherit the marker.
	owner.dispatch([]byte(`{"method":"item/agentMessage/delta","params":{"threadId":"thread-owner","turnId":"to","delta":"Mine"}}`))
	owner.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"thread-owner","turnId":"to","item":{"type":"agentMessage","id":"m2","text":"Mine"}}}`))
	owner.dispatch([]byte(`{"id":91,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread-owner","turnId":"to","itemId":"cmd-10","command":"pwd"}}`))

	oc.mu.Lock()
	defer oc.mu.Unlock()
	wantPrompts := []string{marker + "\n\nApprove command: ls", "Approve command: pwd"}
	wantDeltas := []string{marker + "\n\nHel", "lo", "Mine"}
	wantText := []string{marker + "\n\nHello", "Mine"}
	check := func(name string, got, want []string) {
		if len(got) != len(want) {
			t.Errorf("%s = %q, want %q", name, got, want)
			return
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s[%d] = %q, want %q", name, i, got[i], want[i])
			}
		}
	}
	check("prompts", oc.promptTx, wantPrompts)
	check("deltas", oc.deltas, wantDeltas)
	check("chat text", oc.chatText, wantText)
	if !strings.HasPrefix(marker, "↪️ **From codex thread stray-thread**") {
		t.Errorf("marker = %q, want the shared ↪️ From family", marker)
	}
}

// TestLostTurnCompletedLeavesOwnersTurnOpen: a lost thread's turn lifecycle
// must not touch the owner's turn. Mid-turn, the owner gets a lost
// turn/started, agent message and turn/completed; its turn must still be
// open, the lost message must not join its turn text, and its own later
// turn/completed must end it with only its own text. Before dispatchLost the
// lost turn/completed ended the owner's turn with the lost thread's text.
func TestLostTurnCompletedLeavesOwnersTurnOpen(t *testing.T) {
	owner, _, oc, _, _ := newSharedPair(t)
	got := openUsageTurn(owner)

	owner.dispatch([]byte(`{"method":"turn/started","params":{"threadId":"stray-thread","turn":{"id":"ts"}}}`))
	owner.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"stray-thread","turnId":"ts","item":{"type":"agentMessage","id":"m1","text":"Theirs"}}}`))
	owner.dispatch([]byte(`{"method":"turn/completed","params":{"threadId":"stray-thread","turn":{"id":"ts","status":"completed"}}}`))

	owner.turnMu.Lock()
	active := owner.turnActive
	owner.turnMu.Unlock()
	if *got != nil || !active {
		t.Fatalf("lost turn/completed ended the owner's turn (result=%+v, active=%v)", *got, active)
	}

	owner.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"thread-owner","turnId":"to","item":{"type":"agentMessage","id":"m2","text":"Mine"}}}`))
	owner.dispatch([]byte(`{"method":"turn/completed","params":{"threadId":"thread-owner","turn":{"id":"to","status":"completed"}}}`))
	if *got == nil {
		t.Fatal("the owner's own turn/completed did not end its turn")
	}
	if (*got).Text != "Mine" {
		t.Errorf("owner's turn text = %q, want %q (a lost message must not join it)", (*got).Text, "Mine")
	}
	// The lost message was still shown, marked.
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if len(oc.chatText) == 0 || !strings.HasPrefix(oc.chatText[0], "↪️ **From codex thread stray-thread**") || !strings.HasSuffix(oc.chatText[0], "Theirs") {
		t.Errorf("chat text = %q, want the lost message first, marked", oc.chatText)
	}
}

// TestLostTokenUsageNotBookedToOwner: a lost thread's API cycle must not be
// counted as the owner's turn usage. The same usage line on the owner's own
// thread is the positive control: it IS booked, so a nil Usage means the lost
// line was skipped, not that usage is never recorded here.
func TestLostTokenUsageNotBookedToOwner(t *testing.T) {
	owner, _, _, _, _ := newSharedPair(t)
	lost := strings.Replace(usageCycle1, `"threadId":"th_1"`, `"threadId":"stray-thread"`, 1)
	own := strings.Replace(usageCycle1, `"threadId":"th_1"`, `"threadId":"thread-owner"`, 1)
	end := `{"method":"turn/completed","params":{"threadId":"thread-owner","turn":{"id":"to","status":"completed"}}}`

	got := openUsageTurn(owner)
	owner.dispatch([]byte(lost))
	owner.dispatch([]byte(end))
	if *got == nil {
		t.Fatal("owner's turn did not complete")
	}
	if (*got).Usage != nil {
		t.Errorf("lost token usage booked to the owner's turn: %+v", *(*got).Usage)
	}

	got = openUsageTurn(owner)
	owner.dispatch([]byte(own))
	owner.dispatch([]byte(end))
	if *got == nil || (*got).Usage == nil {
		t.Fatal("control: the owner's own usage was not recorded, so this test cannot see a leak")
	}
}

// TestLostFailedTurnIsANoteNotTheOwnersError: a lost thread's failed turn is
// shown as a marked note, but it neither ends the owner's turn nor sets the
// owner's last-turn error.
func TestLostFailedTurnIsANoteNotTheOwnersError(t *testing.T) {
	owner, _, oc, _, _ := newSharedPair(t)
	got := openUsageTurn(owner)
	owner.dispatch([]byte(`{"method":"turn/completed","params":{"threadId":"stray-thread","turn":{"id":"ts","status":"failed","error":{"message":"out of credits"}}}}`))

	if *got != nil {
		t.Fatalf("lost failed turn ended the owner's turn: %+v", *got)
	}
	owner.mu.Lock()
	lastErr := owner.lastTurnError
	owner.mu.Unlock()
	if lastErr != "" {
		t.Errorf("owner's lastTurnError = %q, want empty", lastErr)
	}
	want := delegator.RedirectedOrigin("codex thread stray-thread", "it matches no session, so it is shown here") +
		"\n\n" + turnFailedText("out of credits")
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if len(oc.chatText) != 1 || oc.chatText[0] != want {
		t.Errorf("chat text = %q, want [%q]", oc.chatText, want)
	}
}

// TestUnflaggedCodexChildStaysOutOfTheOwnersTurn is the guard if codex ever stops
// honouring agents.enabled=false, or starts streaming a guardian thread. A child
// announces its parent with thread/started and then streams its whole lifecycle.
// foci has no child routing any more, so the child is a lost thread even though
// its parent is a live session: the owner's turn stays open and its text and
// usage stay the owner's, the child's message is shown only marked, and the
// thread is reported once.
func TestUnflaggedCodexChildStaysOutOfTheOwnersTurn(t *testing.T) {
	owner, _, oc, pc, _ := newSharedPair(t)
	stopUnclaimedTimer(t, owner)
	warns := captureWarns(t)
	got := openUsageTurn(owner)

	for _, parent := range []string{"thread-owner", "thread-peer"} {
		child := "child-of-" + parent
		lines := []string{
			`{"method":"thread/started","params":{"thread":{"id":"` + child + `","parentThreadId":"` + parent + `","source":{"subAgent":{"other":"guardian"}}}}}`,
			`{"method":"turn/started","params":{"threadId":"` + child + `","turn":{"id":"tc","status":"inProgress"}}}`,
			`{"method":"item/agentMessage/delta","params":{"threadId":"` + child + `","turnId":"tc","itemId":"m1","delta":"CHILD-DELTA"}}`,
			`{"method":"item/completed","params":{"threadId":"` + child + `","turnId":"tc","item":{"type":"agentMessage","id":"m1","text":"CHILD-TEXT"}}}`,
			strings.Replace(usageCycle1, `"threadId":"th_1"`, `"threadId":"`+child+`"`, 1),
			`{"method":"turn/completed","params":{"threadId":"` + child + `","turn":{"id":"tc","status":"completed"}}}`,
		}
		for _, l := range lines {
			owner.dispatch([]byte(l))
		}
	}

	if *got != nil {
		t.Fatalf("a child's turn/completed ended the owner's turn: %+v", **got)
	}
	owner.turnMu.Lock()
	turnText, active := owner.turnText.String(), owner.turnActive
	owner.turnMu.Unlock()
	if !active || strings.Contains(turnText, "CHILD") {
		t.Errorf("owner's turn: active=%v text=%q, want active with none of the child's text", active, turnText)
	}
	for _, s := range oc.chatText {
		if strings.Contains(s, "CHILD-TEXT") && !strings.Contains(s, "child-of-") {
			t.Errorf("child text shown unmarked in the owner's chat: %q", s)
		}
	}
	if len(oc.chatText) != 2 {
		t.Errorf("owner chat = %q, want each child's message once, marked (the lost-line guard)", oc.chatText)
	}
	if len(pc.chatText)+len(pc.deltas) != 0 {
		t.Errorf("a non-owner parent received a child's lines: %q %q", pc.chatText, pc.deltas)
	}

	owner.flushUnclaimed(time.Now().Add(unclaimedGrace+time.Second), false)
	if lost := lostWarns(warns()); len(lost) != 2 {
		t.Errorf("lost WARNs = %q, want one per child thread", lost)
	}

	end := `{"method":"turn/completed","params":{"threadId":"thread-owner","turn":{"id":"to","status":"completed"}}}`
	owner.dispatch([]byte(end))
	if *got == nil {
		t.Fatal("owner's turn did not complete")
	}
	if (*got).Usage != nil {
		t.Errorf("a child's usage was booked to the owner's turn: %+v", *(*got).Usage)
	}
}

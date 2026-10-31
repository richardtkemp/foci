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
// subagent-panel events, ordinary text, and approval prompts.
type facadeCapture struct {
	mu       sync.Mutex
	starts   []string
	texts    []string
	ends     int
	chatText []string
	deltas   []string
	prompts  []string
	promptTx []string
}

func (c *facadeCapture) attach(b *Backend) {
	b.AttachSessionEvents(&delegator.SessionEvents{
		OnSubagentStart: func(g, _, _ string, _ int) { c.mu.Lock(); c.starts = append(c.starts, g); c.mu.Unlock() },
		OnSubagentText:  func(_, text string, _ int) { c.mu.Lock(); c.texts = append(c.texts, text); c.mu.Unlock() },
		OnSubagentEnd:   func(string, int) { c.mu.Lock(); c.ends++; c.mu.Unlock() },
		OnText:          func(s string) { c.mu.Lock(); c.chatText = append(c.chatText, s); c.mu.Unlock() },
		OnTextDelta:     func(s string) { c.mu.Lock(); c.deltas = append(c.deltas, s); c.mu.Unlock() },
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
			subagents:    newSubagentTracker(),
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

// TestNonOwnerSubagentReachesItsParentNotTheOwner is the regression for the
// 2026-10-29 leak: a session that did NOT launch the shared app-server spawns
// a subagent, and the child's final message and approval prompt showed up in
// the OWNER's chat. Every line is fed through owner.dispatch, as the owner's
// reader does. The child's text, its approval prompt and its run end must reach
// the peer, the owner must see none of it, and answering the prompt from the
// peer must write codex's own request id back to the shared connection.
func TestNonOwnerSubagentReachesItsParentNotTheOwner(t *testing.T) {
	owner, peer, oc, pc, wire := newSharedPair(t)
	warns := captureWarns(t)

	// The peer's own thread reports the spawn.
	owner.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"thread-peer","turnId":"tp","item":` + string(activityItem("call_spawn", "started", "child-1")) + `}}`))
	// The child's own traffic, all tagged with the child's thread id.
	owner.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"child-1","turnId":"tc","item":{"type":"agentMessage","id":"m1","text":"Implemented and committed 859dc1de","phase":"final_answer"}}}`))
	owner.dispatch([]byte(`{"id":77,"method":"item/commandExecution/requestApproval","params":{"threadId":"child-1","turnId":"tc","itemId":"cmd-1","command":"git add x"}}`))
	owner.dispatch([]byte(`{"method":"turn/completed","params":{"threadId":"child-1","turn":{"id":"tc","status":"completed"}}}`))

	if len(pc.starts) != 1 || len(pc.texts) != 1 || pc.texts[0] != "Implemented and committed 859dc1de" || pc.ends != 1 {
		t.Errorf("peer subagent panel: starts=%v texts=%q ends=%d, want 1 start, the child's message, 1 end", pc.starts, pc.texts, pc.ends)
	}
	if len(pc.prompts) != 1 || pc.prompts[0] != "cmd-1" {
		t.Errorf("peer prompts = %v, want the child's approval cmd-1 in the PARENT's chat", pc.prompts)
	}
	if len(oc.starts)+len(oc.texts)+oc.ends+len(oc.chatText)+len(oc.prompts) != 0 {
		t.Errorf("owner saw the peer's child: starts=%v texts=%q ends=%d chat=%q prompts=%v", oc.starts, oc.texts, oc.ends, oc.chatText, oc.prompts)
	}
	if len(owner.pendingPerms) != 0 {
		t.Errorf("owner holds a pending approval for the peer's child: %+v", owner.pendingPerms)
	}

	if err := peer.RespondToPermission("cmd-1", delegator.PermissionDecision{Allow: true}); err != nil {
		t.Fatalf("peer could not answer its child's approval: %v", err)
	}
	if got := wire.String(); !strings.Contains(got, `"id":77`) || !strings.Contains(got, `"accept"`) {
		t.Errorf("response on the shared connection = %q, want id 77 with decision accept", got)
	}
	for _, w := range warns() {
		if strings.Contains(w, "lost message") {
			t.Errorf("a routed child line was reported lost: %s", w)
		}
	}
}

// TestChildBoundFromThreadStartedParent covers the other arrival order: the
// child's thread/started (which carries parentThreadId for a subagent) comes
// before the parent's subAgentActivity item. The child must already route to
// the peer, so its approval prompt is not handed to the owner.
func TestChildBoundFromThreadStartedParent(t *testing.T) {
	owner, _, oc, pc, _ := newSharedPair(t)

	owner.dispatch([]byte(`{"method":"thread/started","params":{"thread":{"id":"child-2","parentThreadId":"thread-peer"}}}`))
	owner.dispatch([]byte(`{"id":78,"method":"item/commandExecution/requestApproval","params":{"threadId":"child-2","turnId":"tc","itemId":"cmd-2","command":"ls"}}`))

	if len(pc.prompts) != 1 || pc.prompts[0] != "cmd-2" {
		t.Errorf("peer prompts = %v, want cmd-2", pc.prompts)
	}
	if len(oc.prompts) != 0 {
		t.Errorf("owner prompted for the peer's child: %v", oc.prompts)
	}
}

// TestOwnersOwnChildApprovalIsPrompted: a child of the OWNER's session used to
// have its approval request consumed as a subagent notification (the old
// isChild path took every method), so nobody was asked and the child waited
// forever. It must now prompt in the owner's chat.
func TestOwnersOwnChildApprovalIsPrompted(t *testing.T) {
	owner, _, oc, pc, _ := newSharedPair(t)

	owner.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"thread-owner","turnId":"to","item":` + string(activityItem("call_spawn", "started", "child-3")) + `}}`))
	owner.dispatch([]byte(`{"id":79,"method":"item/commandExecution/requestApproval","params":{"threadId":"child-3","turnId":"tc","itemId":"cmd-3","command":"ls"}}`))

	if len(oc.prompts) != 1 || oc.prompts[0] != "cmd-3" {
		t.Errorf("owner prompts = %v, want cmd-3", oc.prompts)
	}
	if len(pc.prompts) != 0 {
		t.Errorf("peer prompted for the owner's child: %v", pc.prompts)
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
	owner, peer, oc, pc, _ := newSharedPair(t)
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

	// A closed parent's child no longer resolves to it.
	owner.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"thread-peer","turnId":"tp","item":` + string(activityItem("call_spawn", "started", "child-5")) + `}}`))
	owner.unregisterChildrenOf(peer)
	owner.dispatch([]byte(`{"id":81,"method":"item/commandExecution/requestApproval","params":{"threadId":"child-5","turnId":"tc","itemId":"cmd-5","command":"ls"}}`))
	if len(pc.prompts) != 0 {
		t.Errorf("closed peer still received its child's prompt: %v", pc.prompts)
	}
	if len(oc.prompts) != 2 {
		t.Errorf("owner prompts = %v, want the orphaned child's approval handed to the owner", oc.prompts)
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
// the owner shows from a thread that matches no session and no subagent — an
// approval prompt, a streamed message (marked once, on its first delta) and the
// completed message — leads with the shared redirected-origin marker naming
// the thread, while the owner's own text and prompts stay unmarked.
func TestLostLineIsMarkedInOwnersChat(t *testing.T) {
	owner, _, oc, _, _ := newSharedPair(t)
	marker := delegator.RedirectedOrigin("codex thread stray-thread", "it matches no session or subagent, so it is shown here")

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
	want := delegator.RedirectedOrigin("codex thread stray-thread", "it matches no session or subagent, so it is shown here") +
		"\n\n" + turnFailedText("out of credits")
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if len(oc.chatText) != 1 || oc.chatText[0] != want {
		t.Errorf("chat text = %q, want [%q]", oc.chatText, want)
	}
}

package tools

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/question"
	"foci/internal/session"
)

// ---------------------------------------------------------------------------
// #1711 — two concurrent asks in ONE session.
//
// Before this suite nothing anywhere covered a second foci_ask arriving while one
// was already pending, which is how "latest ask wins" (bySession holding a single
// requestID) survived: the superseded ask stayed answerable by button but lost
// typed-answer routing, /pause, /resume, /complete and the {ask} statusline field.
// ---------------------------------------------------------------------------

// Two single-question asks whose questions are OPTION-LESS (typed-answer only), so
// a test can resolve a named one through the typed-answer path and identify which
// ask's batch was delivered from the question text alone.
const (
	askOlderInput = `{"questions":[{"question":"OLDER-Q","header":"Older"}]}`
	askNewerInput = `{"questions":[{"question":"NEWER-Q","header":"Newer"}]}`
)

// askAck posts an ask and returns its request id and the ack status ("asked" when
// it went on screen, "queued" when it is waiting behind the session's primary).
func askAck(t *testing.T, tool *Tool, raw string) (string, string) {
	t.Helper()
	res := execAsk(t, tool, raw)
	var ack struct {
		RequestID string `json:"request_id"`
		Status    string `json:"status"`
		Note      string `json:"note"`
	}
	if err := json.Unmarshal([]byte(res.Text), &ack); err != nil {
		t.Fatalf("unmarshal ack %q: %v", res.Text, err)
	}
	if ack.RequestID == "" {
		t.Fatalf("ack carried no request_id: %q", res.Text)
	}
	// The note is the only thing the asking agent reads; it must not claim a
	// queued ask was posted.
	if (ack.Status == "queued") != strings.Contains(ack.Note, "NOT posted yet") {
		t.Fatalf("ack status %q disagrees with its note %q", ack.Status, ack.Note)
	}
	return ack.RequestID, ack.Status
}

// askReqID posts an ask and returns the request id from its async ack.
func askReqID(t *testing.T, tool *Tool, raw string) string {
	t.Helper()
	id, _ := askAck(t, tool, raw)
	return id
}

// loadPersisted decodes the durable ask_pending set for agent "test".
func loadPersisted(t *testing.T, idx *session.SessionIndex) []persistedAsk {
	t.Helper()
	raw, err := idx.GetAgentMetadata("test", askMetaKey)
	if err != nil {
		t.Fatalf("read %s: %v", askMetaKey, err)
	}
	if raw == "" {
		return nil
	}
	var saved []persistedAsk
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatalf("unmarshal %s (%q): %v", askMetaKey, raw, err)
	}
	return saved
}

// findPersisted returns the persisted entry for reqID (nil if absent).
func findPersisted(saved []persistedAsk, reqID string) *persistedAsk {
	for i := range saved {
		if saved[i].RequestID == reqID {
			return &saved[i]
		}
	}
	return nil
}

// T1 — a second ask in a session must NOT orphan the first. On a chat transport
// (no batched app form) the second ask QUEUES: it is not presented, and the OLDER
// ask stays the session's primary for every session-keyed path — typed answers,
// /pause, and the {ask} statusline field all read PendingForSession. When the
// primary resolves, the queued ask is promoted and presented.
func TestAsk_SecondAskQueuesAndOlderStaysRoutable(t *testing.T) {
	t.Parallel()
	idx := newStateDB(t)
	p := &fakePresenter{}
	d := &fakeDeliver{}
	tool, router := NewAskTool(p.present, nil, d.deliver, nil, idx, "test")

	older, olderStatus := askAck(t, tool, askOlderInput)
	newer, newerStatus := askAck(t, tool, askNewerInput)
	if older == newer {
		t.Fatalf("both asks share a request id %q", older)
	}
	// Ruling Q2: a queued ask returns immediately WITH the ask queued — the agent
	// must not be told it was posted to a user who cannot see it.
	if olderStatus != "asked" || newerStatus != "queued" {
		t.Errorf("ack statuses = %q / %q, want \"asked\" then \"queued\"", olderStatus, newerStatus)
	}

	// Coexisting asks are app-only: a chat transport cannot make it unambiguous
	// which ask a typed answer belongs to, so the second one waits its turn and
	// nothing extra is put on screen.
	if got := p.presents; got != 1 {
		t.Errorf("presenter calls after a second ask = %d, want 1 (the second ask queues, it is not shown)", got)
	}

	// The older ask keeps the session. PendingForSession is the single hook behind
	// typed-answer routing (run_turn.go / inbox.go), /pause+/resume+/complete
	// (internal/command/ask_pause.go) and the {ask} statusline field.
	if got := router.PendingForSession(askSession); got != older {
		t.Errorf("PendingForSession = %q, want the OLDER ask %q — the newer ask must not displace it", got, older)
	}

	// A typed reply therefore answers the OLDER ask.
	router.HandleResponse(router.PendingForSession(askSession), "typed-older")
	msg, ok := d.last()
	if !ok {
		t.Fatal("typed answer to the primary ask delivered nothing")
	}
	if !strings.Contains(msg, "OLDER-Q") {
		t.Errorf("delivered batch = %q, want the OLDER ask's questions", msg)
	}

	// ...and only now is the queued ask presented.
	if got := p.presents; got != 2 {
		t.Errorf("presenter calls after the primary resolved = %d, want 2 (queued ask promoted and shown)", got)
	}
	if got := router.PendingForSession(askSession); got != newer {
		t.Errorf("PendingForSession after promotion = %q, want the queued ask %q", got, newer)
	}
	saved := loadPersisted(t, idx)
	if len(saved) != 1 || saved[0].RequestID != newer {
		t.Errorf("durable set after promotion = %+v, want exactly the promoted ask %q", saved, newer)
	}
}

// T1b — /pause, /resume and /complete act on the PRIMARY ask only (#1711 ruling
// 2): the OLDEST ask, in both shapes a session can be in — a chat transport where
// the second ask is queued, and the native app where both are genuinely live.
func TestAsk_PauseTargetsThePrimaryAsk(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		coexists bool // app: both asks are on screen at once
	}{
		{"chat-second-ask-queued", false},
		{"app-both-asks-live", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idx := newStateDB(t)
			p := &fakePresenter{}
			d := &fakeDeliver{}
			opts := []AskOption{}
			if tc.coexists {
				opts = append(opts, WithBatchPresent((&multiBatchPresenter{}).present))
			}
			tool, router := NewAskTool(p.present, nil, d.deliver, nil, idx, "test", opts...)

			older := askReqID(t, tool, askOlderInput)
			newer := askReqID(t, tool, askNewerInput)

			saved := loadPersisted(t, idx)
			if e := findPersisted(saved, newer); e == nil || e.Queued == tc.coexists {
				t.Fatalf("second ask %q persisted queued=%v, want queued=%v", newer, e != nil && e.Queued, !tc.coexists)
			}

			if !router.PauseSession(askSession) {
				t.Fatal("PauseSession should succeed while an ask is pending")
			}
			saved = loadPersisted(t, idx)
			if e := findPersisted(saved, older); e == nil || !e.Paused {
				t.Errorf("older ask %q persisted as paused=%v, want true (/pause acts on the primary)", older, e != nil && e.Paused)
			}
			if e := findPersisted(saved, newer); e == nil || e.Paused {
				t.Errorf("newer ask %q persisted as paused=%v, want false", newer, e != nil && e.Paused)
			}

			// /complete likewise ends the PRIMARY, not the newest.
			if _, _, ok := router.CompleteSession(askSession); ok {
				t.Fatal("CompleteSession should report not-ok with nothing answered yet")
			}
			router.HandleResponse(older, "typed-older")
			if _, _, _ = router.CompleteSession(askSession); router.PendingForSession(askSession) != newer {
				t.Errorf("after the primary resolved, PendingForSession = %q, want %q", router.PendingForSession(askSession), newer)
			}
		})
	}
}

// multiBatchPresenter is an AskPresentBatchFn that keeps EVERY presented form's
// callback (fakeBatchPresenter keeps only the last), so a test can resolve two
// coexisting app asks independently and in either order.
type multiBatchPresenter struct {
	mu        sync.Mutex
	presented []string
	cbs       map[string]func([]string)
}

func (f *multiBatchPresenter) present(sessionKey, promptID string, qs []question.Question, onResponse func(answers []string)) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cbs == nil {
		f.cbs = make(map[string]func([]string))
	}
	f.presented = append(f.presented, promptID)
	f.cbs[promptID] = onResponse
	return true
}

// answer resolves the ask with the given request id (its form's promptID is
// question 0's message id).
func (f *multiBatchPresenter) answer(t *testing.T, reqID string, answers []string) {
	t.Helper()
	f.mu.Lock()
	cb := f.cbs[questionMsgID(reqID, 0)]
	f.mu.Unlock()
	if cb == nil {
		t.Fatalf("no batched form registered for ask %q (presented: %v)", reqID, f.presented)
	}
	cb(answers)
}

// T2 — two asks that genuinely coexist (native app, ruling 1) must leave
// CONSISTENT state whichever order they resolve in. removeLocked is asymmetric
// today: resolving the newer first deletes the whole session entry, so the still-
// open older ask reports as "no ask pending".
func TestAsk_CoexistingAppAsksResolveInEitherOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		newerFirst bool
	}{
		{"older-first", false},
		{"newer-first", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idx := newStateDB(t)
			seq := &fakePresenter{}
			bp := &multiBatchPresenter{}
			d := &fakeDeliver{}
			tool, router := NewAskTool(seq.present, nil, d.deliver, nil, idx, "test", WithBatchPresent(bp.present))

			older := askReqID(t, tool, askOlderInput)
			newer := askReqID(t, tool, askNewerInput)

			// The app shows both forms at once — that is what makes coexistence
			// unambiguous, and why it is permitted here and nowhere else.
			if len(bp.presented) != 2 {
				t.Fatalf("batched forms presented = %d, want 2 (app asks coexist)", len(bp.presented))
			}

			first, second := older, newer
			if tc.newerFirst {
				first, second = newer, older
			}

			bp.answer(t, first, []string{"answer-one"})
			if got := router.PendingForSession(askSession); got != second {
				t.Errorf("after resolving %q first, PendingForSession = %q, want the still-open ask %q", first, got, second)
			}
			saved := loadPersisted(t, idx)
			if len(saved) != 1 || saved[0].RequestID != second {
				t.Errorf("durable set after resolving %q = %+v, want exactly the still-open ask %q", first, saved, second)
			}

			bp.answer(t, second, []string{"answer-two"})
			if got := router.PendingForSession(askSession); got != "" {
				t.Errorf("after both asks resolved, PendingForSession = %q, want empty", got)
			}
			if saved := loadPersisted(t, idx); len(saved) != 0 {
				t.Errorf("durable set after both asks resolved = %+v, want empty", saved)
			}
		})
	}
}

// T2b — onResolve fires PER-ASK (#1711 ruling 5). Today it is skipped entirely
// when the OLDER ask resolves first, which is exactly what makes #1712's
// deferral-starvation intermittent rather than permanent.
func TestAsk_OnResolveFiresPerAskInEitherOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		newerFirst bool
	}{
		{"older-first", false},
		{"newer-first", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seq := &fakePresenter{}
			bp := &multiBatchPresenter{}
			d := &fakeDeliver{}
			resolved := make(chan string, 4)
			tool, _ := NewAskTool(seq.present, nil, d.deliver, nil, nil, "test",
				WithBatchPresent(bp.present),
				WithOnResolve(func(_, reqID string) { resolved <- reqID }))

			older := askReqID(t, tool, askOlderInput)
			newer := askReqID(t, tool, askNewerInput)
			first, second := older, newer
			if tc.newerFirst {
				first, second = newer, older
			}

			bp.answer(t, first, []string{"answer-one"})
			select {
			case <-resolved:
			case <-time.After(time.Second):
				t.Fatalf("onResolve did not fire when ask %q resolved", first)
			}
			bp.answer(t, second, []string{"answer-two"})
			select {
			case <-resolved:
			case <-time.After(time.Second):
				t.Fatalf("onResolve did not fire when ask %q resolved", second)
			}
		})
	}
}

// seedAsk builds one persisted entry in the shape the live artifact has (batched
// app form, un-advanced, its platform message id = question 0's msg id).
func seedAsk(reqID string, created time.Time) persistedAsk {
	return persistedAsk{
		RequestID:     reqID,
		SessionKey:    askSession,
		Questions:     []question.Question{{Question: "## " + reqID, ID: "vocab:1"}},
		Idx:           0,
		CreatedAt:     created,
		PlatformMsgID: questionMsgID(reqID, 0),
		Batched:       true,
	}
}

// T3 — restart determinism. persistLocked iterates a Go map, so the persisted
// array order is arbitrary: a real artifact (helen, three pending asks in ONE
// session) is stored at 19:58 / 08:00 / 02:14 — not created_at order. Restore must
// elect the SAME primary (the oldest — the head of the queue) from every possible
// array order, so which ask owns typed answers and /pause is not re-rolled on each
// restart. All six permutations are exercised: one run passes by luck ~1/3 of the
// time under the unsorted code.
func TestAsk_RestoreElectsOldestPrimaryFromAnyPersistedOrder(t *testing.T) {
	t.Parallel()
	now := time.Now()
	// Oldest → newest. Names deliberately do NOT sort in created_at order, so a
	// lexicographic tiebreak cannot accidentally produce the right answer.
	entries := []persistedAsk{
		seedAsk("ask-test-zzz-0214", now.Add(-6*time.Hour)),    // oldest
		seedAsk("ask-test-mmm-0800", now.Add(-4*time.Hour)),    //
		seedAsk("ask-test-aaa-1958", now.Add(-90*time.Minute)), // newest
	}
	wantPrimary := entries[0].RequestID

	for _, order := range [][]int{
		{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0},
	} {
		order := order
		t.Run(permName(order), func(t *testing.T) {
			t.Parallel()
			idx := newStateDB(t)
			blob := make([]persistedAsk, 0, len(order))
			for _, i := range order {
				blob = append(blob, entries[i])
			}
			data, err := json.Marshal(blob)
			if err != nil {
				t.Fatal(err)
			}
			if err := idx.SetAgentMetadata("test", askMetaKey, string(data)); err != nil {
				t.Fatal(err)
			}

			fr := &fakeRestore{}
			p := &fakePresenter{}
			d := &fakeDeliver{}
			_, router := NewAskTool(p.present, fr.restore, d.deliver, nil, idx, "test")

			if got := router.PendingForSession(askSession); got != wantPrimary {
				t.Errorf("restored primary = %q, want the OLDEST ask %q (persisted order %v must not decide it)", got, wantPrimary, order)
			}
			// All three survived: none may be dropped by the election.
			saved := loadPersisted(t, idx)
			if len(saved) != 3 {
				t.Fatalf("durable set after restore = %d asks, want 3", len(saved))
			}
			ids := make([]string, 0, 3)
			for _, s := range saved {
				ids = append(ids, s.RequestID)
			}
			sort.Strings(ids)
			want := []string{"ask-test-aaa-1958", "ask-test-mmm-0800", "ask-test-zzz-0214"}
			for i := range want {
				if ids[i] != want[i] {
					t.Fatalf("durable set after restore = %v, want %v", ids, want)
				}
			}
		})
	}
}

func permName(order []int) string {
	var b strings.Builder
	for _, i := range order {
		b.WriteByte(byte('0' + i))
	}
	return b.String()
}

// A queued ask is persisted with its queued flag set, and the primary without it
// (#1711 ruling Q3): losing a queued ask across a restart is worse than losing a
// live one, because the user never saw it and the agent is still waiting.
func TestAsk_QueuedFlagIsPersisted(t *testing.T) {
	t.Parallel()
	idx := newStateDB(t)
	p := &fakePresenter{}
	d := &fakeDeliver{}
	tool, _ := NewAskTool(p.present, nil, d.deliver, nil, idx, "test")

	older := askReqID(t, tool, askOlderInput)
	newer := askReqID(t, tool, askNewerInput)

	saved := loadPersisted(t, idx)
	if e := findPersisted(saved, older); e == nil || e.Queued {
		t.Errorf("primary ask %q persisted as %+v, want queued=false", older, e)
	}
	if e := findPersisted(saved, newer); e == nil || !e.Queued {
		t.Errorf("second ask %q persisted as %+v, want queued=true", newer, e)
	}
}

// A queue that drains hours later must not surface a question that went stale
// while it waited: the TTL is applied at PRESENTATION, aged from created_at
// (#1711 ruling Q3). An expired queued ask is dropped with a notice to the agent
// — which never saw an answer for it — instead of being shown.
func TestAsk_QueuedAskExpiresAtPresentationTime(t *testing.T) {
	t.Parallel()
	p := &fakePresenter{}
	d := &fakeDeliver{}
	st := newAskState(p.present, nil, d.deliver, nil, nil, "test")

	qs := []question.Question{{Question: "OLDER-Q"}}
	older, _ := st.start(askSession, qs, nil, graderConfig{})
	stale, staleQueued := st.start(askSession, []question.Question{{Question: "STALE-Q"}}, nil, graderConfig{})
	fresh, freshQueued := st.start(askSession, []question.Question{{Question: "FRESH-Q"}}, nil, graderConfig{})
	if !staleQueued || !freshQueued {
		t.Fatalf("start reported queued=%v/%v for asks behind a live one, want both queued", staleQueued, freshQueued)
	}

	// The middle of the queue went stale while it waited its turn.
	st.mu.Lock()
	st.byReqID[stale].createdAt = time.Now().Add(-2 * pendingAskTTL)
	st.mu.Unlock()

	st.handleResponse(older, "typed") // resolves the primary → drains the queue

	// The stale entry is gone, the agent was told, and the FRESH one is on screen.
	if got := st.pendingForSession(askSession); got != fresh {
		t.Errorf("primary after draining the queue = %q, want the fresh ask %q (the stale one must be skipped)", got, fresh)
	}
	var notice string
	for i, m := range d.messages {
		if d.reqIDs[i] == stale {
			notice = m
		}
	}
	if !strings.Contains(notice, "expired") || !strings.Contains(notice, "NEVER shown") {
		t.Errorf("expiry notice for the stale queued ask = %q, want an explicit never-shown expiry notice", notice)
	}
	if p.presents != 2 { // OLDER at start, FRESH after the drain — never STALE
		t.Errorf("presenter calls = %d, want 2 (the expired queued ask must never reach the screen)", p.presents)
	}
}

// A restart whose session has ONLY queued asks (its live one expired or was
// answered before the crash) must still drain the queue rather than strand it:
// restore rehydrates the queue and promotes its head.
func TestAsk_RestorePromotesQueueWhenNoLiveAskSurvives(t *testing.T) {
	t.Parallel()
	idx := newStateDB(t)
	now := time.Now()
	a := seedAsk("ask-test-queued-older", now.Add(-3*time.Hour))
	a.Batched = false
	a.PlatformMsgID = ""
	a.Queued = true
	b := seedAsk("ask-test-queued-newer", now.Add(-time.Hour))
	b.Batched = false
	b.PlatformMsgID = ""
	b.Queued = true
	data, err := json.Marshal([]persistedAsk{b, a}) // persisted out of created_at order
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.SetAgentMetadata("test", askMetaKey, string(data)); err != nil {
		t.Fatal(err)
	}

	fr := &fakeRestore{}
	p := &fakePresenter{}
	d := &fakeDeliver{}
	_, router := NewAskTool(p.present, fr.restore, d.deliver, nil, idx, "test")

	if got := router.PendingForSession(askSession); got != a.RequestID {
		t.Errorf("primary after restoring a queue-only session = %q, want the queue head %q", got, a.RequestID)
	}
	if p.presents != 1 {
		t.Errorf("presenter calls after restore = %d, want 1 (only the queue head is shown)", p.presents)
	}
	// A queued ask was never on screen, so there are no buttons to re-attach to.
	if fr.calls != 0 {
		t.Errorf("reattach fired %d times for never-presented queued asks, want 0", fr.calls)
	}
	saved := loadPersisted(t, idx)
	if e := findPersisted(saved, a.RequestID); e == nil || e.Queued {
		t.Errorf("promoted ask persisted as %+v, want queued=false", e)
	}
	if e := findPersisted(saved, b.RequestID); e == nil || !e.Queued {
		t.Errorf("still-waiting ask persisted as %+v, want queued=true", e)
	}
}

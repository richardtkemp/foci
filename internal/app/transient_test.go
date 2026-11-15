package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/fap"
)

// Tests for transient frames (#1064): status and streaming frames go out live
// only (no seq, not stored, not replayed), seqs stay contiguous, the hello
// carries meta and the prompt suggestion, activity is rate-limited, and the
// upgrade drops the stored transient rows without leaving a seq hole at the
// top of a conversation.

// rawWires drains the socket queue as raw wire strings.
func rawWires(c *wsClient) []string {
	var out []string
	for {
		select {
		case b := <-c.send:
			out = append(out, string(b))
		default:
			return out
		}
	}
}

func wireType(t *testing.T, w string) (string, bool) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(w), &raw); err != nil {
		t.Fatalf("bad wire %q: %v", w, err)
	}
	var typ string
	_ = json.Unmarshal(raw["t"], &typ)
	_, hasSeq := raw["seq"]
	return typ, hasSeq
}

// allTransientFrames returns one frame of each transient type for convID.
func allTransientFrames(convID string) []fap.ServerFrame {
	cost := 0.12
	return []fap.ServerFrame{
		fap.Activity{ConversationID: convID, Kind: "tool", Detail: "Read file"},
		fap.TurnStart{ConversationID: convID, TurnID: "t1"},
		fap.TextDelta{ConversationID: convID, TurnID: "t1", Text: "he"},
		fap.TextDelta{ConversationID: convID, TurnID: "t1", Text: "llo"},
		fap.Meta{ConversationID: convID, Model: "m1", PrevCostUsd: &cost},
		fap.PromptSuggestion{ConversationID: convID, Text: "run tests"},
		fap.Commands{ConversationID: convID, Commands: []fap.CommandInfo{{Name: "x"}}},
		fap.CacheExpiry{ConversationID: convID, ExpiryMs: 123},
		fap.Reminders{ConversationID: convID, Reminders: []fap.PendingReminder{{ID: 1, Text: "r", DueAtMs: 9, State: "pending"}}},
		fap.WaitingOnOther{ConversationID: convID, WaitingOnOther: true},
	}
}

// H1, H2, H3: transient frames carry no seq, are neither buffered nor stored
// nor replayed, and the sequenced frames around them stay contiguous.
func TestTransient_NoSeqNotStoredNotReplayed(t *testing.T) {
	fs := tempFrameStore(t)
	b := &convBinding{convID: "c1", store: fs, seen: map[string]struct{}{}}
	c := fakeClient()
	b.attach(c)

	b.send(fap.ServerMessage{ConversationID: "c1", MessageID: "m1", Role: "agent", Text: "before"}) // seq 1
	for _, f := range allTransientFrames("c1") {
		b.send(f)
	}
	final := "hello"
	stored := make(chan struct{})
	b.sendThen(fap.TextEnd{ConversationID: "c1", TurnID: "t1", MessageID: "m2", FinalText: &final}, func() { close(stored) })
	select {
	case <-stored:
	case <-time.After(10 * time.Second):
		t.Fatal("text.end not stored")
	}

	var seqs []int64
	transient := 0
	for _, w := range rawWires(c) {
		typ, hasSeq := wireType(t, w)
		if isTransientType(typ) {
			transient++
			if hasSeq {
				t.Errorf("transient %s carries a seq: %s", typ, w)
			}
			continue
		}
		if !hasSeq {
			t.Errorf("sequenced %s has no seq: %s", typ, w)
		}
		var env fap.Envelope
		_ = json.Unmarshal([]byte(w), &env)
		seqs = append(seqs, env.Seq)
	}
	if transient != len(allTransientFrames("c1")) {
		t.Errorf("client got %d transient frames, want %d", transient, len(allTransientFrames("c1")))
	}
	if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Errorf("sequenced seqs = %v, want [1 2] (contiguous)", seqs)
	}
	if got := b.info().LastSeq; got != 2 {
		t.Errorf("roster lastSeq = %d, want 2", got)
	}

	b.mu.Lock()
	nbuf := len(b.buffer)
	b.mu.Unlock()
	if nbuf != 2 {
		t.Errorf("replay buffer holds %d frames, want 2 (sequenced only)", nbuf)
	}
	for _, f := range fs.Range("c1", 0, 100) {
		if typ, _ := wireType(t, f.wire); isTransientType(typ) {
			t.Errorf("store holds transient %s at seq %d", typ, f.seq)
		}
	}
	// Resume from 0, from memory and from the store alone.
	for _, fromMemory := range []bool{true, false} {
		if !fromMemory {
			b.mu.Lock()
			b.buffer = nil
			b.mu.Unlock()
		}
		re := fakeClient()
		b.attach(re)
		b.replayTo(re, 0)
		var got []int64
		for _, w := range rawWires(re) {
			typ, _ := wireType(t, w)
			if isTransientType(typ) {
				t.Errorf("fromMemory=%v: replay re-sent transient %s", fromMemory, typ)
			}
			var env fap.Envelope
			_ = json.Unmarshal([]byte(w), &env)
			got = append(got, env.Seq)
		}
		if len(got) != 2 || got[0] != 1 || got[1] != 2 {
			t.Errorf("fromMemory=%v: replay seqs = %v, want [1 2]", fromMemory, got)
		}
	}
	// A restart continues at the next seq.
	b2 := &convBinding{convID: "c1", store: fs, seq: fs.MaxSeq("c1"), seen: map[string]struct{}{}}
	c2 := fakeClient()
	b2.attach(c2)
	b2.send(fap.ServerMessage{ConversationID: "c1", MessageID: "m3", Role: "agent", Text: "after"})
	if ds := drainEnv(t, c2); len(ds) != 1 || ds[0].seq != 3 {
		t.Errorf("after restart = %v, want one frame at seq 3", ds)
	}
}

// H16: transient frames never push and never move the roster preview.
func TestTransient_NoPushNoPreview(t *testing.T) {
	var pushes []pushPayload
	b := &convBinding{convID: "c1", seen: map[string]struct{}{}, notifyOffline: func(p pushPayload) { pushes = append(pushes, p) }}
	for _, f := range allTransientFrames("c1") {
		b.send(f)
	}
	if len(pushes) != 0 || b.lastPreview != "" || b.lastActMs != 0 {
		t.Errorf("transient frames pushed %d / preview %q / lastActMs %d, want none", len(pushes), b.lastPreview, b.lastActMs)
	}
	final := "done"
	b.send(fap.TextEnd{ConversationID: "c1", TurnID: "t1", MessageID: "m1", FinalText: &final})
	if len(pushes) != 1 || b.lastPreview == "" {
		t.Errorf("text.end pushed %d / preview %q, want one push and a preview", len(pushes), b.lastPreview)
	}
}

// H5: the hello entry carries meta (no conversationId) and the suggestion,
// and "promptSuggestion":"" when there is none.
func TestTransient_HelloCarriesMetaAndSuggestion(t *testing.T) {
	b := &convBinding{convID: "c1", seen: map[string]struct{}{}}
	cost := 0.12
	b.send(fap.Meta{ConversationID: "c1", Model: "m1", PrevCostUsd: &cost, Tokens: &fap.Tokens{In: 100, Out: 5, CR: 900}, Gap: "3m", CompactionLimitTokens: 150000})
	b.send(fap.PromptSuggestion{ConversationID: "c1", Text: "run tests"})
	ci := b.info()
	raw, _ := json.Marshal(ci)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	var meta map[string]any
	if err := json.Unmarshal(m["meta"], &meta); err != nil {
		t.Fatalf("hello meta: %v (%s)", err, raw)
	}
	if meta["model"] != "m1" || meta["prevCostUsd"] != 0.12 || meta["gap"] != "3m" || meta["compactionLimitTokens"] != float64(150000) {
		t.Errorf("hello meta = %v", meta)
	}
	if _, has := meta["conversationId"]; has {
		t.Errorf("hello meta carries conversationId: %v", meta)
	}
	if string(m["promptSuggestion"]) != `"run tests"` {
		t.Errorf("promptSuggestion = %s", m["promptSuggestion"])
	}

	empty := (&convBinding{convID: "c2"}).info()
	raw, _ = json.Marshal(empty)
	if !strings.Contains(string(raw), `"promptSuggestion":""`) {
		t.Errorf("no-suggestion hello lacks \"promptSuggestion\":\"\": %s", raw)
	}
	if strings.Contains(string(raw), `"meta"`) {
		t.Errorf("never-had-meta hello carries meta: %s", raw)
	}
}

// H6: a silent turn keeps the suggestion; a visible reply or the clear frame
// drops it.
func TestTransient_SuggestionClearing(t *testing.T) {
	b := &convBinding{convID: "c1", seen: map[string]struct{}{}}
	b.send(fap.PromptSuggestion{ConversationID: "c1", Text: "S"})
	for _, k := range []string{"warming", "thinking", "tool", "idle"} {
		b.send(fap.Activity{ConversationID: "c1", Kind: k})
	}
	b.send(fap.CacheExpiry{ConversationID: "c1", ExpiryMs: 5})
	if got := b.info().PromptSuggestion; got != "S" {
		t.Fatalf("after a silent turn suggestion = %q, want S", got)
	}
	b.send(fap.ServerMessage{ConversationID: "c1", MessageID: "m1", Role: "agent", Text: "hi"})
	if got := b.info().PromptSuggestion; got != "" {
		t.Errorf("after a visible reply suggestion = %q, want empty", got)
	}
	for _, clear := range []fap.ServerFrame{
		fap.TurnStart{ConversationID: "c1", TurnID: "t"},
		fap.Media{ConversationID: "c1", MessageID: "m", BlobID: "b", MIME: "image/png"},
		fap.Interactive{ConversationID: "c1", PromptID: "p", Text: "?"},
		fap.PromptSuggestion{ConversationID: "c1"},
	} {
		b.send(fap.PromptSuggestion{ConversationID: "c1", Text: "S"})
		b.send(clear)
		if got := b.info().PromptSuggestion; got != "" {
			t.Errorf("after %s suggestion = %q, want empty", clear.Type(), got)
		}
	}
}

// H7: meta and the suggestion survive a restart (the snapshot table), and the
// hub's ensureBinding restores them.
func TestTransient_SnapshotSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frames.db")
	fs, err := newFrameStore(path, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b := &convBinding{convID: "c1", store: fs, seen: map[string]struct{}{}}
	b.send(fap.Meta{ConversationID: "c1", Model: "m1"})
	b.send(fap.PromptSuggestion{ConversationID: "c1", Text: "run tests"})
	fs.Close()

	fs2, err := newFrameStore(path, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer fs2.Close()
	h := newTestHub()
	h.frames = fs2
	nb := h.ensureBinding(nil, "ag", "c1")
	ci := nb.info()
	if ci.Meta == nil || ci.Meta.Model != "m1" || ci.PromptSuggestion != "run tests" {
		t.Errorf("after restart hello meta=%+v suggestion=%q, want m1 / run tests", ci.Meta, ci.PromptSuggestion)
	}
}

// fakeActivityClock drives the activity rate limit by hand.
type fakeActivityClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []fakeTimer
}

type fakeTimer struct {
	at time.Time
	f  func()
}

func (k *fakeActivityClock) Now() time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.now
}

func (k *fakeActivityClock) After(d time.Duration, f func()) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.timers = append(k.timers, fakeTimer{k.now.Add(d), f})
}

// Advance moves the clock to t, firing due timers in order.
func (k *fakeActivityClock) Advance(to time.Time) {
	for {
		k.mu.Lock()
		idx := -1
		for i, tm := range k.timers {
			if !tm.at.After(to) && (idx < 0 || tm.at.Before(k.timers[idx].at)) {
				idx = i
			}
		}
		if idx < 0 {
			k.now = to
			k.mu.Unlock()
			return
		}
		tm := k.timers[idx]
		k.timers = append(k.timers[:idx], k.timers[idx+1:]...)
		k.now = tm.at
		k.mu.Unlock()
		tm.f()
	}
}

// withActivityRateLimit turns the package-wide rate limit on with a fake clock.
func withActivityRateLimit(t *testing.T) *fakeActivityClock {
	t.Helper()
	k := &fakeActivityClock{now: time.Unix(1_800_000_000, 0)}
	oldI, oldN, oldA := activityInterval, activityNow, activityAfter
	activityInterval, activityNow, activityAfter = activityMinInterval, k.Now, k.After
	t.Cleanup(func() { activityInterval, activityNow, activityAfter = oldI, oldN, oldA })
	return k
}

type timedFrame struct {
	at time.Time
	a  fap.Activity
}

// activityRecv drains c and stamps each Activity frame with the clock's time.
func activityRecv(t *testing.T, k *fakeActivityClock, c *wsClient, into *[]timedFrame) {
	t.Helper()
	for _, w := range rawWires(c) {
		var env fap.Envelope
		if err := json.Unmarshal([]byte(w), &env); err != nil {
			t.Fatal(err)
		}
		if env.T != fap.TypeActivity {
			continue
		}
		var a fap.Activity
		if err := json.Unmarshal(env.D, &a); err != nil {
			t.Fatal(err)
		}
		*into = append(*into, timedFrame{k.Now(), a})
	}
}

// H10, H12: activity reaches sockets at most once per 0.5 s and ends on the
// current state, while the hello snapshot and the change hook see every
// change at once.
func TestTransient_ActivityRateLimit(t *testing.T) {
	k := withActivityRateLimit(t)
	hookCalls := 0
	b := &convBinding{convID: "c1", seen: map[string]struct{}{}, onActivityChange: func(*convBinding) { hookCalls++ }}
	c := fakeClient()
	b.attach(c)
	start := k.Now()
	var got []timedFrame
	for i := 0; i < 10; i++ {
		k.Advance(start.Add(time.Duration(i) * 50 * time.Millisecond))
		kind := fap.ActivityKindTyping
		detail := ""
		if i%2 == 1 || i == 9 {
			kind, detail = fap.ActivityKindTool, "X"
		}
		if i == 9 {
			detail = "X"
		} else if kind == fap.ActivityKindTool {
			detail = "step"
		}
		b.setTurnTool(kind, detail, "", false)
		if ci := b.info(); ci.ActivityDetail != detail || ci.Activity != string(kind) {
			t.Fatalf("change %d: hello snapshot = %s/%q, want %s/%q at once", i, ci.Activity, ci.ActivityDetail, kind, detail)
		}
		activityRecv(t, k, c, &got)
	}
	if hookCalls < 9 {
		t.Errorf("onActivityChange ran %d times, want each change (>= 9)", hookCalls)
	}
	for _, step := range []time.Duration{500, 600, 950} {
		k.Advance(start.Add(step * time.Millisecond))
		activityRecv(t, k, c, &got)
	}
	for i := 1; i < len(got); i++ {
		if gap := got[i].at.Sub(got[i-1].at); gap < activityMinInterval {
			t.Errorf("frames %d and %d only %v apart", i-1, i, gap)
		}
	}
	early := 0
	for _, f := range got {
		if f.at.Before(start.Add(600 * time.Millisecond)) {
			early++
		}
	}
	if early > 2 {
		t.Errorf("%d frames in the first 600ms, want at most 2", early)
	}
	if len(got) == 0 || got[len(got)-1].a.Kind != string(fap.ActivityKindTool) || got[len(got)-1].a.Detail != "X" {
		t.Fatalf("last frame by 950ms = %+v, want tool X", got)
	}

	// A single change after a quiet second reaches the socket at once.
	k.Advance(start.Add(2 * time.Second))
	got = nil
	b.setTurnActivity(fap.ActivityKindIdle)
	activityRecv(t, k, c, &got)
	if len(got) != 1 || got[0].a.Kind != string(fap.ActivityKindIdle) {
		t.Errorf("change after quiet = %+v, want one idle frame at once", got)
	}
}

// H11: only activity is rate-limited.
func TestTransient_OnlyActivityIsLimited(t *testing.T) {
	k := withActivityRateLimit(t)
	b := &convBinding{convID: "c1", seen: map[string]struct{}{}}
	c := fakeClient()
	b.attach(c)
	b.setTurnActivity(fap.ActivityKindTyping)
	rawWires(c)
	start := k.Now()
	for i := 0; i < 3; i++ {
		k.Advance(start.Add(time.Duration(i) * 50 * time.Millisecond))
		b.send(fap.WaitingOnOther{ConversationID: "c1", WaitingOnOther: i%2 == 0})
		b.send(fap.TextDelta{ConversationID: "c1", TurnID: "t", Text: "x"})
		if n := len(rawWires(c)); n != 2 {
			t.Errorf("step %d: %d frames sent at once, want 2", i, n)
		}
	}
}

// H12 (#3118): a running command's relabel still lands while rate-limited.
func TestTransient_RelabelUnderRateLimit(t *testing.T) {
	k := withActivityRateLimit(t)
	b := &convBinding{convID: "c1", seen: map[string]struct{}{}}
	c := fakeClient()
	b.attach(c)
	n := b.setTurnTool(fap.ActivityKindTool, "Bash", "ls", true)
	if !b.turnToolRunning(n) {
		t.Fatal("turnToolRunning = false right after setTurnTool")
	}
	start := k.Now()
	k.Advance(start.Add(100 * time.Millisecond))
	b.setTurnToolDetail(n, "Listing files")
	if got := b.info().ActivityDetail; got != "Listing files" {
		t.Errorf("hello caption = %q, want the new caption at once", got)
	}
	var got []timedFrame
	activityRecv(t, k, c, &got)
	k.Advance(start.Add(500 * time.Millisecond))
	activityRecv(t, k, c, &got)
	if len(got) == 0 || got[len(got)-1].a.Detail != "Listing files" {
		t.Errorf("client frames = %+v, want the relabel by 500ms", got)
	}
}

// H8: the connect catch-up sends a device only the values that moved since
// its hello was built.
func TestTransient_DiffSendsWhatMoved(t *testing.T) {
	old := fap.ConversationInfo{ID: "c1", Activity: "typing", CacheExpiryMs: 5, PromptSuggestion: "S"}
	cur := old
	cur.Activity, cur.ActivityDetail = "tool", "Read file"
	cur.Meta = &fap.ConversationMeta{Model: "m1"}
	frames := transientDiff(cur, old, true)
	var types []string
	for _, f := range frames {
		types = append(types, f.Type())
	}
	if strings.Join(types, ",") != "activity,meta" {
		t.Errorf("diff = %v, want [activity meta]", types)
	}
	if a := frames[0].(fap.Activity); a.Kind != "tool" || a.Detail != "Read file" {
		t.Errorf("catch-up activity = %+v", a)
	}
	if n := len(transientDiff(cur, cur, true)); n != 0 {
		t.Errorf("no change sent %d frames", n)
	}
	if n := len(transientDiff(cur, fap.ConversationInfo{}, false)); n < 5 {
		t.Errorf("a conversation not in the hello sent %d frames, want every value", n)
	}
}

// --- the upgrade (#1064 ruling 2026-11-15: stored transient rows dropped) ---

func encodeAt(t *testing.T, f fap.ServerFrame, seq int64) string {
	t.Helper()
	w, err := fap.Encode(f, seq, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// seedPreUpgrade writes, as BASE's send path did, seqs 1..40 for c1 with
// activity, text.delta, meta and prompt.suggestion rows among them and an
// activity row on top; and a conversation c2 whose top row is sequenced.
func seedPreUpgrade(t *testing.T, path string) map[int64]string {
	t.Helper()
	fs, err := newFrameStore(path, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// newFrameStore marked the (empty) store migrated; a pre-upgrade store
	// has no marker.
	if _, err := fs.db.Exec(`DELETE FROM app_migrations`); err != nil {
		t.Fatal(err)
	}
	types := map[int64]string{}
	now := time.Now().UnixMilli()
	for seq := int64(1); seq <= 40; seq++ {
		var f fap.ServerFrame
		switch {
		case seq == 40:
			f = fap.Activity{ConversationID: "c1", Kind: "idle"}
		case seq == 38:
			f = fap.Meta{ConversationID: "c1", Model: "m-old"}
		case seq == 39:
			f = fap.PromptSuggestion{ConversationID: "c1", Text: "run tests"}
		case seq%4 == 0:
			f = fap.Activity{ConversationID: "c1", Kind: "typing"}
		case seq%4 == 1:
			f = fap.TextDelta{ConversationID: "c1", TurnID: "t", Text: "x"}
		default:
			f = fap.ServerMessage{ConversationID: "c1", MessageID: "m" + string(rune('a'+seq)), Role: "agent", Text: "hi"}
		}
		types[seq] = f.Type()
		_, visible := pushPreview(f)
		fs.insert(frameWrite{convID: "c1", agentID: "ag", seq: seq, wire: encodeAt(t, f, seq), sentMs: now, visible: visible})
	}
	fs.insert(frameWrite{convID: "c2", agentID: "ag", seq: 1, wire: encodeAt(t, fap.Activity{ConversationID: "c2", Kind: "typing"}, 1), sentMs: now})
	fs.insert(frameWrite{convID: "c2", agentID: "ag", seq: 2, wire: encodeAt(t, fap.ServerMessage{ConversationID: "c2", MessageID: "x", Role: "agent", Text: "x"}, 2), sentMs: now, visible: true})
	fs.Close()
	return types
}

func TestTransient_UpgradeDropsStoredTransientRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frames.db")
	types := seedPreUpgrade(t, path)

	fs, err := newFrameStore(path, 24*time.Hour) // runs the upgrade
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()

	rows := fs.Range("c1", 0, 100)
	var seqs []int64
	for _, r := range rows {
		typ, _ := wireType(t, r.wire)
		seqs = append(seqs, r.seq)
		if isTransientType(typ) {
			t.Errorf("seq %d: transient %s survived the upgrade", r.seq, typ)
		}
		if r.seq < 40 && types[r.seq] != typ {
			t.Errorf("seq %d: stored %s, want the original %s", r.seq, typ, types[r.seq])
		}
	}
	// The top seq is kept, as a conversation.ack with the same seq.
	if len(rows) == 0 || rows[len(rows)-1].seq != 40 {
		t.Fatalf("top seq after upgrade = %v, want 40 kept", seqs)
	}
	var top fap.Envelope
	_ = json.Unmarshal([]byte(rows[len(rows)-1].wire), &top)
	if top.T != fap.TypeConversationAck || top.Seq != 40 {
		t.Errorf("top row = %s seq %d, want conversation.ack seq 40", top.T, top.Seq)
	}
	if got := fs.MaxSeq("c1"); got != 40 {
		t.Errorf("MaxSeq(c1) = %d, want 40", got)
	}
	var hw int64
	if err := fs.db.QueryRow(`SELECT max_seq FROM app_conv_seq WHERE conv_id = 'c1'`).Scan(&hw); err != nil || hw != 40 {
		t.Errorf("app_conv_seq(c1) = %d (%v), want 40", hw, err)
	}
	// c2's top row was sequenced: only its transient row went, no rewrite.
	if r := fs.Range("c2", 0, 10); len(r) != 1 || r[0].seq != 2 {
		t.Errorf("c2 after upgrade = %+v, want only seq 2", r)
	}
	// The snapshot was seeded from the dropped rows.
	meta, sugg := fs.ConvSnapshot("c1")
	if meta == nil || meta.Model != "m-old" || sugg != "run tests" {
		t.Errorf("seeded snapshot = %+v / %q, want m-old / run tests", meta, sugg)
	}
	// Still restorable (a visible frame remains).
	found := false
	for _, rc := range fs.RestorableConvs() {
		found = found || rc.convID == "c1"
	}
	if !found {
		t.Error("c1 is no longer restorable after the upgrade")
	}

	// Resume from 20 over a binding rebuilt on the upgraded store: ascending,
	// ends at the top seq, and new frames continue from 41.
	b := &convBinding{convID: "c1", store: fs, seq: fs.MaxSeq("c1"), seen: map[string]struct{}{}}
	c := fakeClient()
	c.send = make(chan []byte, 100)
	b.attach(c)
	b.replayTo(c, 20)
	ds := drainEnv(t, c)
	if len(ds) == 0 || ds[len(ds)-1].seq != 40 {
		t.Fatalf("replay from 20 = %v, want it to end at 40", ds)
	}
	for i := 1; i < len(ds); i++ {
		if ds[i].seq <= ds[i-1].seq || ds[i].seq <= 20 {
			t.Errorf("replay out of order at %d: %v", i, ds)
		}
	}
	b.send(fap.ServerMessage{ConversationID: "c1", MessageID: "new", Role: "agent", Text: "new"})
	if ds := drainEnv(t, c); len(ds) != 1 || ds[0].seq != 41 {
		t.Errorf("first new frame = %v, want seq 41", ds)
	}
}

// The upgrade runs once: a later open does not touch rows again.
func TestTransient_UpgradeRunsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frames.db")
	seedPreUpgrade(t, path)
	fs, err := newFrameStore(path, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// A transient row written after the upgrade (e.g. by a rolled-back binary).
	fs.insert(frameWrite{convID: "c3", agentID: "ag", seq: 1, wire: encodeAt(t, fap.Activity{ConversationID: "c3", Kind: "typing"}, 1), sentMs: time.Now().UnixMilli()})
	fs.Close()
	fs2, err := newFrameStore(path, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer fs2.Close()
	if r := fs2.Range("c3", 0, 10); len(r) != 1 {
		t.Errorf("second open changed rows: %+v", r)
	}
}

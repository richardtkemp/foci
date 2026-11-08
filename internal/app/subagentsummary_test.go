package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/fap"
)

// subagentSummaryHub is a hub with one conversation c1 bound to session
// "ag/c7", a stub summariser and a fake clock for the activity store.
func subagentSummaryHub(t *testing.T, reply string) (*Hub, *openRouterStub, *time.Time) {
	t.Helper()
	stub := &openRouterStub{reply: reply}
	srv := stub.serve(t)
	h := newTestHub()
	h.pinSum = testSummariser(srv.URL, nil)
	h.subAct = newSubagentActivityStore()
	now := time.Date(2026, 11, 8, 3, 0, 0, 0, time.UTC)
	h.subAct.now = func() time.Time { return now }
	b := withIdent(&convBinding{convID: "c1", chatID: 7}, "ag", "ag/c7")
	h.mu.Lock()
	h.convs["c1"] = b
	h.bySession["ag/c7"] = b
	h.mu.Unlock()
	return h, stub, &now
}

// awaitSubagentSummary waits for the next frame on c, which must be a
// subagent.summary.
func awaitSubagentSummary(t *testing.T, c *wsClient) map[string]any {
	t.Helper()
	select {
	case b := <-c.send:
		var env struct {
			T string         `json:"t"`
			D map[string]any `json:"d"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatalf("bad frame: %v", err)
		}
		if env.T != fap.TypeSubagentSummary {
			t.Fatalf("frame type = %q, want %q", env.T, fap.TypeSubagentSummary)
		}
		return env.D
	case <-time.After(10 * time.Second):
		t.Fatal("no subagent.summary within 10s")
		return nil
	}
}

func expectNoFrame(t *testing.T, c *wsClient) {
	t.Helper()
	select {
	case b := <-c.send:
		t.Fatalf("unexpected frame %s", b)
	case <-time.After(200 * time.Millisecond):
	}
}

// The model reads the subagent's last three steps, text and tool calls, under
// the six-word prompt, and its answer is sent as written (never cut: Dick,
// 2026-11-08).
func TestSubagentSummary_InputAndCap(t *testing.T) {
	h, stub, _ := subagentSummaryHub(t, "Reading the config loader.")
	st := h.subAct
	st.note("ag/c7", "toolu_a", "text", "first, oldest step")
	st.note("ag/c7", "toolu_a", "text", "Looking   at\nthe parser")
	st.note("ag/c7", "toolu_a", "tool", `Read {"file_path":"/x/config.go"}`)
	st.note("ag/c7", "toolu_a", "tool", "Grep")
	st.note("ag/c7", "other", "text", "another subagent's step")

	c := fakeClient()
	h.handleSubagentSummarize(c, fap.SubagentSummarize{ConversationID: "c1", IDs: []string{"toolu_a"}})
	f := awaitSubagentSummary(t, c)
	if f["conversationId"] != "c1" || f["id"] != "toolu_a" || f["summary"] != "Reading the config loader" {
		t.Errorf("answer = %v, want the cleaned answer", f)
	}
	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("model calls = %d, want 1", len(reqs))
	}
	msgs := reqs[0]["messages"].([]any)
	system := msgs[0].(map[string]any)["content"].(string)
	user := msgs[1].(map[string]any)["content"].(string)
	if !strings.Contains(system, "AT MOST SIX WORDS") || !strings.Contains(system, "CURRENT step") {
		t.Errorf("system prompt = %q", system)
	}
	want := "Message: Looking at the parser\nTool call: Read {\"file_path\":\"/x/config.go\"}\nTool call: Grep"
	if user != want {
		t.Errorf("model input = %q, want %q", user, want)
	}
}

// Unchanged activity is answered from the last summary with no model call; new
// activity is summarised again only once subagentSummaryMinGap has passed.
func TestSubagentSummary_NoRepeatAndThrottle(t *testing.T) {
	h, stub, now := subagentSummaryHub(t, "Reading the parser")
	h.subAct.note("ag/c7", "toolu_a", "text", "step one")
	c := fakeClient()
	ask := func() {
		h.handleSubagentSummarize(c, fap.SubagentSummarize{ConversationID: "c1", IDs: []string{"toolu_a"}})
	}

	ask()
	awaitSubagentSummary(t, c)
	*now = now.Add(30 * time.Second)
	ask() // unchanged: the cached summary, no call
	if f := awaitSubagentSummary(t, c); f["summary"] != "Reading the parser" {
		t.Errorf("cached answer = %v", f)
	}
	if n := len(stub.requests()); n != 1 {
		t.Fatalf("model calls after an unchanged ask = %d, want 1", n)
	}

	// New activity, but the last call started only 30 s ago -> allowed.
	h.subAct.note("ag/c7", "toolu_a", "tool", "Bash ls")
	stub.mu.Lock()
	stub.reply = "Listing the files"
	stub.mu.Unlock()
	ask()
	awaitSubagentSummary(t, c) // the cached one, at once
	if f := awaitSubagentSummary(t, c); f["summary"] != "Listing the files" {
		t.Errorf("fresh answer = %v", f)
	}
	if n := len(stub.requests()); n != 2 {
		t.Fatalf("model calls = %d, want 2", n)
	}

	// More activity within 10 s of that call: no new call, cached answer only.
	*now = now.Add(5 * time.Second)
	h.subAct.note("ag/c7", "toolu_a", "tool", "Bash pwd")
	ask()
	awaitSubagentSummary(t, c)
	expectNoFrame(t, c)
	if n := len(stub.requests()); n != 2 {
		t.Fatalf("model calls within the gap = %d, want 2", n)
	}
	*now = now.Add(6 * time.Second)
	ask()
	awaitSubagentSummary(t, c)
	awaitSubagentSummary(t, c)
	if n := len(stub.requests()); n != 3 {
		t.Fatalf("model calls after the gap = %d, want 3", n)
	}
}

// An id with no activity, an unknown conversation, an ended entry or a failed
// call gets no answer, so the app shows the entry as before.
func TestSubagentSummary_NothingToSay(t *testing.T) {
	h, stub, _ := subagentSummaryHub(t, "Doing a thing")
	c := fakeClient()
	h.handleSubagentSummarize(c, fap.SubagentSummarize{ConversationID: "c1", IDs: []string{"never-seen"}})
	h.subAct.note("ag/c7", "toolu_a", "text", "step")
	h.handleSubagentSummarize(c, fap.SubagentSummarize{ConversationID: "nope", IDs: []string{"toolu_a"}})
	h.subAct.note("ag/c7", "toolu_a", "end", "")
	h.handleSubagentSummarize(c, fap.SubagentSummarize{ConversationID: "c1", IDs: []string{"toolu_a"}})
	expectNoFrame(t, c)

	stub.mu.Lock()
	stub.status = 502
	stub.mu.Unlock()
	h.subAct.note("ag/c7", "toolu_b", "text", "step")
	h.handleSubagentSummarize(c, fap.SubagentSummarize{ConversationID: "c1", IDs: []string{"toolu_b"}})
	expectNoFrame(t, c)
	if n := len(stub.requests()); n != 1 {
		t.Errorf("model calls = %d, want 1 (the failed one)", n)
	}
}

// NoteSubagentActivity and the spawn observer feed the store of the running hub.
func TestSubagentSummary_Feeds(t *testing.T) {
	h, _, _ := subagentSummaryHub(t, "x")
	activeMu.Lock()
	prev := activeHub
	activeHub = h
	activeMu.Unlock()
	t.Cleanup(func() { activeMu.Lock(); activeHub = prev; activeMu.Unlock() })

	NoteSubagentActivity("ag/c7", "toolu_a", "tool", "Read x")
	SpawnObserver{}.SpawnTool("ag/c7", "spawn-1", "bash ls")
	SpawnObserver{}.SpawnText("ag/c7", "spawn-1", "done listing")
	p := h.subAct.plan("ag/c7", "spawn-1")
	if p.input != "Tool call: bash ls\nMessage: done listing" {
		t.Errorf("spawn input = %q", p.input)
	}
	if p := h.subAct.plan("ag/c7", "toolu_a"); p.input != "Tool call: Read x" {
		t.Errorf("subagent input = %q", p.input)
	}
	SpawnObserver{}.SpawnEnded("ag/c7", "spawn-1")
	if p := h.subAct.plan("ag/c7", "spawn-1"); p.entry != nil {
		t.Errorf("an ended spawn is still stored")
	}
}

// The store is bounded: past the cap the oldest-touched entry goes.
func TestSubagentSummary_StoreBounded(t *testing.T) {
	st := newSubagentActivityStore()
	base := time.Unix(0, 0)
	i := 0
	st.now = func() time.Time { return base.Add(time.Duration(i) * time.Second) }
	for i = 0; i <= subagentActivityMaxEntries; i++ {
		st.note("s", "g"+string(rune('A'+i%26))+strings.Repeat("x", i/26), "text", "t")
	}
	if len(st.entries) != subagentActivityMaxEntries {
		t.Errorf("entries = %d, want %d", len(st.entries), subagentActivityMaxEntries)
	}
	if _, ok := st.entries[activityKey("s", "gA")]; ok {
		t.Errorf("the oldest entry was kept")
	}
}

// An answer longer than 1.5 times the six words asked for (more than nine) is
// not used, and never cut (Dick, 2026-11-08; the shared check, #2962). Nine
// words still pass.
func TestSubagentSummary_TooLongNotUsed(t *testing.T) {
	h, stub, now := subagentSummaryHub(t, "one two three four five six seven eight nine ten")
	h.subAct.note("ag/c7", "toolu_a", "text", "step")
	c := fakeClient()
	ask := func() {
		h.handleSubagentSummarize(c, fap.SubagentSummarize{ConversationID: "c1", IDs: []string{"toolu_a"}})
	}
	ask()
	expectNoFrame(t, c)

	stub.mu.Lock()
	stub.reply = "one two three four five six seven eight nine"
	stub.mu.Unlock()
	*now = now.Add(subagentSummaryMinGap)
	ask()
	if f := awaitSubagentSummary(t, c); f["summary"] != "one two three four five six seven eight nine" {
		t.Errorf("nine-word answer = %v, want it whole", f)
	}
}

package app

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/fap"
	"foci/internal/session"
)

// Tests for #2993: each app chat's sections, found by the fast model as the
// chat goes.

// sectionHub is autoNameHub with the sectioner set from app.
func sectionHub(t *testing.T, stub *nameStub, app *config.AppSpecific) (*Hub, *session.SessionIndex, *convBinding) {
	t.Helper()
	h, idx, b := autoNameHub(t, stub, app)
	h.sections = newSectioner(app)
	return h, idx, b
}

func storedSections(t *testing.T, idx *session.SessionIndex, b *convBinding) chatSections {
	t.Helper()
	raw := chatMeta(t, idx, b, chatMetaSections)
	if raw == "" {
		return chatSections{}
	}
	var st chatSections
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("stored sections %q: %v", raw, err)
	}
	return st
}

// TestSections_OnlyNewMessages_RunEveryFive: a chat with history is sectioned
// from the first message this code sees (Q49); no call until five messages
// from there; the fifth runs one call whose input holds only those messages,
// numbered M1.., with the instructions after the conversation; the title is
// stored and served by sections.get.
func TestSections_OnlyNewMessages_RunEveryFive(t *testing.T) {
	stub := &nameStub{reply: "TITLE: Booking the Athens flights\nNEW: none"}
	h, idx, b := sectionHub(t, stub, nil)

	seq := seedChat(t, h, 0, 20, "old history") // before the feature saw the chat
	seq = seedChat(t, h, seq, 1, "athens")      // m21: the first message seen
	h.sectionCheck(b)
	st := storedSections(t, idx, b)
	if len(st.Sections) != 1 || st.Sections[0].MessageID != "m21" || st.ReadSeq != 20 {
		t.Fatalf("after first sight: %+v, want one section at m21, readSeq 20", st)
	}
	seq = seedChat(t, h, seq, 3, "athens") // m22..m24: four messages so far
	h.sectionCheck(b)
	if n := stub.count(); n != 0 {
		t.Fatalf("calls at 4 messages = %d, want 0", n)
	}
	_ = seedChat(t, h, seq, 1, "athens") // m25: five
	h.sectionCheck(b)
	if n := stub.count(); n != 1 {
		t.Fatalf("calls at 5 messages = %d, want 1", n)
	}
	in := stub.lastInput()
	if strings.Contains(in, "old history") {
		t.Errorf("input holds messages from before the feature saw the chat:\n%s", in)
	}
	for _, want := range []string{"[M1] User: athens 21", "[M5] User: athens 25"} {
		if !strings.Contains(in, want) {
			t.Errorf("input lacks %q:\n%s", want, in)
		}
	}
	if strings.Contains(in, "[M6]") {
		t.Errorf("input numbers more than five messages:\n%s", in)
	}
	if i, j := strings.Index(in, "END OF CONVERSATION"), strings.Index(in, "NEW: <none"); i < 0 || j < i {
		t.Errorf("instructions not after the conversation:\n%s", in)
	}

	st = storedSections(t, idx, b)
	if len(st.Sections) != 1 || st.Sections[0].Title != "Booking the Athens flights" || st.ReadSeq != 25 || st.Broke {
		t.Fatalf("after run: %+v", st)
	}
	got := h.sectionsFor("r1", autoNameConv)
	if got.RequestID != "r1" || len(got.Sections) != 1 || got.Sections[0].MessageID != "m21" || got.Sections[0].Title != "Booking the Athens flights" || got.Sections[0].StartMs == 0 {
		t.Fatalf("sections.get answer = %+v", got)
	}
}

// TestSections_BreakMapsCompactIDAndWaitsTen: an answer naming M3 starts a
// new section at the third shown message (its real id and time), retitles
// the old one, and the next run waits for ten messages, not five; that run
// reads the new section only.
func TestSections_BreakMapsCompactIDAndWaitsTen(t *testing.T) {
	stub := &nameStub{reply: "TITLE: Flights\nNEW: M3 | Hotel in Plaka"}
	h, idx, b := sectionHub(t, stub, nil)

	seq := seedChat(t, h, 0, 1, "trip")
	h.sectionCheck(b)
	seq = seedChat(t, h, seq, 4, "trip")
	h.sectionCheck(b)
	st := storedSections(t, idx, b)
	if len(st.Sections) != 2 || st.Sections[0].Title != "Flights" || st.Sections[1].MessageID != "m3" ||
		st.Sections[1].StartSeq != 3 || st.Sections[1].Title != "Hotel in Plaka" || !st.Broke {
		t.Fatalf("after break: %+v", st)
	}

	stub.set("TITLE: Hotel in Plaka and taxis\nNEW: none", 0)
	seq = seedChat(t, h, seq, 9, "hotel")
	h.sectionCheck(b)
	if n := stub.count(); n != 1 {
		t.Fatalf("calls 9 messages after a break = %d, want 1 (no new run)", n)
	}
	_ = seedChat(t, h, seq, 1, "hotel")
	h.sectionCheck(b)
	if n := stub.count(); n != 2 {
		t.Fatalf("calls 10 messages after a break = %d, want 2", n)
	}
	in := stub.lastInput()
	if strings.Contains(in, "trip 1") || strings.Contains(in, "reply 2") || !strings.Contains(in, "[M1] User: trip 3") {
		t.Errorf("run after the break does not start at the new section (m3):\n%s", in)
	}
	st = storedSections(t, idx, b)
	if len(st.Sections) != 2 || st.Sections[1].Title != "Hotel in Plaka and taxis" || st.Broke {
		t.Fatalf("after second run: %+v", st)
	}
}

// TestSections_BadAnswerChangesNothing: an unparseable answer, an id outside
// the shown range, and an HTTP failure each leave the stored sections as they
// were (req 9) and start the retry gap.
func TestSections_BadAnswerChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  string
		status int
	}{
		{"prose", "This chat is about flights.", 0},
		{"id out of range", "TITLE: Flights\nNEW: M9 | Hotels", 0},
		{"title too long", "TITLE: one two three four five six seven eight nine ten eleven twelve thirteen\nNEW: none", 0},
		{"http error", "", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &nameStub{reply: tc.reply, status: tc.status}
			h, idx, b := sectionHub(t, stub, nil)
			seq := seedChat(t, h, 0, 1, "trip")
			h.sectionCheck(b)
			before := chatMeta(t, idx, b, chatMetaSections)
			_ = seedChat(t, h, seq, 4, "trip")
			h.sectionCheck(b)
			if n := stub.count(); n != 1 {
				t.Fatalf("calls = %d, want 1", n)
			}
			if after := chatMeta(t, idx, b, chatMetaSections); after != before {
				t.Errorf("stored sections changed:\nbefore %s\nafter  %s", before, after)
			}
			if got := h.sectionsFor("r", autoNameConv); len(got.Sections) != 0 {
				t.Errorf("served sections = %+v, want none", got.Sections)
			}
			// The retry gap holds the next run off.
			seedChat(t, h, seq+4, 5, "trip")
			h.sectionCheck(b)
			if n := stub.count(); n != 1 {
				t.Errorf("calls during the retry gap = %d, want 1", n)
			}
		})
	}
}

// TestSections_Off: chat_sections = false makes no call and stores nothing.
func TestSections_Off(t *testing.T) {
	off := false
	stub := &nameStub{reply: "TITLE: x\nNEW: none"}
	h, idx, b := sectionHub(t, stub, &config.AppSpecific{ChatSections: &off})
	seedChat(t, h, 0, 12, "trip")
	h.sectionCheck(b)
	if n := stub.count(); n != 0 {
		t.Errorf("calls = %d, want 0", n)
	}
	if v := chatMeta(t, idx, b, chatMetaSections); v != "" {
		t.Errorf("stored = %q, want nothing", v)
	}
}

func TestParseSectionAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		wantTitle    string
		wantAt       int
		wantNewTitle string
		wantErr      bool
	}{
		{name: "none", answer: "TITLE: Fixing the build\nNEW: none", wantTitle: "Fixing the build"},
		{name: "break", answer: "TITLE: Fixing the build\nNEW: M4 | Deploying it", wantTitle: "Fixing the build", wantAt: 4, wantNewTitle: "Deploying it"},
		{name: "markdown and quotes", answer: "**TITLE:** \"Fixing the build.\"\n**NEW:** [M4]: Deploying it", wantTitle: "Fixing the build", wantAt: 4, wantNewTitle: "Deploying it"},
		{name: "lower case keys", answer: "title: Fixing the build\nnew: m2 - Deploying it", wantTitle: "Fixing the build", wantAt: 2, wantNewTitle: "Deploying it"},
		{name: "no NEW line reads as none", answer: "TITLE: Fixing the build", wantTitle: "Fixing the build"},
		{name: "M1 is no break", answer: "TITLE: Fixing the build\nNEW: M1 | Same thing", wantTitle: "Fixing the build"},
		{name: "no title", answer: "NEW: M3 | Deploying", wantErr: true},
		{name: "unlabelled title (seen live)", answer: "Deploy Failed with Permission Error\n\nNEW: M5 | Cooking lamb", wantTitle: "Deploy Failed with Permission Error", wantAt: 5, wantNewTitle: "Cooking lamb"},
		{name: "prose", answer: "This chat is about flights.", wantErr: true},
		{name: "id past the shown", answer: "TITLE: a\nNEW: M6 | b", wantErr: true},
		{name: "M0", answer: "TITLE: a\nNEW: M0 | b", wantErr: true},
		{name: "new with no title", answer: "TITLE: a\nNEW: M3", wantErr: true},
		{name: "new names no message", answer: "TITLE: a\nNEW: yes, the deploy", wantErr: true},
		{name: "new title too long", answer: "TITLE: a\nNEW: M3 | one two three four five six seven eight nine ten eleven twelve thirteen", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSectionAnswer(tc.answer, 5, sectionTitleWords)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if got.title != tc.wantTitle || got.newAt != tc.wantAt || got.newTitle != tc.wantNewTitle {
				t.Errorf("got %+v, want title %q at %d new %q", got, tc.wantTitle, tc.wantAt, tc.wantNewTitle)
			}
		})
	}
}

// TestSectionInput_HeadCutThenNewestTen: a long section shows its start cut
// to 5,000 characters of text, a gap mark, then its newest ten messages; the
// numbering runs on across both, and shown maps each number to its message.
func TestSectionInput_HeadCutThenNewestTen(t *testing.T) {
	var all []chatMessage
	for i := 1; i <= 40; i++ {
		all = append(all, chatMessage{seq: int64(i), id: "m" + strconv.Itoa(i), user: i%2 == 1, text: strings.Repeat("w", 999) + strconv.Itoa(i%10)})
	}
	in, shown := sectionInput(all, all[30:])
	// 1,000 runes each: m1..m5 fill the 5,000 exactly; then m31..m40.
	if len(shown) != 15 || shown[4].id != "m5" || shown[5].id != "m31" || shown[14].id != "m40" {
		ids := make([]string, len(shown))
		for i, m := range shown {
			ids[i] = m.id
		}
		t.Fatalf("shown = %v", ids)
	}
	if !strings.Contains(in, "[... messages left out ...]") || !strings.Contains(in, "[M6] User: ") || !strings.Contains(in, "M1 to M15") {
		t.Errorf("input lacks the gap mark, M6 or the range:\n%.400s", in)
	}

	// A message crossing the 5,000 mark is cut and marked.
	long := []chatMessage{{seq: 1, id: "a", user: true, text: strings.Repeat("x", 6000)}, {seq: 2, id: "b", text: "reply"}}
	in, shown = sectionInput(long, long)
	if len(shown) != 2 || !strings.Contains(in, strings.Repeat("x", 5000)+" [...]") || strings.Contains(in, strings.Repeat("x", 5001)) {
		t.Errorf("cut head: shown %d, input %.200s", len(shown), in)
	}
	if strings.Contains(in, "left out") {
		t.Errorf("gap marked where none is:\n%.200s", in)
	}

	// A short section: head and tail overlap; nothing repeats.
	short := all[:4]
	in, shown = sectionInput(short, short)
	if len(shown) != 4 || strings.Count(in, "[M") != 4 || strings.Contains(in, "left out") {
		t.Errorf("short section: shown %d, input:\n%.300s", len(shown), in)
	}
}

// TestSections_LiveSendPath drives the real send path through the hook
// ensureBinding installs (chatMessageStored): the fifth stored chat message
// after the first runs a sections call, and the title is stored. Auto naming
// is off so every model call is the sections'.
func TestSections_LiveSendPath(t *testing.T) {
	off := false
	stub := &nameStub{reply: "TITLE: Live section\nNEW: none"}
	h, idx, b := sectionHub(t, stub, &config.AppSpecific{AutoName: &off})
	b.onMessageStored = h.chatMessageStored

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	b.send(fap.ServerMessage{ConversationID: autoNameConv, MessageID: "u0", Role: "user", Text: "hello"})
	waitFor("the first section's start", func() bool { return len(storedSections(t, idx, b).Sections) == 1 })
	for i := 1; i < 5; i++ {
		b.send(fap.ServerMessage{ConversationID: autoNameConv, MessageID: "u" + strconv.Itoa(i), Role: "user", Text: "hello"})
	}
	waitFor("the titled section", func() bool {
		st := storedSections(t, idx, b)
		return len(st.Sections) == 1 && st.Sections[0].Title == "Live section"
	})
	if got := storedSections(t, idx, b).Sections[0].MessageID; got != "u0" {
		t.Errorf("first section starts at %q, want u0", got)
	}
}

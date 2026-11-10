package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/session"
)

// Tests for #2962: open app chats without a name set on purpose are named
// from their conversation by the fast summary model.

const autoNameConv = "conv-auto"

// nameStub is a chat-completions endpoint whose answer each test sets per
// request. onCall, when set, runs inside the request before it is answered.
type nameStub struct {
	mu     sync.Mutex
	inputs []string // the user message of each request
	system []string
	reply  string
	status int
	onCall func()
	calls  chan struct{}
}

func (s *nameStub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	s.calls = make(chan struct{}, 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || len(body.Messages) != 2 {
			t.Errorf("stub: bad request body %q: %v", raw, err)
			return
		}
		s.mu.Lock()
		s.system = append(s.system, body.Messages[0].Content)
		s.inputs = append(s.inputs, body.Messages[1].Content)
		reply, status, onCall := s.reply, s.status, s.onCall
		s.mu.Unlock()
		if onCall != nil {
			onCall()
		}
		defer func() { s.calls <- struct{}{} }()
		if status != 0 && status != http.StatusOK {
			http.Error(w, `{"error":{"message":"nope"}}`, status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gen-1", "model": "meta-llama/llama-3.1-8b-instruct",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": reply}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 500, "completion_tokens": 6},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *nameStub) set(reply string, status int) {
	s.mu.Lock()
	s.reply, s.status = reply, status
	s.mu.Unlock()
}

func (s *nameStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inputs)
}

func (s *nameStub) lastInput() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.inputs) == 0 {
		return ""
	}
	return s.inputs[len(s.inputs)-1]
}

// autoNameHub is a hub with a session index, a frame store, a summariser
// pointed at stub, the auto-namer with app's settings, and one open chat.
func autoNameHub(t *testing.T, stub *nameStub, app *config.AppSpecific) (*Hub, *session.SessionIndex, *convBinding) {
	t.Helper()
	srv := stub.serve(t)
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.frames = tempFrameStore(t)
	h.pinSum = testSummariser(srv.URL, nil)
	h.autoName = newAutoNamer(app)
	b := withIdent(&convBinding{convID: autoNameConv, chatID: chatIDForConv(autoNameConv), store: h.frames},
		"clutch", session.NewChatSessionKey("clutch", chatIDForConv(autoNameConv)))
	h.convs[autoNameConv] = b
	h.storeOpenChats([]string{autoNameConv})
	return h, idx, b
}

// seedChat stores n chat messages after seq from (alternating user and
// agent; the agent's as a streamed reply's end) and returns the last seq.
func seedChat(t *testing.T, h *Hub, from int64, n int, text string) int64 {
	t.Helper()
	seq := from
	for i := 0; i < n; i++ {
		seq++
		var frame fap.ServerFrame
		if seq%2 == 1 {
			frame = fap.ServerMessage{ConversationID: autoNameConv, MessageID: "m" + strconv.FormatInt(seq, 10), Role: "user", Text: text + " " + strconv.FormatInt(seq, 10)}
		} else {
			final := text + " reply " + strconv.FormatInt(seq, 10)
			frame = fap.TextEnd{ConversationID: autoNameConv, TurnID: "t", MessageID: "m" + strconv.FormatInt(seq, 10), FinalText: &final}
		}
		w, err := fap.Encode(frame, seq, 0, "", "")
		if err != nil {
			t.Fatal(err)
		}
		h.frames.insert(frameWrite{convID: autoNameConv, agentID: "clutch", seq: seq, wire: w, sentMs: time.Now().UnixMilli(), visible: true, preview: "x"})
	}
	return seq
}

func chatMeta(t *testing.T, idx *session.SessionIndex, b *convBinding, key string) string {
	t.Helper()
	v, err := idx.GetChatMetadata("clutch", "app", b.chatID, key)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestAutoName_FirstNameAfterTenMessages: no call at nine messages; at ten
// the chat is named, marked as the auto-namer's own, and the prompt holds the
// conversation first and the instruction (six words) at its end.
func TestAutoName_FirstNameAfterTenMessages(t *testing.T) {
	stub := &nameStub{reply: "Planning the Athens family trip"}
	h, idx, b := autoNameHub(t, stub, nil)

	seq := seedChat(t, h, 0, 9, "athens flights")
	h.autoNameCheck(b)
	if n := stub.count(); n != 0 {
		t.Fatalf("calls at 9 messages = %d, want 0", n)
	}
	seq = seedChat(t, h, seq, 1, "athens flights")
	h.autoNameCheck(b)
	if n := stub.count(); n != 1 {
		t.Fatalf("calls at 10 messages = %d, want 1", n)
	}
	if got := chatMeta(t, idx, b, "alias"); got != "Planning the Athens family trip" {
		t.Errorf("alias = %q", got)
	}
	if got := chatMeta(t, idx, b, chatMetaAliasSummary); got != "Planning the Athens family trip" {
		t.Errorf("alias_summary = %q", got)
	}
	if got := chatMeta(t, idx, b, "alias_auto"); got != "1" {
		t.Errorf("alias_auto = %q, want 1 (an agent may still rename)", got)
	}
	if got := chatMeta(t, idx, b, chatMetaAliasSummarySeq); got != strconv.FormatInt(seq, 10) {
		t.Errorf("alias_summary_seq = %q, want %d", got, seq)
	}
	in := stub.lastInput()
	conv := strings.Index(in, "User: athens flights 1")
	instr := strings.Index(in, "AT MOST 6 WORDS")
	if conv < 0 || instr < 0 || instr < conv {
		t.Errorf("input must hold the conversation, then the six-word instruction:\n%s", in)
	}
	if !strings.Contains(in, "Agent: athens flights reply 10") {
		t.Errorf("input lacks the streamed agent reply:\n%s", in)
	}
	if strings.Contains(in, "current title") {
		t.Errorf("a first naming must not mention a previous title:\n%s", in)
	}
}

// TestAutoName_NameSetOnPurposeNeverReplaced: a user's name, and an agent's
// set_session_alias name, are not replaced however many messages follow;
// the fail-arm chat with no name is named.
func TestAutoName_NameSetOnPurposeNeverReplaced(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]string
	}{
		{"user", map[string]string{"alias": "My own name"}},
		{"agent", map[string]string{"alias": "Agent picked", "alias_auto": "1"}},
		{"user renamed an auto name", map[string]string{"alias": "Renamed", "alias_summary": "Old auto name", "alias_auto": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &nameStub{reply: "Something else entirely"}
			h, idx, b := autoNameHub(t, stub, nil)
			for k, v := range tc.meta {
				if err := idx.SetChatMetadata("clutch", "app", b.chatID, k, v); err != nil {
					t.Fatal(err)
				}
			}
			seedChat(t, h, 0, 120, "chat")
			h.autoNameCheck(b)
			if n := stub.count(); n != 0 {
				t.Errorf("calls = %d, want 0", n)
			}
			if got := chatMeta(t, idx, b, "alias"); got != tc.meta["alias"] {
				t.Errorf("alias = %q, want %q kept", got, tc.meta["alias"])
			}
		})
	}
	t.Run("fail-arm: no name", func(t *testing.T) {
		stub := &nameStub{reply: "Something else entirely"}
		h, idx, b := autoNameHub(t, stub, nil)
		seedChat(t, h, 0, 120, "chat")
		h.autoNameCheck(b)
		if got := chatMeta(t, idx, b, "alias"); got != "Something else entirely" {
			t.Errorf("alias = %q, want the automatic name", got)
		}
	})
}

// TestAutoName_RecheckEveryFifty: after the first name, nothing until 50 new
// messages; then the prompt names the current title; an answer equal to it
// keeps the name and moves the mark, a different answer renames.
func TestAutoName_RecheckEveryFifty(t *testing.T) {
	stub := &nameStub{reply: "Athens trip"}
	h, idx, b := autoNameHub(t, stub, nil)
	seq := seedChat(t, h, 0, 10, "athens")
	h.autoNameCheck(b)
	if got := chatMeta(t, idx, b, "alias"); got != "Athens trip" {
		t.Fatalf("first name = %q", got)
	}

	seq = seedChat(t, h, seq, 49, "athens")
	h.autoNameCheck(b)
	if n := stub.count(); n != 1 {
		t.Fatalf("calls after 49 new messages = %d, want 1", n)
	}
	seq = seedChat(t, h, seq, 1, "athens")
	stub.set("athens Trip", 0) // the same title, other case
	h.autoNameCheck(b)
	if n := stub.count(); n != 2 {
		t.Fatalf("calls after 50 new messages = %d, want 2", n)
	}
	in := stub.lastInput()
	if !strings.Contains(in, "current title is: Athens trip") || strings.Index(in, "current title") < strings.Index(in, "END OF CONVERSATION") {
		t.Errorf("re-check input must end with the previous title:\n%s", in)
	}
	if strings.Contains(in, "athens reply 10") || !strings.Contains(in, "User: athens 11") {
		t.Errorf("re-check input must hold only the messages since the last naming:\n%s", in)
	}
	if got := chatMeta(t, idx, b, "alias"); got != "Athens trip" {
		t.Errorf("alias = %q, want kept", got)
	}
	if got := chatMeta(t, idx, b, chatMetaAliasSummarySeq); got != strconv.FormatInt(seq, 10) {
		t.Errorf("mark = %q, want moved to %d", got, seq)
	}

	seq = seedChat(t, h, seq, 50, "tax")
	stub.set("Filing the 2026 tax return", 0)
	h.autoNameCheck(b)
	if got := chatMeta(t, idx, b, "alias"); got != "Filing the 2026 tax return" {
		t.Errorf("alias = %q, want renamed", got)
	}
	if got := chatMeta(t, idx, b, chatMetaAliasSummary); got != "Filing the 2026 tax return" {
		t.Errorf("alias_summary = %q", got)
	}
	_ = seq
}

// TestAutoName_ClosedChatSkipped: a chat not in the open-set is not named.
func TestAutoName_ClosedChatSkipped(t *testing.T) {
	stub := &nameStub{reply: "Name"}
	h, idx, b := autoNameHub(t, stub, nil)
	h.storeOpenChats([]string{"another"})
	seedChat(t, h, 0, 20, "x")
	h.autoNameCheck(b)
	if n := stub.count(); n != 0 || chatMeta(t, idx, b, "alias") != "" {
		t.Errorf("closed chat: calls = %d, alias = %q; want none", n, chatMeta(t, idx, b, "alias"))
	}
}

// TestAutoName_FailureChangesNothing: an HTTP error and an over-long answer
// leave the chat unnamed, and the chat is not tried again until the retry gap
// has passed.
func TestAutoName_FailureChangesNothing(t *testing.T) {
	stub := &nameStub{status: http.StatusInternalServerError}
	h, idx, b := autoNameHub(t, stub, nil)
	now := time.Unix(1_800_000_000, 0)
	h.autoName.now = func() time.Time { return now }
	seedChat(t, h, 0, 10, "x")

	h.autoNameCheck(b)
	if n := stub.count(); n != 1 || chatMeta(t, idx, b, "alias") != "" {
		t.Fatalf("after an HTTP error: calls = %d, alias = %q", n, chatMeta(t, idx, b, "alias"))
	}
	h.autoNameCheck(b)
	if n := stub.count(); n != 1 {
		t.Fatalf("inside the retry gap: calls = %d, want 1", n)
	}

	now = now.Add(autoNameRetryGap)
	stub.set("one two three four five six seven eight nine ten", 0) // 10 > 1.5 x 6
	h.autoNameCheck(b)
	if n := stub.count(); n != 2 {
		t.Fatalf("after the gap: calls = %d, want 2", n)
	}
	if got := chatMeta(t, idx, b, "alias"); got != "" {
		t.Errorf("an answer over 1.5x the words was used: alias = %q", got)
	}

	now = now.Add(autoNameRetryGap)
	stub.set("one two three four five six seven eight nine", 0) // 9 = 1.5 x 6: used, not cut
	h.autoNameCheck(b)
	if got := chatMeta(t, idx, b, "alias"); got != "one two three four five six seven eight nine" {
		t.Errorf("alias = %q, want the nine-word answer whole", got)
	}
}

// TestAutoName_RenameDuringCallKept: a rename the user makes while the model
// call runs is not overwritten.
func TestAutoName_RenameDuringCallKept(t *testing.T) {
	stub := &nameStub{reply: "Automatic"}
	h, idx, b := autoNameHub(t, stub, nil)
	stub.onCall = func() {
		if err := idx.SetChatAliasUnique("clutch", "app", b.chatID, "Picked by hand"); err != nil {
			t.Error(err)
		}
	}
	seedChat(t, h, 0, 10, "x")
	h.autoNameCheck(b)
	if n := stub.count(); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	if got := chatMeta(t, idx, b, "alias"); got != "Picked by hand" {
		t.Errorf("alias = %q, want the user's rename kept", got)
	}
	if got := chatMeta(t, idx, b, chatMetaAliasSummary); got != "" {
		t.Errorf("alias_summary = %q, want unset", got)
	}
}

// TestAutoName_Settings: the switch, the counts and the word limit come from
// config, and a live configure applies to the next check; a missing key or
// the model set to off stops the feature with it on.
func TestAutoName_Settings(t *testing.T) {
	off := false
	stub := &nameStub{reply: "Name"}
	h, idx, b := autoNameHub(t, stub, &config.AppSpecific{AutoName: &off})
	seedChat(t, h, 0, 12, "x")
	h.autoNameCheck(b)
	if n := stub.count(); n != 0 {
		t.Fatalf("auto_name=false: calls = %d, want 0", n)
	}

	on, first, words := true, 13, 3
	h.autoName.configure(&config.AppSpecific{AutoName: &on, AutoNameFirst: &first, AutoNameWords: &words})
	h.autoNameCheck(b)
	if n := stub.count(); n != 0 {
		t.Fatalf("auto_name_first=13 at 12 messages: calls = %d, want 0", n)
	}
	seedChat(t, h, 12, 1, "x")
	h.autoNameCheck(b)
	if n := stub.count(); n != 1 {
		t.Fatalf("auto_name_first=13 at 13 messages: calls = %d, want 1", n)
	}
	if !strings.Contains(stub.lastInput(), "AT MOST 3 WORDS") {
		t.Errorf("auto_name_words=3 not in the prompt:\n%s", stub.lastInput())
	}
	if chatMeta(t, idx, b, "alias") != "Name" {
		t.Errorf("alias = %q", chatMeta(t, idx, b, "alias"))
	}

	// Unusable summariser: no key, or the model off.
	for name, sum := range map[string]*pinSummariser{
		"no key":           newPinSummariser(nil, nil, func(string) (string, bool) { return "", false }),
		"model off":        newPinSummariser(&config.AppSpecific{PinSummaryModel: "off"}, nil, func(string) (string, bool) { return "k", true }),
		"name model off":   newPinSummariser(&config.AppSpecific{AutoNameModel: "off"}, nil, func(string) (string, bool) { return "k", true }),
		"unknown endpoint": newPinSummariser(&config.AppSpecific{AutoNameEndpoint: "nowhere"}, nil, func(string) (string, bool) { return "k", true }),
	} {
		stub2 := &nameStub{reply: "Name"}
		h2, _, b2 := autoNameHub(t, stub2, nil)
		sum.openRouterURL = h2.pinSum.openRouterURL
		h2.pinSum = sum
		seedChat(t, h2, 0, 20, "x")
		h2.autoNameCheck(b2)
		if n := stub2.count(); n != 0 {
			t.Errorf("%s: calls = %d, want 0", name, n)
		}
	}
}

// TestAutoName_LiveSendPath drives the real send path: system notices and
// streaming frames do not count, the hook fires after each stored chat
// message, and the tenth names the chat. It also pins that the live filter
// (autoNameCounts) and the store's (chatMessageWhere) agree.
func TestAutoName_LiveSendPath(t *testing.T) {
	stub := &nameStub{reply: "Live named chat"}
	h, idx, b := autoNameHub(t, stub, nil)
	b.onMessageStored = h.autoNameCheck

	counted := 0
	send := func(f fap.ServerFrame) {
		if autoNameCounts(f) {
			counted++
		}
		b.send(f)
	}
	for i := 0; i < 9; i++ {
		send(fap.ServerMessage{ConversationID: autoNameConv, MessageID: "u" + strconv.Itoa(i), Role: "user", Text: "hello"})
		send(fap.ServerMessage{ConversationID: autoNameConv, MessageID: "s" + strconv.Itoa(i), Role: "system", Text: "notice"})
		send(fap.TextDelta{ConversationID: autoNameConv, TurnID: "t", Text: "partial"})
	}
	if counted != 9 {
		t.Fatalf("autoNameCounts accepted %d frames, want 9", counted)
	}
	final := "the tenth"
	send(fap.TextEnd{ConversationID: autoNameConv, TurnID: "t", MessageID: "a9", FinalText: &final})

	select {
	case <-stub.calls:
	case <-time.After(20 * time.Second):
		t.Fatal("no model call after the tenth stored message")
	}
	deadline := time.Now().Add(20 * time.Second)
	for chatMeta(t, idx, b, "alias") != "Live named chat" {
		if time.Now().After(deadline) {
			t.Fatalf("alias = %q, want the live name", chatMeta(t, idx, b, "alias"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := h.frames.CountMessagesAfter(autoNameConv, 0, 1000); n != counted {
		t.Errorf("store counts %d chat messages, live filter %d", n, counted)
	}
	if n := stub.count(); n != 1 {
		t.Errorf("calls = %d, want exactly 1", n)
	}
}

// TestSummaryWordsOK: up to 1.5 times the asked-for words is accepted, more
// is not (Dick 2026-11-08).
func TestSummaryWordsOK(t *testing.T) {
	words := func(n int) string { return strings.TrimSpace(strings.Repeat("w ", n)) }
	for _, tc := range []struct {
		max, n int
		ok     bool
	}{
		{6, 6, true}, {6, 9, true}, {6, 10, false},
		{10, 15, true}, {10, 16, false},
		{5, 7, true}, {5, 8, false},
		{0, 100, true},
	} {
		if got := summaryWordsOK(words(tc.n), tc.max); got != tc.ok {
			t.Errorf("summaryWordsOK(%d words, max %d) = %v, want %v", tc.n, tc.max, got, tc.ok)
		}
	}
}

// TestPinSummary_TooLongNotUsed: the shared length check applies to the pin
// label too: 16 words against its ten is an error, 15 is used whole.
func TestPinSummary_TooLongNotUsed(t *testing.T) {
	stub := &openRouterStub{reply: strings.TrimSpace(strings.Repeat("w ", 16))}
	srv := stub.serve(t)
	s := testSummariser(srv.URL, nil)
	if got, err := s.summarise(t.Context(), "text", "", ""); err == nil {
		t.Errorf("16 words: summary = %q, want an error", got)
	}
	stub.mu.Lock()
	stub.reply = strings.TrimSpace(strings.Repeat("w ", 15))
	stub.mu.Unlock()
	got, err := s.summarise(t.Context(), "text", "", "")
	if err != nil || len(strings.Fields(got)) != 15 {
		t.Errorf("15 words: summary = %q, %v; want all 15", got, err)
	}
}

// TestAutoNameInput_KeepsNewest: an input over the cap drops the oldest
// messages, and the instruction is always last.
func TestAutoNameInput_KeepsNewest(t *testing.T) {
	big := strings.Repeat("a", 3000) // 50 of these are over autoNameInputRunes
	var msgs []chatMessage
	for i := 0; i < 50; i++ {
		msgs = append(msgs, chatMessage{seq: int64(i + 1), user: i%2 == 0, text: big + " " + strconv.Itoa(i)})
	}
	msgs[49].text = "the newest"
	in := autoNameInput(msgs, "", 6)
	if !strings.Contains(in, "the newest") {
		t.Error("newest message dropped")
	}
	if strings.Contains(in, " 0\n") {
		t.Error("oldest message kept over the cap")
	}
	if !strings.HasSuffix(in, "one line.") {
		t.Errorf("input must end with the instruction: ...%s", in[len(in)-200:])
	}
}

// TestAutoNamer_MessageDuringCheckRunsAgain: a message stored while a check
// of the chat runs is not dropped: the check ends asking to run again.
func TestAutoNamer_MessageDuringCheckRunsAgain(t *testing.T) {
	n := newAutoNamer(nil)
	if _, ok := n.begin("c"); !ok {
		t.Fatal("first begin refused")
	}
	if _, ok := n.begin("c"); ok {
		t.Fatal("second begin claimed a chat already being checked")
	}
	if !n.end("c", false) {
		t.Error("end = false, want true: a message came in during the check")
	}
	if _, ok := n.begin("c"); !ok {
		t.Fatal("begin after end refused")
	}
	if n.end("c", false) {
		t.Error("end = true with no message during the check")
	}
}

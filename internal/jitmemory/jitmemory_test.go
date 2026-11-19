package jitmemory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/memory"
	"foci/internal/memsearch"
)

func TestParseQueries_StripsPreambleAndMarkers(t *testing.T) {
	// The shape Llama 3.1 8B returned in the #3175 evidence.
	answer := "Here are three potential keyword search queries:\n\n1. \"Anna archive link\"\n2) Anna document repository\n- Sharepoint link Anna archive\n- a fourth query"
	got := ParseQueries(answer)
	want := []string{"Anna archive link", "Anna document repository", "Sharepoint link Anna archive"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("ParseQueries = %q, want %q", got, want)
	}
}

func TestParseQueries_DropsLongAndLabelLines(t *testing.T) {
	got := ParseQueries("Queries:\nthis line is far too long to be a keyword query at all really\ncodex steer")
	if len(got) != 1 || got[0] != "codex steer" {
		t.Fatalf("ParseQueries = %q", got)
	}
}

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		answer string
		n      int
		want   []int
	}{
		{"none", 3, nil},
		{"None.", 3, nil},
		{"2, 3", 3, []int{1, 2}},
		{"[1] and 7", 3, []int{0}},
		{"Relevant: 1", 2, []int{0}},
	}
	for _, c := range cases {
		got := ParseVerdict(c.answer, c.n)
		if len(got) != len(c.want) {
			t.Errorf("ParseVerdict(%q) = %v, want %v", c.answer, got, c.want)
			continue
		}
		for _, i := range c.want {
			if !got[i] {
				t.Errorf("ParseVerdict(%q) = %v, want %v", c.answer, got, c.want)
			}
		}
	}
}

const sampleNote = `# 2026-11-11

intro line

## Anna's Archive working domain

Working domain from the NUC: annas-archive.pk (14:50).
The .org one is dead.

### Detail

sub detail about the domain

## Other topic

unrelated text here
`

func writeNote(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeModel answers the query and judge calls and records them.
type fakeModel struct {
	mu       sync.Mutex
	queries  string
	verdict  string
	queryErr error
	judgeErr error
	judged   []string // the judge prompts
	calls    []string
}

func (f *fakeModel) call(_ context.Context, what, model, system, user string, _ int) (string, CallReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, what+"|"+model)
	if strings.Contains(what, "queries") {
		return f.queries, CallReport{Model: "m"}, f.queryErr
	}
	f.judged = append(f.judged, user)
	return f.verdict, CallReport{Model: "m", CostUSD: 0.001}, f.judgeErr
}

type fixture struct {
	dir     string
	model   *fakeModel
	engine  *Engine
	store   *Store
	own     *memsearch.Index
	results map[string][]memory.Result
	queries []string
}

// fakeBackend is a memory search backend answering from a func.
type fakeBackend func(q string) ([]memory.Result, error)

func (f fakeBackend) Search(q, _ string, _ *memory.SearchOptions) ([]memory.Result, error) {
	return f(q)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir(), model: &fakeModel{verdict: "1"}, results: map[string][]memory.Result{}}
	writeNote(t, f.dir, "a.md", sampleNote)
	writeNote(t, f.dir, "MEMORY.md", "## Lessons\nAnna's Archive is in the system prompt\n")
	store, err := OpenStore(filepath.Join(t.TempDir(), "jit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.store = store
	own, err := memsearch.OpenIndex(filepath.Join(t.TempDir(), "own.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = own.Close() })
	f.own = own
	ms := &memsearch.Searcher{
		Backends: map[string]memory.Searcher{"x": fakeBackend(func(q string) ([]memory.Result, error) {
			f.queries = append(f.queries, q)
			if r, ok := f.results[q]; ok {
				return r, nil
			}
			return f.results["*"], nil
		})},
		SourceDirs: func() map[string]string { return map[string]string{"clutch": f.dir} },
		Own:        own,
		Window:     func() int { return 3 },
	}
	f.engine = New(Deps{
		AgentID:           "clutch",
		Search:            ms.Search,
		SystemPromptFiles: func() []string { return []string{filepath.Join(f.dir, "MEMORY.md")} },
		Model:             f.model.call,
		Store:             func() *Store { return store },
	})
	return f
}

func settings() Settings {
	return Settings{Enabled: true, SkipChars: 10, ContextChars: 80, ContextMessages: 4, TopN: 5, Wait: time.Second,
		DedupeCompactions: 3, InjectMaxChars: 3000, SectionMaxChars: 1200, QueryStrategy: QueryBoth}
}

func annaHit() memory.Result {
	return memory.Result{Path: "a.md", Source: "clutch", Snippet: "Working domain from the >NUC<: annas-archive.pk"}
}

func TestRun_InjectsJudgedSectionAndLogs(t *testing.T) {
	f := newFixture(t)
	f.model.queries = "Here are the queries:\nAnna archive link"
	f.results["*"] = []memory.Result{
		annaHit(),
		// The asking session's own row and its branch's: dropped (#3185).
		{Path: "clutch/c1", Source: "conversation", Snippet: "own row", RowID: 7, Time: time.Now()},
		{Path: "clutch/c5/b1795000000", Source: "conversation", Snippet: "a branch row", RowID: 8, Time: time.Now()},
		{Path: "MEMORY.md", Source: "clutch", Snippet: ">Anna's< Archive is in the system prompt"},
	}
	out := f.engine.Run(context.Background(), Request{SessionKey: "clutch/c1", Role: RoleUser,
		Text: "Recent working link to Anna's archive?", Settings: settings()})
	if out.Err != nil || out.Injection == "" {
		t.Fatalf("outcome = %+v", out)
	}
	if !strings.HasPrefix(out.Injection, InjectionHeader) || !strings.Contains(out.Injection, "annas-archive.pk") {
		t.Fatalf("injection = %q", out.Injection)
	}
	// both: the raw message and the model's query were searched.
	if len(f.queries) != 2 || f.queries[0] != "Recent working link to Anna's archive?" || f.queries[1] != "Anna archive link" {
		t.Fatalf("queries searched = %q", f.queries)
	}
	// Only the a.md section went to the judge: no conversation hit, no
	// system-prompt file.
	judged := f.model.judged[0]
	if strings.Contains(judged, "MEMORY.md") || strings.Contains(judged, "[2]") {
		t.Fatalf("judge saw an excluded hit:\n%s", judged)
	}
	var hits, verdicts string
	if err := f.store.db.QueryRow(`SELECT hits, judge_answer FROM runs WHERE id = ?`, out.RunID).Scan(&hits, &verdicts); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"judged_relevant", "excluded", "own_conversation", "non_delivered"} {
		if !strings.Contains(hits, `"verdict":"`+v+`"`) {
			t.Fatalf("logged hits lack verdict %s: %s", v, hits)
		}
	}
	f.engine.SetDelivery(out.RunID, DeliveryAttached)
	var delivery string
	_ = f.store.db.QueryRow(`SELECT delivery FROM runs WHERE id = ?`, out.RunID).Scan(&delivery)
	if delivery != DeliveryAttached {
		t.Fatalf("delivery = %q", delivery)
	}
}

func TestRun_DedupeUntilNCompactions(t *testing.T) {
	f := newFixture(t)
	f.results["*"] = []memory.Result{annaHit()}
	s := settings()
	s.QueryStrategy = QueryRaw
	s.DedupeCompactions = 2
	run := func() Outcome {
		return f.engine.Run(context.Background(), Request{SessionKey: "clutch/c1", Role: RoleUser, Text: "Anna's archive link please", Settings: s})
	}
	if o := run(); o.Injection == "" {
		t.Fatalf("first run injected nothing: %+v", o)
	}
	if o := run(); o.Injection != "" || o.Skipped != "no new hits" {
		t.Fatalf("second run in the same session = %+v, want deduped", o)
	}
	f.engine.NoteCompaction("clutch/c1")
	if o := run(); o.Injection != "" {
		t.Fatalf("after 1 of 2 compactions = %+v, want deduped", o)
	}
	f.engine.NoteCompaction("clutch/c1")
	if o := run(); o.Injection == "" {
		t.Fatalf("after 2 compactions the hit should be judged again: %+v", o)
	}
	// Another session has its own set.
	if o := f.engine.Run(context.Background(), Request{SessionKey: "clutch/c2", Role: RoleUser, Text: "Anna's archive link please", Settings: s}); o.Injection == "" {
		t.Fatalf("other session deduped: %+v", o)
	}
	// The set survives a restart (a new engine on the same store).
	f2 := New(Deps{AgentID: "clutch", Search: f.engine.deps.Search,
		Model: f.model.call, Store: func() *Store { return f.store }})
	if o := f2.Run(context.Background(), Request{SessionKey: "clutch/c1", Role: RoleUser, Text: "Anna's archive link please", Settings: s}); o.Injection != "" {
		t.Fatalf("restarted engine re-offered a seen hit: %+v", o)
	}
}

func TestRun_SkipsShortMessageWithoutModelCall(t *testing.T) {
	f := newFixture(t)
	o := f.engine.Run(context.Background(), Request{SessionKey: "clutch/c1", Role: RoleUser, Text: "yes", Settings: settings()})
	if o.Skipped != "short" || len(f.model.calls) != 0 || len(f.queries) != 0 {
		t.Fatalf("outcome %+v calls %v queries %v", o, f.model.calls, f.queries)
	}
}

func TestRun_ShortMessageGetsContext(t *testing.T) {
	f := newFixture(t)
	f.model.queries = "anna archive"
	f.engine.Observe("clutch/c1", RoleAgent, "The Anna's Archive mirror moved to a new domain.")
	f.engine.Run(context.Background(), Request{SessionKey: "clutch/c1", Role: RoleUser, Text: "which one works now?", Settings: settings()})
	if len(f.model.calls) == 0 {
		t.Fatal("no model call")
	}
	// The query model saw the earlier agent message as context.
	f.results["*"] = []memory.Result{annaHit()}
	f.engine.Run(context.Background(), Request{SessionKey: "clutch/c1", Role: RoleUser, Text: "and the old one?", Settings: settings()})
	j := f.model.judged[len(f.model.judged)-1]
	if !strings.Contains(j, "Earlier messages") || !strings.Contains(j, "mirror moved") || !strings.Contains(j, "which one works now?") {
		t.Fatalf("judge prompt lacks the context:\n%s", j)
	}
}

func TestRun_LongMessageGetsNoContext(t *testing.T) {
	f := newFixture(t)
	f.results["*"] = []memory.Result{annaHit()}
	f.engine.Observe("clutch/c1", RoleAgent, "earlier message")
	f.engine.Run(context.Background(), Request{SessionKey: "clutch/c1", Role: RoleUser,
		Text: strings.Repeat("a long message about anna's archive domains ", 3), Settings: settings()})
	if strings.Contains(f.model.judged[0], "Earlier messages") {
		t.Fatal("a long message got context")
	}
}

func TestRun_QueryModelFailure(t *testing.T) {
	f := newFixture(t)
	f.results["*"] = []memory.Result{annaHit()}
	f.model.queryErr = errors.New("boom")
	// both: the raw query still runs.
	if o := f.engine.Run(context.Background(), Request{SessionKey: "clutch/c1", Role: RoleUser, Text: "Anna's archive link please", Settings: settings()}); o.Injection == "" {
		t.Fatalf("both strategy should survive a query-model failure: %+v", o)
	}
	// model: nothing, error reported, no retry.
	s := settings()
	s.QueryStrategy = QueryModel
	n := len(f.model.calls)
	o := f.engine.Run(context.Background(), Request{SessionKey: "clutch/c9", Role: RoleUser, Text: "Anna's archive link please", Settings: s})
	if o.Err == nil || o.Injection != "" || len(f.model.calls) != n+1 {
		t.Fatalf("model strategy failure = %+v, calls %d -> %d", o, n, len(f.model.calls))
	}
}

func TestRun_JudgeFailureInjectsNothing(t *testing.T) {
	f := newFixture(t)
	f.results["*"] = []memory.Result{annaHit()}
	f.model.judgeErr = errors.New("timeout")
	o := f.engine.Run(context.Background(), Request{SessionKey: "clutch/c1", Role: RoleUser, Text: "Anna's archive link please", Settings: settings()})
	if o.Err == nil || o.Injection != "" {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestRun_JudgeNoneInjectsNothing(t *testing.T) {
	f := newFixture(t)
	f.results["*"] = []memory.Result{annaHit()}
	f.model.verdict = "none"
	o := f.engine.Run(context.Background(), Request{SessionKey: "clutch/c1", Role: RoleUser, Text: "Anna's archive link please", Settings: settings()})
	if o.Injection != "" || o.Skipped != "none relevant" {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestRun_ModelsArePassedThrough(t *testing.T) {
	f := newFixture(t)
	f.results["*"] = []memory.Result{annaHit()}
	s := settings()
	s.QueryModel, s.JudgeModel = "q-model", "j-model"
	f.engine.Run(context.Background(), Request{SessionKey: "clutch/c1", Role: RoleUser, Text: "Anna's archive link please", Settings: s})
	if strings.Join(f.model.calls, ",") != "jit memory queries|q-model,jit memory judge|j-model" {
		t.Fatalf("calls = %v", f.model.calls)
	}
}

func TestFormatInjection_Cap(t *testing.T) {
	hits := []*Hit{{Path: "a.md", Section: "A", text: strings.Repeat("x", 500)}, {Path: "b.md", Section: "B", text: strings.Repeat("y", 500)}}
	got := FormatInjection(hits, len([]rune(InjectionHeader))+600)
	if !strings.Contains(got, "a.md") || strings.Contains(got, "b.md") {
		t.Fatalf("cap kept the wrong sections: %q", got)
	}
	one := FormatInjection(hits[:1], len([]rune(InjectionHeader))+100)
	if n := len([]rune(one)); n > len([]rune(InjectionHeader))+102 {
		t.Fatalf("first section not cut to the cap: %d runes", n)
	}
}

func TestSaysNone(t *testing.T) {
	for _, a := range []string{"NONE", "none", " None.\n", "\"NONE\""} {
		if !SaysNone(a) {
			t.Errorf("SaysNone(%q) = false", a)
		}
	}
	for _, a := range []string{"none of the above\nfoo", "android client install", "NONE\nandroid client", ""} {
		if SaysNone(a) {
			t.Errorf("SaysNone(%q) = true", a)
		}
	}
}

// The query writer may say "don't search" (Dick 2026-11-19): a NONE answer
// skips the whole run, the raw-message search and the judge included.
func TestRun_QueryModelNoneSkipsSearchAndJudge(t *testing.T) {
	f := newFixture(t)
	f.model.queries = "NONE"
	f.results["*"] = []memory.Result{annaHit()}
	o := f.engine.Run(context.Background(), Request{SessionKey: "clutch/c1", Role: RoleUser,
		Text: "Thanks, that looks good to me", Settings: settings()})
	if o.Skipped != "query model: none" || o.Injection != "" {
		t.Fatalf("outcome = %+v", o)
	}
	if len(f.queries) != 0 || len(f.model.judged) != 0 {
		t.Fatalf("searched %q, judged %d times; want neither", f.queries, len(f.model.judged))
	}
}

// Earlier messages are labelled as background, and the current message as
// CURRENT, so the models do not take an earlier topic for the subject.
func TestMessageBlock_LabelsContextAsBackground(t *testing.T) {
	b := messageBlock(RoleUser, "install the latest client", []Msg{{Role: RoleAgent, Text: "the obsidian vault skill"}})
	if !strings.Contains(b, "background only") || !strings.Contains(b, "CURRENT message (from the user):\ninstall the latest client") {
		t.Fatalf("block:\n%s", b)
	}
	for _, p := range []string{querySystemPrompt, judgeSystemPrompt} {
		if !strings.Contains(p, "CURRENT") {
			t.Errorf("prompt does not name the CURRENT message: %q", p)
		}
	}
}

// A note the asking session's family wrote through its tools (here: its
// reflection branch's heredoc) within the window is not offered to that
// session's judge; another session still gets it (#3185).
func TestRun_DropsOwnFamilyNote(t *testing.T) {
	f := newFixture(t)
	f.results["*"] = []memory.Result{annaHit()}
	s := settings()
	s.QueryStrategy = QueryRaw
	// A Claude Code Bash input, as OnToolStart carries it: JSON, the
	// heredoc body in the command string.
	f.own.Observe("clutch/c1/b1795000000", `{"command":"cat >> memory/a.md <<'EOF'\n## Anna's Archive working domain\n\nWorking domain from the NUC: annas-archive.pk (14:50).\nThe .org one is dead.\nEOF"}`)
	f.own.Flush()
	o := f.engine.Run(context.Background(), Request{SessionKey: "clutch/c1", Role: RoleUser, Text: "Anna's archive link please", Settings: s})
	if o.Injection != "" || o.Skipped != "no new hits" {
		t.Fatalf("own note offered: %+v", o)
	}
	var hits string
	if err := f.store.db.QueryRow(`SELECT hits FROM runs WHERE id = ?`, o.RunID).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hits, `"verdict":"own_note"`) {
		t.Fatalf("drop not logged: %s", hits)
	}
	if o := f.engine.Run(context.Background(), Request{SessionKey: "clutch/c2", Role: RoleUser, Text: "Anna's archive link please", Settings: s}); o.Injection == "" {
		t.Fatalf("another session lost the note: %+v", o)
	}
}

// Another session's conversation row is a candidate: the judge sees it
// labelled with its session, row and time.
func TestRun_ConversationHitIsJudged(t *testing.T) {
	f := newFixture(t)
	at := time.Date(2026, 11, 19, 14, 2, 0, 0, time.UTC)
	f.results["*"] = []memory.Result{{Path: "clutch/c2", Source: "conversation", RowID: 45, Time: at,
		Snippet: "the >annas-archive< mirror is annas-archive.pk"}}
	s := settings()
	s.QueryStrategy = QueryRaw
	o := f.engine.Run(context.Background(), Request{SessionKey: "clutch/c1", Role: RoleUser, Text: "Anna's archive link please", Settings: s})
	if o.Injection == "" || !strings.Contains(o.Injection, "clutch/c2#45") {
		t.Fatalf("outcome = %+v", o)
	}
	if j := f.model.judged[0]; !strings.Contains(j, "[1] clutch/c2#45 — conversation 2026-11-19 14:02 UTC") || !strings.Contains(j, "annas-archive.pk") {
		t.Fatalf("judge prompt:\n%s", j)
	}
}

// Package jitmemory is just-in-time memory search (#3175).
//
// Dick 2026-11-19: when the user sends the agent a message, or the agent
// sends the user one, foci searches the agent's memory for it, a fast model
// judges which hits are useful context for the agent, and foci gives the
// agent those. For a user message they are attached to the message; for an
// agent message they are sent to the agent as information it need not answer.
// The feature is off by default and invisible to the user.
//
// One run (Engine.Run) is:
//
//  1. skip a message shorter than Settings.SkipChars;
//  2. for a message shorter than Settings.ContextChars, take the previous
//     Settings.ContextMessages messages of the chat (both sides) as context;
//  3. make the queries (Settings.QueryStrategy): the raw message, 1-3
//     keyword queries the fast model writes, or both;
//  4. search the agent's own memory index with each query, top
//     Settings.TopN per query; drop conversation hits, hits in files of the
//     agent's system prompt, and hits already sent to the judge in this
//     session less than Settings.DedupeCompactions compactions ago;
//  5. read the section of the file around each hit and give the message,
//     its context and the sections to the judge (the fast model), which
//     names the relevant ones (possibly none);
//  6. format the relevant sections, within Settings.InjectMaxChars.
//
// Every run is logged to the Store, for the evaluation Dick asked for. A
// failing step is not retried: it is logged once as a warning and the run
// injects nothing.
package jitmemory

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"foci/internal/log"
	"foci/internal/memory"
)

var jitLog = log.NewComponentLogger("jit-memory")

// Role is which side of the chat wrote a message.
type Role string

const (
	RoleUser  Role = "user"
	RoleAgent Role = "agent"
)

// Query strategies (Settings.QueryStrategy).
const (
	QueryRaw   = "raw"   // the message itself is the one query
	QueryModel = "model" // the fast model writes 1-3 keyword queries
	QueryBoth  = "both"  // both, hits merged
)

// Settings are the live knobs of the feature ([jit_memory], per agent).
type Settings struct {
	Enabled           bool
	SkipChars         int           // a message shorter than this (runes) is not searched
	ContextChars      int           // a message shorter than this gets the previous messages as context
	ContextMessages   int           // how many previous messages that is
	TopN              int           // hits per query given to the judge
	Wait              time.Duration // how long a user message waits for the result
	DedupeCompactions int           // compactions before a hit may be judged again in a session
	InjectMaxChars    int           // the most characters one injection carries
	SectionMaxChars   int           // the most characters of one hit's section
	QueryStrategy     string        // raw | model | both
	QueryModel        string        // "" = the fast summary model
	JudgeModel        string        // "" = the fast summary model
}

// CallReport describes one model call.
type CallReport struct {
	Model        string
	InputTokens  int
	OutputTokens int
	Duration     time.Duration
	CostUSD      float64
}

// ModelFunc calls the fast model: what names the call, model "" means the
// fast summary model. It must not retry.
type ModelFunc func(ctx context.Context, what, model, system, user string, maxTokens int) (string, CallReport, error)

// Deps is what an Engine needs from its agent. Every func is called fresh per
// run, so a reindex or a reloaded system prompt is seen.
type Deps struct {
	AgentID string
	// Search runs one relevance-sorted query on the agent's memory index.
	Search func(query string) ([]memory.Result, error)
	// SourceDirs maps a memory source name to its directory, to read the
	// file behind a hit.
	SourceDirs func() map[string]string
	// SystemPromptFiles lists the absolute paths of the files in the
	// agent's system prompt: hits in them are never offered.
	SystemPromptFiles func() []string
	Model             ModelFunc
	// Store returns the run log and dedupe store; nil (or a nil func) =
	// runs are not recorded and the dedupe set lives in memory only.
	Store func() *Store
}

// Msg is one chat message, for context.
type Msg struct {
	Role Role
	Text string
}

// Request is one message to run on.
type Request struct {
	SessionKey string
	Role       Role
	Text       string
	Settings   Settings
}

// Outcome is what one run produced.
type Outcome struct {
	RunID     int64  // the Store row (0 = not recorded)
	Injection string // the text to give the agent; "" = nothing relevant
	Skipped   string // why the run did nothing, when it did nothing by design
	Err       error  // the step that failed (already logged)
}

// recentCap bounds the per-session message ring.
const recentCap = 16

// sessionState is one session's dedupe set and recent messages.
type sessionState struct {
	loaded bool
	gen    int            // compactions seen
	seen   map[string]int // hit key -> the gen it was judged in
	recent []Msg
}

// Engine runs JIT memory searches for one agent.
type Engine struct {
	deps Deps

	mu       sync.Mutex
	sessions map[string]*sessionState
}

// New builds an Engine.
func New(deps Deps) *Engine {
	return &Engine{deps: deps, sessions: map[string]*sessionState{}}
}

// store returns the Store, or nil.
func (e *Engine) store() *Store {
	if e.deps.Store == nil {
		return nil
	}
	return e.deps.Store()
}

// SetDelivery records how run id's injection reached the agent (a no-op
// without a Store or a run id).
func (e *Engine) SetDelivery(id int64, mode string) {
	store := e.store()
	if store == nil || id == 0 {
		return
	}
	if err := store.SetDelivery(id, mode); err != nil {
		jitLog.Warnf("record delivery of run %d: %v", id, err)
	}
}

// stateLocked returns sk's state, loading its persisted dedupe set once.
func (e *Engine) stateLocked(sk string) *sessionState {
	st := e.sessions[sk]
	if st == nil {
		st = &sessionState{seen: map[string]int{}}
		e.sessions[sk] = st
	}
	if !st.loaded {
		st.loaded = true
		if store := e.store(); store != nil {
			gen, seen, err := store.LoadSession(sk)
			if err != nil {
				jitLog.Warnf("session=%s load dedupe set: %v", sk, err)
			} else {
				st.gen, st.seen = gen, seen
			}
		}
	}
	return st
}

// NoteCompaction records that sk's context was compacted: hits judged
// Settings.DedupeCompactions compactions ago may be judged again.
func (e *Engine) NoteCompaction(sk string) {
	e.mu.Lock()
	st := e.stateLocked(sk)
	st.gen++
	gen := st.gen
	e.mu.Unlock()
	if store := e.store(); store != nil {
		if err := store.SetGeneration(sk, gen); err != nil {
			jitLog.Warnf("session=%s record compaction: %v", sk, err)
		}
	}
}

// observe appends m to sk's recent messages and returns the ones before it.
func (e *Engine) observe(sk string, m Msg) []Msg {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.stateLocked(sk)
	before := append([]Msg(nil), st.recent...)
	st.recent = append(st.recent, m)
	if len(st.recent) > recentCap {
		st.recent = st.recent[len(st.recent)-recentCap:]
	}
	return before
}

// Observe records a message the engine will not run on (the feature off for
// it), so later short messages still get their context.
func (e *Engine) Observe(sk string, role Role, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	e.observe(sk, Msg{Role: role, Text: text})
}

// Hit is one candidate memory section.
type Hit struct {
	Key     string `json:"key"` // path#section, the dedupe identity
	Source  string `json:"source"`
	Path    string `json:"path"`    // relative to the source dir
	Section string `json:"section"` // the heading, or "L<line>"
	Query   int    `json:"query"`   // index of the first query that found it
	Rank    int    `json:"rank"`    // its position in that query's results
	ModTime int64  `json:"mtime"`   // the file's mtime (unix), to tell this session's own notes apart
	// Verdict: judged_relevant, judged_irrelevant, seen (deduped), excluded
	// (system prompt file), unreadable.
	Verdict string `json:"verdict"`
	text    string
}

// Run runs one message. It is safe to call from many goroutines. ctx bounds
// the whole run.
func (e *Engine) Run(ctx context.Context, req Request) Outcome {
	s := req.Settings
	text := strings.TrimSpace(req.Text)
	before := e.observe(req.SessionKey, Msg{Role: req.Role, Text: text})
	rec := &RunRecord{
		Time: time.Now(), AgentID: e.deps.AgentID, SessionKey: req.SessionKey, Role: string(req.Role),
		TextChars: utf8.RuneCountInString(text), TextHead: headRunes(text, 200), Strategy: s.QueryStrategy,
	}
	start := time.Now()
	finish := func(o Outcome) Outcome {
		rec.TotalMS = time.Since(start).Milliseconds()
		if o.Err != nil {
			rec.Error = o.Err.Error()
		}
		rec.Skipped = o.Skipped
		rec.Injection = o.Injection
		if store := e.store(); store != nil {
			id, err := store.LogRun(rec)
			if err != nil {
				jitLog.Warnf("session=%s record run: %v", req.SessionKey, err)
			}
			o.RunID = id
		}
		return o
	}

	if rec.TextChars < s.SkipChars {
		return finish(Outcome{Skipped: "short"})
	}
	var ctxMsgs []Msg
	if rec.TextChars < s.ContextChars && s.ContextMessages > 0 {
		if n := len(before); n > s.ContextMessages {
			before = before[n-s.ContextMessages:]
		}
		ctxMsgs = before
	}
	rec.ContextMessages = len(ctxMsgs)
	prompt := messageBlock(req.Role, text, ctxMsgs)

	// Queries.
	var queries []string
	strategy := s.QueryStrategy
	if strategy != QueryRaw && strategy != QueryModel {
		strategy = QueryBoth
	}
	if strategy != QueryModel {
		queries = append(queries, headRunes(text, rawQueryMaxRunes))
	}
	if strategy != QueryRaw {
		answer, rep, err := e.deps.Model(ctx, "jit memory queries", s.QueryModel, querySystemPrompt, prompt, 80)
		rec.addCall("query", rep, err)
		if err != nil {
			jitLog.Warnf("session=%s query model failed (no retry): %v", req.SessionKey, err)
			if strategy == QueryModel {
				return finish(Outcome{Err: fmt.Errorf("query model: %w", err)})
			}
		} else {
			queries = append(queries, ParseQueries(answer)...)
		}
	}
	queries = dedupeStrings(queries)
	rec.Queries = queries
	if len(queries) == 0 {
		return finish(Outcome{Skipped: "no queries"})
	}

	// Search, merge, filter, dedupe.
	searchStart := time.Now()
	hits, err := e.collect(req.SessionKey, queries, s)
	rec.SearchMS = time.Since(searchStart).Milliseconds()
	rec.Hits = hits
	if err != nil {
		jitLog.Warnf("session=%s memory search failed (no retry): %v", req.SessionKey, err)
		return finish(Outcome{Err: fmt.Errorf("search: %w", err)})
	}
	var cands []*Hit
	for i := range hits {
		if hits[i].Verdict == "" {
			cands = append(cands, &hits[i])
		}
	}
	if len(cands) == 0 {
		return finish(Outcome{Skipped: "no new hits"})
	}
	e.markSeen(req.SessionKey, cands)

	// Judge.
	answer, rep, err := e.deps.Model(ctx, "jit memory judge", s.JudgeModel, judgeSystemPrompt, judgePrompt(prompt, cands), 40)
	rec.addCall("judge", rep, err)
	if err != nil {
		jitLog.Warnf("session=%s judge model failed (no retry): %v", req.SessionKey, err)
		for _, c := range cands {
			c.Verdict = "unjudged"
		}
		return finish(Outcome{Err: fmt.Errorf("judge: %w", err)})
	}
	rec.JudgeAnswer = headRunes(answer, 300)
	picked := ParseVerdict(answer, len(cands))
	var relevant []*Hit
	for i, c := range cands {
		if picked[i] {
			c.Verdict = "judged_relevant"
			relevant = append(relevant, c)
		} else {
			c.Verdict = "judged_irrelevant"
		}
	}
	if len(relevant) == 0 {
		return finish(Outcome{Skipped: "none relevant"})
	}
	return finish(Outcome{Injection: FormatInjection(relevant, s.InjectMaxChars)})
}

// rawQueryMaxRunes caps the raw-message query: a long agent reply searched
// whole is all common words.
const rawQueryMaxRunes = 600

// collect runs every query and returns the merged hits, each with its
// verdict set when it is not a candidate (seen, excluded, unreadable).
func (e *Engine) collect(sk string, queries []string, s Settings) ([]Hit, error) {
	dirs := map[string]string{}
	if e.deps.SourceDirs != nil {
		dirs = e.deps.SourceDirs()
	}
	excluded := map[string]bool{}
	if e.deps.SystemPromptFiles != nil {
		for _, p := range e.deps.SystemPromptFiles() {
			for _, v := range pathVariants(p) {
				excluded[v] = true
			}
		}
	}
	byKey := map[string]bool{}
	var hits []Hit
	var lastErr error
	ok := 0
	for qi, q := range queries {
		res, err := e.deps.Search(q)
		if err != nil {
			lastErr = err
			continue
		}
		ok++
		rank := 0
		for _, r := range res {
			if r.Source == "conversation" {
				continue // memory files only: a chat row is not a note
			}
			if rank >= s.TopN {
				break
			}
			rank++
			dir, known := dirs[r.Source]
			abs := r.Path
			if known && !filepath.IsAbs(abs) {
				abs = filepath.Join(dir, r.Path)
			}
			h := Hit{Source: r.Source, Path: r.Path, Query: qi, Rank: rank}
			if !r.Time.IsZero() {
				h.ModTime = r.Time.Unix()
			}
			if isExcluded(abs, excluded) {
				h.Key = r.Path
				h.Verdict = "excluded"
			} else if sec, err := ReadSection(abs, r.Snippet, s.SectionMaxChars); err != nil || !known {
				h.Key = r.Path
				h.Verdict = "unreadable"
			} else {
				h.Section, h.text = sec.Heading, sec.Text
				h.Key = r.Path + "#" + sec.Heading
			}
			if byKey[h.Key] {
				continue
			}
			byKey[h.Key] = true
			hits = append(hits, h)
		}
	}
	if ok == 0 && lastErr != nil {
		return nil, lastErr
	}
	// Dedupe against what this session's judge already saw.
	e.mu.Lock()
	st := e.stateLocked(sk)
	for i := range hits {
		if hits[i].Verdict != "" {
			continue
		}
		if g, seen := st.seen[hits[i].Key]; seen && st.gen-g < s.DedupeCompactions {
			hits[i].Verdict = "seen"
		}
	}
	e.mu.Unlock()
	return hits, nil
}

// markSeen records that cands went to the judge in this session.
func (e *Engine) markSeen(sk string, cands []*Hit) {
	e.mu.Lock()
	st := e.stateLocked(sk)
	gen := st.gen
	keys := make([]string, 0, len(cands))
	for _, c := range cands {
		st.seen[c.Key] = gen
		keys = append(keys, c.Key)
	}
	e.mu.Unlock()
	if store := e.store(); store != nil {
		if err := store.MarkSeen(sk, keys, gen); err != nil {
			jitLog.Warnf("session=%s record dedupe set: %v", sk, err)
		}
	}
}

// pathVariants is p cleaned and, when it resolves, with symlinks resolved.
func pathVariants(p string) []string {
	out := []string{filepath.Clean(p)}
	if r, err := filepath.EvalSymlinks(p); err == nil && r != out[0] {
		out = append(out, r)
	}
	return out
}

func isExcluded(abs string, excluded map[string]bool) bool {
	for _, v := range pathVariants(abs) {
		if excluded[v] {
			return true
		}
	}
	return false
}

// querySystemPrompt asks for keyword queries only. The model's answer is
// still filtered (ParseQueries): Llama 3.1 8B adds a preamble line.
const querySystemPrompt = "You write search queries for an AI assistant's memory notes. " +
	"Given a chat message (and maybe the messages before it), write 1 to 3 short keyword search queries, 2 to 5 words each, " +
	"that would find notes relevant to it: names of systems, tickets, tools, people, places. " +
	"Reply with the queries only, one per line. No numbering, no quotes, no preamble, no explanation."

// judgeSystemPrompt asks for the numbers of the useful notes.
const judgeSystemPrompt = "You decide which memory notes would be useful extra context for an AI assistant handling a chat message. " +
	"A note is useful only if it is specifically about the subject of the message and would help the assistant answer or act. " +
	"A note that merely shares common words is not useful. Most messages need no notes. " +
	"Reply with the numbers of the useful notes, comma-separated (for example: 2, 5), or the word none. Nothing else."

// messageBlock renders the message and its context for the models.
func messageBlock(role Role, text string, ctxMsgs []Msg) string {
	var b strings.Builder
	if len(ctxMsgs) > 0 {
		b.WriteString("Earlier messages, oldest first:\n")
		for _, m := range ctxMsgs {
			fmt.Fprintf(&b, "[%s] %s\n", roleName(m.Role), headRunes(m.Text, 600))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "The message (from the %s):\n%s\n", roleName(role), headRunes(text, 3000))
	return b.String()
}

func roleName(r Role) string {
	if r == RoleAgent {
		return "assistant"
	}
	return "user"
}

// judgePrompt renders the message block and the numbered candidates.
func judgePrompt(msg string, cands []*Hit) string {
	var b strings.Builder
	b.WriteString(msg)
	b.WriteString("\nMemory notes:\n")
	for i, c := range cands {
		fmt.Fprintf(&b, "\n[%d] %s — %s\n%s\n", i+1, c.Path, c.Section, c.text)
	}
	return b.String()
}

// listMarker matches a leading list marker: "1.", "2)", "-", "*", "•".
var listMarker = regexp.MustCompile(`^\s*(?:\d+[.)]|[-*•])\s*`)

// ParseQueries reads the query model's answer: one query per line, list
// markers and quotes removed, and every line that is not a query dropped (a
// preamble such as "Here are three potential keyword search queries:", an
// empty line, a line of more than 8 words). At most 3 are kept.
func ParseQueries(answer string) []string {
	var out []string
	for _, line := range strings.Split(answer, "\n") {
		l := strings.TrimSpace(listMarker.ReplaceAllString(line, ""))
		l = strings.Trim(l, "\"'`“”‘’")
		l = strings.TrimSpace(l)
		if l == "" || strings.HasSuffix(l, ":") {
			continue
		}
		low := strings.ToLower(l)
		if strings.HasPrefix(low, "here are") || strings.HasPrefix(low, "here is") || strings.HasPrefix(low, "sure") ||
			strings.HasPrefix(low, "query") || strings.HasPrefix(low, "queries") || strings.HasPrefix(low, "note:") {
			continue
		}
		if len(strings.Fields(l)) > 8 {
			continue
		}
		out = append(out, l)
		if len(out) == 3 {
			break
		}
	}
	return out
}

var verdictNumber = regexp.MustCompile(`\d+`)

// ParseVerdict reads the judge's answer: the note numbers it names (1-based,
// within 1..n). "none" or no number picks nothing.
func ParseVerdict(answer string, n int) map[int]bool {
	picked := map[int]bool{}
	if strings.Contains(strings.ToLower(answer), "none") && !verdictNumber.MatchString(answer) {
		return picked
	}
	for _, m := range verdictNumber.FindAllString(answer, -1) {
		v, err := strconv.Atoi(m)
		if err == nil && v >= 1 && v <= n {
			picked[v-1] = true
		}
	}
	return picked
}

// InjectionHeader opens every injection: the agent can tell it from the
// user's own words, and knows it need not answer it.
const InjectionHeader = "[foci JIT memory: notes from your memory files that a fast model judged relevant to this message. " +
	"Auto-injected for your consideration; no response necessary.]"

// FormatInjection renders the relevant sections within maxChars (runes; 0 =
// no cap). Sections are kept whole in judge order until the next would not
// fit; the first one is cut to fit when it alone is too long.
func FormatInjection(hits []*Hit, maxChars int) string {
	var b strings.Builder
	b.WriteString(InjectionHeader)
	used := utf8.RuneCountInString(InjectionHeader)
	n := 0
	for _, h := range hits {
		block := fmt.Sprintf("\n\n--- %s § %s\n%s", h.Path, h.Section, h.text)
		size := utf8.RuneCountInString(block)
		if maxChars > 0 && used+size > maxChars {
			if n > 0 {
				break
			}
			block = headRunes(block, maxChars-used) + " …"
			size = utf8.RuneCountInString(block)
		}
		b.WriteString(block)
		used += size
		n++
	}
	return b.String()
}

func headRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		k := strings.ToLower(strings.TrimSpace(s))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

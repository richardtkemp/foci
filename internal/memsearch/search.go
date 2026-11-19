// Package memsearch is the one memory search both callers use (#3185): the
// foci_memory_search tool and JIT memory (internal/jitmemory). Dick
// 2026-11-19 15:45: "jit may only return memory, but memory search tool
// returns conversations - that should be included in the jit results, DRY";
// Q124: "Same rules for both".
//
// A search runs one query on the agent's memory backend and then drops the
// hits that are the asking session's own recent material:
//
//   - a conversation row of the asking session's family (its root and the
//     branches under it) newer than the root's Nth-last compaction
//     (DropOwnConversation);
//   - a memory-file hit whose probe lines the family wrote or read through
//     its tools within the same window (DropOwnNote; see Index);
//   - for JIT only (Request.DropNonDelivered), a conversation row that never
//     reached the chat: a subagent's, a reflection, background, consolidation
//     or other non-delivered turn's, or any row of a branch session
//     (DropNonDelivered). The tool keeps them, labelled with their kind.
//
// Every drop is returned with its reason, so each caller can log it.
package memsearch

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/memory"
	"foci/internal/session"
)

// Drop reasons.
const (
	DropOwnConversation = "own_conversation" // a row of the asking family within the window
	DropOwnNote         = "own_note"         // a memory-file section the family wrote or read within the window
	DropNonDelivered    = "non_delivered"    // a conversation row that never reached the chat (JIT only)
)

// ConversationSource is the Source of a conversation hit.
const ConversationSource = "conversation"

// probeSectionChars bounds the section read around a file hit to pick its
// probe lines when the caller did not ask for sections.
const probeSectionChars = 4000

// overfetch is how many times the normal result count is fetched when hits
// may be dropped after the search, so the kept hits still fill the limit.
const overfetch = 3

// Searcher is an agent's memory search with the own-material rules. Build one
// per agent and give the same one to every caller.
type Searcher struct {
	// Backends are the agent's search backends by name; DefaultBackend names
	// the one to use when a request names none (read per search).
	Backends       map[string]memory.Searcher
	DefaultBackend func() string
	// Conv reads conversation rows' kinds and context; nil = none.
	Conv *memory.ConversationReader
	// Own is the own-material index; nil = the window rules are off and the
	// asking session's own rows are excluded whole (the behaviour before
	// #3185).
	Own *Index
	// SourceDirs maps a memory source name to its directory, to read the
	// file behind a hit; nil = files are not read (no own-note check).
	SourceDirs func() map[string]string
	// Window is N, the number of compactions the own-material window spans
	// ([jit_memory] dedupe_compactions), read per search; nil = 0.
	Window func() int
}

// Request is one search.
type Request struct {
	Query   string
	Sort    string // relevance (""), newest, oldest
	Backend string // "" = DefaultBackend
	// DateFrom (inclusive) and DateTo (exclusive) bound the hits' times.
	DateFrom, DateTo *time.Time
	// SessionKey is the asking session; "" = nothing is own.
	SessionKey string
	// DropNonDelivered drops conversation rows that never reached a chat
	// (JIT; the tool labels them instead).
	DropNonDelivered bool
	// Sections reads the markdown section around each file hit into
	// Hit.Section, within SectionMaxChars runes (0 = probeSectionChars).
	Sections        bool
	SectionMaxChars int
	// ContextLines reads that many conversation messages around each
	// conversation hit into Hit.Context (0 = none).
	ContextLines int
}

// Hit is one kept search result.
type Hit struct {
	memory.Result
	// Pos is the result's position in the backend's list (0-based), before
	// any drop: a caller that keeps the top N can tell which drops ranked
	// inside its cut.
	Pos int
	// Kind is the conversation row's kind label (memory.ConversationMessage.Kind).
	Kind string
	// Abs is the absolute path of a file hit, when its source is known.
	Abs string
	// Section is the section around a file hit (Request.Sections, or read
	// for the own-note check); nil when not read or unreadable.
	Section *Section
	// Context is the conversation around a conversation hit
	// (Request.ContextLines).
	Context []memory.ConversationMessage
}

// Dropped is one result a rule removed.
type Dropped struct {
	Hit
	Reason string
	Detail string
}

// Outcome is what one search returned.
type Outcome struct {
	Hits    []Hit
	Dropped []Dropped
	Window  Window
}

// backend picks the request's backend.
func (s *Searcher) backend(name string) (memory.Searcher, error) {
	if name != "" {
		if b, ok := s.Backends[name]; ok {
			return b, nil
		}
		return nil, fmt.Errorf("unknown search backend %q", name)
	}
	if s.DefaultBackend != nil {
		if b, ok := s.Backends[s.DefaultBackend()]; ok {
			return b, nil
		}
	}
	for _, b := range s.Backends { // the default is not configured: any
		return b, nil
	}
	return nil, fmt.Errorf("no memory search backend")
}

// Search runs req.
func (s *Searcher) Search(req Request) (Outcome, error) {
	be, err := s.backend(req.Backend)
	if err != nil {
		return Outcome{}, err
	}
	n := 0
	if s.Window != nil {
		n = s.Window()
	}
	var w Window
	if s.Own != nil {
		w = s.Own.Window(req.SessionKey, n)
	}

	opts := &memory.SearchOptions{DateFrom: req.DateFrom, DateTo: req.DateTo}
	filtering := w.Active() || req.DropNonDelivered
	if s.Own == nil && req.SessionKey != "" {
		// No index: the asking session's own rows are excluded whole, in the
		// query (the behaviour before #3185).
		opts.ExcludePath = req.SessionKey
	}
	limit := 0
	if filtering {
		if l, ok := be.(memory.ReturnLimiter); ok {
			limit = l.ReturnLimit()
			opts.Limit = limit * overfetch
		}
	}
	results, err := be.Search(req.Query, req.Sort, opts)
	if err != nil {
		return Outcome{}, err
	}

	var refs []memory.ConversationRef
	for _, r := range results {
		if r.Source == ConversationSource && r.RowID > 0 {
			refs = append(refs, memory.ConversationRef{Session: r.Path, RowID: r.RowID})
		}
	}
	kinds := s.Conv.Kinds(refs)

	dirs := map[string]string{}
	if s.SourceDirs != nil {
		dirs = s.SourceDirs()
	}
	out := Outcome{Window: w}
	for i, r := range results {
		h := Hit{Result: r, Pos: i}
		if r.Source == ConversationSource {
			h.Kind = kinds[memory.ConversationRef{Session: r.Path, RowID: r.RowID}]
			if w.Own(r.Path, r.Time) {
				out.Dropped = append(out.Dropped, Dropped{Hit: h, Reason: DropOwnConversation,
					Detail: ownConversationDetail(w, r.Time)})
				continue
			}
			if req.DropNonDelivered && nonDelivered(r.Path, h.Kind) {
				detail := h.Kind
				if detail == "" {
					detail = "branch session"
				}
				out.Dropped = append(out.Dropped, Dropped{Hit: h, Reason: DropNonDelivered, Detail: detail})
				continue
			}
			if req.ContextLines > 0 && r.RowID > 0 && s.Conv != nil {
				if msgs, err := s.Conv.ReadContext(r.Path, r.RowID, req.ContextLines); err == nil {
					h.Context = msgs
				}
			}
		} else {
			if dir, ok := dirs[r.Source]; ok {
				h.Abs = r.Path
				if !filepath.IsAbs(h.Abs) {
					h.Abs = filepath.Join(dir, r.Path)
				}
			}
			if h.Abs != "" && (req.Sections || w.Active()) {
				max := req.SectionMaxChars
				if !req.Sections || max <= 0 {
					max = probeSectionChars
				}
				if sec, err := ReadSection(h.Abs, r.Snippet, max); err == nil {
					h.Section = &sec
				}
			}
			if own, detail := s.ownNote(w, h); own {
				out.Dropped = append(out.Dropped, Dropped{Hit: h, Reason: DropOwnNote, Detail: detail})
				continue
			}
			if !req.Sections {
				h.Section = nil
			}
		}
		out.Hits = append(out.Hits, h)
	}
	if limit > 0 && len(out.Hits) > limit {
		out.Hits = out.Hits[:limit]
	}
	return out, nil
}

// ownNote reports whether file hit h is the family's own note: probesNeeded
// of its probe lines are in the family's line set within the window.
func (s *Searcher) ownNote(w Window, h Hit) (bool, string) {
	if !w.Active() || h.Section == nil {
		return false, ""
	}
	probes := Probes(stripCutLines(h.Section.Text))
	if len(probes) == 0 {
		return false, ""
	}
	hashes := make([]int64, len(probes))
	for i, p := range probes {
		hashes[i] = HashLine(p)
	}
	m, err := s.Own.Matches(w, hashes)
	if err != nil {
		ownLog.Warnf("family=%s own-note lookup: %v", w.Family, err)
		return false, ""
	}
	need := probesNeeded(len(probes))
	return m >= need, fmt.Sprintf("%d/%d probe lines in the family's tool lines since compaction %d", m, len(probes), w.MinGen)
}

// stripCutLines drops a section window's first and last line when the window
// cut them ("… " / " …"): a cut line is not a line any tool wrote.
func stripCutLines(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && strings.HasPrefix(lines[0], "… ") {
		lines = lines[1:]
	}
	if n := len(lines); n > 0 && strings.HasSuffix(lines[n-1], " …") {
		lines = lines[:n-1]
	}
	return strings.Join(lines, "\n")
}

// nonDelivered reports whether a conversation row never reached a chat: it
// has a kind (a subagent's row, a reflection, background, consolidation or
// other non-delivered turn, #2060), or its session is a branch (a row of a
// branch session is the branch's, not the chat's, also where the backend
// cannot tell its kind).
func nonDelivered(sessionKey, kind string) bool {
	if kind != "" {
		return true
	}
	k, err := session.ParseSessionKey(sessionKey)
	return err == nil && !k.IsRoot()
}

func ownConversationDetail(w Window, t time.Time) string {
	since := "the family's first row (fewer compactions recorded than the window)"
	if !w.Since.IsZero() {
		since = "compaction " + fmt.Sprint(w.MinGen) + " at " + w.Since.UTC().Format(time.RFC3339)
	}
	at := "unknown time"
	if !t.IsZero() {
		at = t.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("row at %s, window of %d compactions starts at %s", at, w.N, since)
}

// DropSummary renders drops for a log line: "reason path[#row] (detail); ...".
func DropSummary(drops []Dropped) string {
	parts := make([]string, 0, len(drops))
	for _, d := range drops {
		p := d.Path
		if d.RowID > 0 {
			p = fmt.Sprintf("%s#%d", d.Path, d.RowID)
		}
		parts = append(parts, fmt.Sprintf("%s %s (%s)", d.Reason, p, d.Detail))
	}
	return strings.Join(parts, "; ")
}

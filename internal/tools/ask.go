package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"foci/internal/app/fap"
	"foci/internal/procx"
	"foci/internal/question"
	"foci/internal/session"
)

// Grader defaults and bounds.
const (
	defaultGraderTimeout  = 15 * time.Second
	maxGraderOutput       = 256 * 1024 // cap grader stdout delivered to the agent
	graderOnErrorFallback = "fallback" // deliver raw answers + note on failure (default)
	graderOnErrorReport   = "report"   // deliver a failure report (+ raw answers) instead
)

// graderConfig carries the optional answer-grader settings for one ask.
type graderConfig struct {
	path    string        // absolute path to an executable; empty disables grading
	timeout time.Duration // 0 ⇒ defaultGraderTimeout
	onError string        // graderOnErrorFallback (default) | graderOnErrorReport
	args    []string      // extra argv passed AFTER request_id (e.g. source filename)
}

// ---------------------------------------------------------------------------
// foci-native `ask` / `foci_ask` tool
// ---------------------------------------------------------------------------
//
// `ask` mirrors Claude Code's AskUserQuestion arguments (questions → options,
// headers) but with NO 4-question cap, and works for ANY backend (delegated CC
// and API agents). It is ASYNC: the tool posts the first question and returns
// immediately; the user answers one question at a time via interactive buttons
// (or, later, a typed reply), and once every question is answered the collected
// {questions, answers} are delivered back into the calling session as a normal
// inbound user message — waking the agent in a fresh turn.
//
// This async model deliberately sidesteps Claude Code's 600s Bash-tool ceiling
// (a blocking tool call could not wait longer than that) and is the common
// denominator across backends: "deliver a user message to session X" works
// identically for delegated and API agents.
//
// The pure question machinery (types, parse, format, choices, answer
// resolution, accumulator) lives in internal/question and is shared with
// ccstream's blocking AskUserQuestion handler so the two cannot drift.

// AskPresentFn presents ONE question to the user for sessionKey and arranges for
// onResponse to be invoked with the chosen data when the user responds:
//   - "qa:<index>" — a button click selecting that option
//   - "qa:cancel"  — the Cancel button
//   - any other string — a typed ("Other") answer
//
// It must be non-blocking. msgID is a unique, colon-free identifier for the
// interactive message (the platform uses it to route the click back).
//
// It returns the platform-side message id of the posted message (empty if it
// could not be posted, or the platform has no addressable id). The ask layer
// persists this so a restart can address the same message for cancel/expiry
// edits — see AskRestoreFn.
type AskPresentFn func(sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) string

// AskPresentBatchFn presents ALL of an ask's questions at once as a single
// batched prompt (native app only) and arranges for onResponse to be called once
// with one RAW answer per question, positionally — each "qa:<index>", the
// "qa:cancel" sentinel, or a typed string. It must be non-blocking.
//
// It returns true if the prompt was presented as a batch, false if the target
// transport/client cannot batch (no live app socket, or the client did not
// advertise the "interactiveBatch" capability). On false the caller falls back to
// the sequential, one-question-at-a-time AskPresentFn path. Chat transports
// (Telegram/Discord) and uncapable clients always yield false, so their behaviour
// is byte-for-byte unchanged. promptID is the colon-free routing id for the form.
type AskPresentBatchFn func(sessionKey, promptID string, qs []question.Question, onResponse func(answers []string)) bool

// AskDeliverFn delivers the assembled answer batch into sessionKey as a new
// inbound user message, waking the agent. Backend-agnostic (delegated + API).
//
// requestID names the ask the message is the RESULT of. The inbox tags the
// resulting injection with it so ask-deferral can tell "the verdict for the ask
// that just resolved" from "a proactive interruption", and stop holding the
// former behind an unrelated, later ask (#1712).
type AskDeliverFn func(sessionKey, requestID, message string)

// AskCloseFn edits an already-displayed question message to finalText and
// removes its buttons. The button-click path closes the message itself (the
// platform edits it to "✅ <label>" / "❌ Cancelled"); this hook exists for the
// TYPED-answer path, where no button was pressed so nothing else clears the
// now-stale Cancel/option buttons. Must be idempotent and nil-safe: closing a
// message the button path already removed is a harmless no-op.
type AskCloseFn func(msgID, finalText string)

// AskRestoreFn re-attaches the interactive callback for ONE already-displayed
// question after a restart. Unlike AskPresentFn it must NOT send a new message:
// the buttons still live on the platform (the message survived the restart), so
// this only rebinds onResponse to the existing buttons identified by msgID. The
// arguments mirror AskPresentFn minus the text/summary (nothing is rendered).
//
// platformMsgID is the platform-side message id captured by AskPresentFn when the
// question was first posted (empty if it was never captured); passing it through
// lets proactive cancel/expiry edits reach the restored message, so a restored
// ask behaves identically to a fresh one. msgID remains the colon-free routing id
// the on-screen buttons carry.
type AskRestoreFn func(sessionKey, msgID, platformMsgID string, choices []question.Choice, onResponse func(data string))

// AskRestoreBatchFn re-registers the callback for a BATCHED (native-app) ask after
// a restart — the batched analogue of AskRestoreFn. Like AskRestoreFn it must NOT
// send a new message: the batched form still lives in the app's local store, so
// this only re-binds onResponse to the server-side prompt registry that a restart
// dropped (the registry is in-memory). Without it, an app answering a pre-restart
// batched ask (InteractiveResponse.Answers) matches no registration and is silently
// dropped — no resolution frame to siblings, no answer to the agent (#1473).
// promptID is the colon-free routing id of the form; questionCount sizes the
// server's accumulated-answers slice; onResponse receives one raw answer per
// question, positionally (same shape as AskPresentBatchFn's onResponse).
type AskRestoreBatchFn func(sessionKey, promptID string, questionCount int, onResponse func(answers []string))

// pendingAsk is one in-flight ask: a sequential accumulator plus the context
// needed to present follow-up questions and deliver the final answers.
type pendingAsk struct {
	requestID     string
	sessionKey    string
	acc           *question.Accumulator
	originalInput json.RawMessage
	createdAt     time.Time
	grader        graderConfig
	// platformMsgID is the platform-side message id of the CURRENTLY displayed
	// question, captured from AskPresentFn and refreshed on each presentCurrent.
	// Persisted so a restart can address the on-screen message for cancel/expiry.
	platformMsgID string
	// paused, when true, suspends typed-answer capture for this ask: the inbound
	// path lets the user's replies run as normal turns instead of feeding them to
	// the ask. Buttons still resolve it. Toggled by /pause and /resume; persisted
	// so the pause survives a restart mid-ask.
	paused bool
	// queued, when true, means this ask has NEVER been presented: the session
	// already had an active ask when it was started and the client cannot show two
	// asks unambiguously, so it waits its turn (#1711 ruling 1). A queued ask owns
	// none of the session-keyed paths (typed answers, /pause, the {ask} statusline
	// field) and does not count as pending for injection deferral (ruling Q4) — it
	// is on nobody's screen and can be answered by nothing. Persisted so a queue
	// survives a restart (ruling Q3) rather than being silently lost.
	queued bool
	// batched records that this ask was presented as a single native-app form
	// (tryPresentBatch succeeded), so the app answers via InteractiveResponse.Answers.
	// Persisted so restore re-registers the BATCHED callback (restoreBatch) rather
	// than the sequential one — the app still shows the same form (#1473).
	batched bool
}

// askState is the registry of in-flight asks. Keyed by requestID for click
// routing and by sessionKey for typed-answer routing.
//
// A session maps to an ORDERED queue of request ids (oldest first), not to a
// single "latest" id (#1711). The session's PRIMARY ask — the oldest one actually
// on screen — owns every session-keyed path; the rest are either coexisting
// native-app forms (the only UI where two open asks are unambiguous) or queued,
// waiting for the primary to resolve.
//
// Pending asks are persisted to the session index (agent_metadata) on every
// change and restored on startup — mirroring the tmux tool's persist-on-change →
// restore-verify-reattach pattern. After a restart the maps are rehydrated and
// each ask's interactive callback is re-attached to the buttons the platform
// still displays (see restorePending), so both button clicks and typed replies
// survive. store==nil disables persistence (in-memory only).
type askState struct {
	mu           sync.Mutex
	byReqID      map[string]*pendingAsk
	bySession    map[string][]string // sessionKey → its asks' requestIDs, oldest first
	present      AskPresentFn
	presentBatch AskPresentBatchFn // app-only batched presentation; nil = always sequential
	restore      AskRestoreFn
	restoreBatch AskRestoreBatchFn // app-only batched re-registration on restart; nil = sequential restore only
	deliver      AskDeliverFn
	closeMsg     AskCloseFn
	store        *session.SessionIndex // nil = no persistence
	agentID      string
	onResolve    func(sessionKey, requestID string) // fired when ONE ask resolves; nil = disabled
	cacheWarm    func(sessionKey string) bool       // reports whether a session's prompt cache is live; nil = always warm (never suppress)
}

// WithOnResolve sets a callback fired (async) when ONE ask resolves — per-ask,
// not only when the session's pending set empties (#1711 ruling 5) — so the caller
// can redeliver the injections that THAT ask was holding (#1712).
func WithOnResolve(fn func(sessionKey, requestID string)) AskOption {
	return func(s *askState) { s.onResolve = fn }
}

// WithCacheWarm wires a predicate reporting whether a session's prompt cache is
// currently live. It gates the trivial "ask cancelled" system injection: when the
// cache is cold, delivering that one-line notice would spin up a turn that rebuilds
// the entire cached prefix at full input-token cost — a large spend for a message
// the agent (whose asking turn already ended) does not need. Nil = never suppress
// (every cancel is injected), preserving the pre-gate behaviour. (#1302)
func WithCacheWarm(fn func(sessionKey string) bool) AskOption {
	return func(s *askState) { s.cacheWarm = fn }
}

// deliverCancel injects the "ask cancelled" system notice into the session —
// unless the session's prompt cache is cold, in which case it is suppressed:
// delivering it would run a turn that rebuilds the whole cached prefix, and the
// asking turn has already ended, so the agent needs nothing from it. (#1302)
func (a *askState) deliverCancel(sessionKey, requestID string, answered, total int) {
	if a.cacheWarm != nil && !a.cacheWarm(sessionKey) {
		askLog.Infof("session=%s req=%s ask cancelled (%d/%d answered) but prompt cache is cold — suppressing trivial cancel injection (would rebuild the prompt cache)",
			sessionKey, requestID, answered, total)
		return
	}
	a.deliverMsg(sessionKey, requestID, fmt.Sprintf(
		"[SYSTEM: the user CANCELLED your `ask` request after %d of %d answers. Do not retry unless they ask.]",
		answered, total))
}

// AskOption customises the ask tool at construction — used to wire optional,
// transport-specific behaviour without widening the core NewAskTool signature
// (and so without churning the many existing callers/tests).
type AskOption func(*askState)

// WithBatchPresent enables app-only batched presentation: when the answering
// client is the native app and advertised the "interactiveBatch" capability, all
// questions are shown as one form and answered together. Every other transport,
// and any app client without the capability, keeps the sequential flow untouched.
// Nil-safe (a nil fn just leaves batching disabled).
func WithBatchPresent(fn AskPresentBatchFn) AskOption {
	return func(s *askState) { s.presentBatch = fn }
}

// WithBatchRestore wires the batched analogue of the sequential AskRestoreFn: on
// restart it re-registers the server-side callback for a batched ask (whose
// in-memory registration a restart drops), so the app's batched answer still
// resolves instead of being silently dropped (#1473). Nil-safe.
func WithBatchRestore(fn AskRestoreBatchFn) AskOption {
	return func(s *askState) { s.restoreBatch = fn }
}

// askMetaKey is the agent_metadata key under which pending asks are persisted.
const askMetaKey = "ask_pending"

// pendingAskTTL bounds how stale a persisted ask may be before restore drops it
// rather than re-attaching it. Matches the interactive prompt lifetime
// (agent.DefaultIdleTimeout); anything that crosses the threshold while running
// is still cleaned up by the periodic interactive-expiry sweep.
const pendingAskTTL = 24 * time.Hour

func newAskState(present AskPresentFn, restore AskRestoreFn, deliver AskDeliverFn, closeMsg AskCloseFn, store *session.SessionIndex, agentID string) *askState {
	return &askState{
		byReqID:   make(map[string]*pendingAsk),
		bySession: make(map[string][]string),
		present:   present,
		restore:   restore,
		deliver:   deliver,
		closeMsg:  closeMsg,
		store:     store,
		agentID:   agentID,
	}
}

// questionMsgID is the colon-free interactive-message id for question idx of an
// ask. Derived from the request id + index so present, reattach, and close all
// address the same on-screen message.
func questionMsgID(requestID string, idx int) string {
	return fmt.Sprintf("%s-q%d", requestID, idx)
}

// answerEcho renders a typed answer for the closed-message confirmation: a
// single line, rune-safely capped, so a long or multi-line answer doesn't bloat
// the edited prompt.
func answerEcho(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	const max = 80
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// nextRequestID returns a unique, colon-free request id. Colon-free matters: the
// platform encodes button data as "<id>:<index>" and splits on the first colon,
// so an id containing ':' would break click routing. The id is a ULID prefixed
// with "ask-" and the agentID (so logs and persisted state self-identify). ULID
// is used instead of a monotonic counter because the counter was in-memory and
// reset on every restart — colliding with prior-process asks whose promptIds
// still lived in the app's DB, silently corrupting batched answer slots.
func (a *askState) nextRequestID() string {
	return "ask-" + a.agentID + "-" + fap.NewULID()
}

// start registers a new ask and presents its first question, reporting whether it
// was QUEUED instead of shown — the tool's ack must say so rather than claim it was
// posted (#1711 ruling Q2: a queued ask returns immediately, queued).
//
// A second ask in a session is ALLOWED and never displaces the first (#1711
// ruling 3): it joins the session's queue in created_at order. Whether it also
// goes live immediately is app-only (ruling 1) — the native app renders each ask
// as its own self-contained form, which is what makes it unambiguous which ask a
// typed answer belongs to; a chat transport has no such affordance, so there the
// ask waits and promoteQueued presents it when the primary resolves.
func (a *askState) start(sessionKey string, qs []question.Question, originalInput json.RawMessage, grader graderConfig) (string, bool) {
	reqID := a.nextRequestID()
	p := &pendingAsk{
		requestID:     reqID,
		sessionKey:    sessionKey,
		acc:           question.NewAccumulator(qs),
		originalInput: originalInput,
		createdAt:     time.Now(),
		grader:        grader,
	}
	a.mu.Lock()
	siblings := a.bySession[sessionKey]
	busy := len(siblings) > 0
	// Strict FIFO: if an ask is already waiting its turn, this one takes the back
	// of the queue rather than jumping it (possible when the app drops offline
	// between two asks and comes back).
	waiting := a.headQueuedLocked(sessionKey) != nil
	p.queued = busy
	a.byReqID[reqID] = p
	a.bySession[sessionKey] = append(siblings, reqID)
	a.persistLocked()
	a.mu.Unlock()

	// Native app clients that advertised the capability get ALL questions as one
	// batched form; everything else (and any uncapable client) falls through to the
	// unchanged sequential path. tryPresentBatch shows nothing and reports false
	// when the client cannot render a self-contained form, so using it as the
	// coexistence test cannot leak a second prompt onto a chat transport.
	presented := false
	if !waiting {
		if a.tryPresentBatch(p) {
			presented = true
		} else if !busy {
			a.presentCurrent(p)
			presented = true
		}
	}
	if !presented {
		askLog.Infof("session=%s req=%s queued behind the session's active ask (%d ahead of it) — coexisting asks are app-only",
			sessionKey, reqID, len(siblings))
	}
	return reqID, !presented
}

// primaryLocked returns the session's PRIMARY ask: the oldest one that is actually
// on screen. Every session-keyed path targets it and only it — typed-answer
// routing, /pause, /resume, /complete, the {ask} statusline field, and the
// injection-deferral gate (#1711 rulings 2 and Q4). Nil when the session has no
// live ask (a session holding only QUEUED asks reads as "nothing pending", which
// is the point: a queued ask is racing nothing). Caller holds a.mu.
func (a *askState) primaryLocked(sessionKey string) *pendingAsk {
	for _, id := range a.bySession[sessionKey] {
		if p := a.byReqID[id]; p != nil && !p.queued {
			return p
		}
	}
	return nil
}

// headQueuedLocked returns the session's next queued ask in created_at order (nil
// if none is waiting). Caller holds a.mu.
func (a *askState) headQueuedLocked(sessionKey string) *pendingAsk {
	for _, id := range a.bySession[sessionKey] {
		if p := a.byReqID[id]; p != nil && p.queued {
			return p
		}
	}
	return nil
}

// promoteQueued presents the session's next queued ask now that the primary has
// resolved. A no-op while a live ask still holds the session (two coexisting app
// forms: resolving one leaves the other in charge) or when nothing is queued.
//
// The pendingAskTTL is applied HERE, aged from created_at (#1711 ruling Q3):
// a queue draining hours later must not surface a question that went stale while
// it waited, so an expired entry is dropped with a notice to the agent — which
// never saw an answer for it — and the next one is considered.
func (a *askState) promoteQueued(sessionKey string) {
	for {
		a.mu.Lock()
		if a.primaryLocked(sessionKey) != nil {
			a.mu.Unlock()
			return
		}
		p := a.headQueuedLocked(sessionKey)
		if p == nil {
			a.mu.Unlock()
			return
		}
		if waited := time.Since(p.createdAt); waited > pendingAskTTL {
			a.removeLocked(p)
			a.persistLocked()
			a.mu.Unlock()
			askLog.Warnf("session=%s req=%s queued ask expired (%s old) before it could be shown — dropped", sessionKey, p.requestID, waited.Round(time.Minute))
			a.deliverMsg(sessionKey, p.requestID, fmt.Sprintf(
				"[SYSTEM: your queued `ask` request (req %s) expired after waiting %s behind an earlier question and was NEVER shown to the user. Nothing was answered. Re-ask if you still need it.]",
				p.requestID, waited.Round(time.Minute)))
			continue
		}
		p.queued = false
		a.persistLocked()
		a.mu.Unlock()
		askLog.Infof("session=%s req=%s promoted from the queue — presenting now", sessionKey, p.requestID)
		if !a.tryPresentBatch(p) {
			a.presentCurrent(p)
		}
		return
	}
}

// tryPresentBatch presents p's whole question set as a single batched prompt via
// presentBatch, returning whether it was batched. The promptID reuses question
// index 0's id so a restart's re-present idempotently UPSERTs the same on-screen
// form rather than duplicating it. Records the platform msg id on success.
func (a *askState) tryPresentBatch(p *pendingAsk) bool {
	if a.presentBatch == nil {
		return false
	}
	promptID := questionMsgID(p.requestID, 0)
	reqID := p.requestID
	if !a.presentBatch(p.sessionKey, promptID, p.acc.Questions(), func(answers []string) {
		a.handleBatchResponse(reqID, answers)
	}) {
		return false
	}
	a.mu.Lock()
	p.platformMsgID = promptID
	p.batched = true
	p.queued = false // a form is on screen: this ask is live, not waiting
	a.persistLocked()
	a.mu.Unlock()
	return true
}

// presentCurrent shows the question the accumulator is currently positioned on.
func (a *askState) presentCurrent(p *pendingAsk) {
	q := p.acc.Current()
	if q == nil {
		return
	}
	idx := p.acc.Index()
	text := question.FormatText(q, idx, p.acc.Total())
	summary := q.Header
	if summary == "" {
		summary = "Question"
	}
	// Each question is its own one-shot interactive message; give it a unique,
	// colon-free id derived from the request id and the question index.
	msgID := questionMsgID(p.requestID, idx)
	if a.present != nil {
		platformMsgID := a.present(p.sessionKey, msgID, text, summary, question.Choices(q), func(data string) {
			a.handleResponse(p.requestID, data)
		})
		// Record where this question landed so a restart can address it for
		// cancel/expiry edits, then checkpoint. (If the ask was already
		// resolved by a racing click, p is gone from byReqID and persistLocked
		// simply won't include it — harmless.)
		a.mu.Lock()
		p.platformMsgID = platformMsgID
		a.persistLocked()
		a.mu.Unlock()
	}
}

// handleResponse records one answer and either presents the next question or,
// when all questions are answered, delivers the batch into the session. It is
// also the entry point for a typed answer routed by request id.
func (a *askState) handleResponse(requestID, data string) {
	a.mu.Lock()
	p := a.byReqID[requestID]
	if p == nil {
		a.mu.Unlock()
		return
	}
	q := p.acc.Current()
	if q == nil {
		a.mu.Unlock()
		return
	}
	answer, cancelled, err := question.ResolveAnswer(q, data)
	if err != nil {
		a.mu.Unlock()
		askLog.Warnf("session=%s req=%s invalid response %q: %v", p.sessionKey, requestID, data, err)
		return
	}
	if cancelled {
		answered, total, sk, reqID := p.acc.Index(), p.acc.Total(), p.sessionKey, p.requestID
		a.resolveLocked(p)
		a.deliverCancel(sk, reqID, answered, total)
		return
	}
	answeredIdx := p.acc.Index()
	p.acc.Record(answer)
	done := p.acc.Done()
	a.persistLocked() // checkpoint the advanced index / new answer
	a.mu.Unlock()

	// Close the just-answered question's on-screen message so its buttons can't
	// be re-clicked. On the button path the platform already edited it (so this
	// no-ops); on the TYPED path nothing else would clear the stale Cancel /
	// option buttons, so this is what makes a typed-answered question "close".
	if a.closeMsg != nil {
		a.closeMsg(questionMsgID(p.requestID, answeredIdx), "✅ "+answerEcho(answer))
	}

	if !done {
		a.presentCurrent(p)
		return
	}

	// All answered — assemble and deliver the batch.
	a.mu.Lock()
	a.resolveLocked(p)

	a.deliverBatch(p)
}

// deliverBatch assembles the final answers (running the grader if one is set) and
// delivers them into the session as a new inbound message. Shared by the
// sequential done-branch and the app's batched path so the two cannot drift.
// Caller has already removed p from the registry.
func (a *askState) deliverBatch(p *pendingAsk) {
	if p.grader.path == "" {
		a.deliverMsg(p.sessionKey, p.requestID, formatAnswerBatch(p.acc.Questions(), p.acc.Answers()))
		return
	}
	// A grader is set: it may run for up to its timeout. Run it (and deliver
	// its result) off the caller's goroutine so neither the platform button
	// callback nor a typed-answer turn blocks on the subprocess.
	go func() {
		raw := formatAnswerBatch(p.acc.Questions(), p.acc.Answers())
		total := p.acc.Total()
		a.deliverMsg(p.sessionKey, p.requestID, runGrader(p, p.acc.Questions(), p.acc.Answers(), raw, false, total, total))
	}()
}

// handleBatchResponse records every answer of a batched (app) ask at once and
// delivers the result. answers are positional RAW choice tokens (one per
// question, in order): "qa:<index>", the "qa:cancel" sentinel, or typed text. A
// cancel in any slot cancels the whole ask, mirroring the sequential Cancel
// button. This is the batched counterpart to handleResponse.
func (a *askState) handleBatchResponse(requestID string, answers []string) {
	a.mu.Lock()
	p := a.byReqID[requestID]
	if p == nil {
		a.mu.Unlock()
		return
	}
	askLog.Debugf("session=%s req=%s batch response: %d answers, accumulator at idx=%d/%d",
		p.sessionKey, requestID, len(answers), p.acc.Index(), p.acc.Total())
	for ansIdx, raw := range answers {
		q := p.acc.Current()
		if q == nil {
			break // more answers than questions — ignore extras defensively
		}
		answer, cancelled, err := question.ResolveAnswer(q, raw)
		if err != nil {
			a.mu.Unlock()
			askLog.Warnf("session=%s req=%s invalid batched response ans[%d]=%q against Q%d (%d options) %q: %v",
				p.sessionKey, requestID, ansIdx, raw, p.acc.Index(), len(q.Options), answerEcho(q.Question), err)
			return
		}
		if cancelled {
			answered, total, sk, reqID := p.acc.Index(), p.acc.Total(), p.sessionKey, p.requestID
			a.resolveLocked(p)
			a.deliverCancel(sk, reqID, answered, total)
			return
		}
		p.acc.Record(answer)
	}
	if !p.acc.Done() {
		// Fewer answers than questions. The app gates Submit on all-answered, so
		// this is not expected; don't deliver a partial batch — keep it pending
		// (a later complete submit, or the 24h expiry, resolves it).
		a.persistLocked()
		a.mu.Unlock()
		askLog.Warnf("session=%s req=%s batched response had %d of %d answers; awaiting rest",
			p.sessionKey, requestID, p.acc.Index(), p.acc.Total())
		return
	}
	a.resolveLocked(p)
	a.deliverBatch(p)
}

// removeLocked deletes a pending ask from both indexes and notifies the resolve
// hook FOR THAT ASK. Caller holds a.mu.
//
// Symmetric by construction (#1711): the session holds an ordered list, so
// removing any member — oldest, newest, or one still queued — leaves the rest
// exactly as they were, and onResolve fires once per ask rather than only when the
// session's set empties (ruling 5). Firing with the resolved ask's requestID is
// what lets the inbox release only the injections THAT ask was holding (#1712);
// the old "session emptied" signal could not distinguish them.
func (a *askState) removeLocked(p *pendingAsk) {
	delete(a.byReqID, p.requestID)
	ids := a.bySession[p.sessionKey]
	for i, id := range ids {
		if id == p.requestID {
			ids = append(ids[:i:i], ids[i+1:]...)
			break
		}
	}
	if len(ids) == 0 {
		delete(a.bySession, p.sessionKey)
	} else {
		a.bySession[p.sessionKey] = ids
	}
	// Notify async (off a.mu): the hook reaches into the agent inbox.
	if a.onResolve != nil {
		go a.onResolve(p.sessionKey, p.requestID)
	}
}

// resolveLocked removes p, checkpoints, RELEASES a.mu, and then presents the
// session's next queued ask if the slot is now free. Every resolution path
// (answered, cancelled, batched, /complete) funnels through it so promotion can
// never be forgotten at one of them. Caller holds a.mu and must not touch it after.
func (a *askState) resolveLocked(p *pendingAsk) {
	a.removeLocked(p)
	a.persistLocked()
	a.mu.Unlock()
	a.promoteQueued(p.sessionKey)
}

// persistedAsk is the JSON-serialisable form of one in-flight ask. The
// Accumulator's progress is flattened to (Questions, Idx, Answers); the grader's
// config travels alongside so a restored ask still grades.
type persistedAsk struct {
	RequestID     string              `json:"request_id"`
	SessionKey    string              `json:"session_key"`
	Questions     []question.Question `json:"questions"`
	Idx           int                 `json:"idx"`
	Answers       map[string]string   `json:"answers,omitempty"`
	OriginalInput json.RawMessage     `json:"original_input,omitempty"`
	CreatedAt     time.Time           `json:"created_at"`
	Grader        persistedGrader     `json:"grader,omitempty"`
	PlatformMsgID string              `json:"platform_msg_id,omitempty"`
	Paused        bool                `json:"paused,omitempty"`
	Batched       bool                `json:"batched,omitempty"`
	// Queued marks an ask that has never been shown, waiting behind the session's
	// primary. It must persist: a queued ask is WORSE to lose than a live one
	// because the user never saw it, and the agent is still waiting on an answer
	// (#1711 ruling Q3).
	Queued bool `json:"queued,omitempty"`
}

// persistedGrader mirrors graderConfig with exported, JSON-friendly fields
// (timeout as whole seconds).
type persistedGrader struct {
	Path        string   `json:"path,omitempty"`
	TimeoutSecs int      `json:"timeout_secs,omitempty"`
	OnError     string   `json:"on_error,omitempty"`
	Args        []string `json:"args,omitempty"`
}

// persistLocked writes the full set of in-flight asks to the session index.
// Caller holds a.mu. No-op when persistence is disabled. Best-effort: a write
// failure is logged, never propagated — persistence is a convenience, and an
// ask still works in-memory for the life of the process without it.
//
// The output is sorted by created_at (requestID breaking ties): byReqID is a Go
// map, so writing it in range order produced a differently-ordered blob on every
// write. restorePending sorts on the way back in, which is what actually makes the
// primary deterministic, but persisting in order keeps the stored artifact
// readable and stable instead of reshuffling on every keystroke.
func (a *askState) persistLocked() {
	if a.store == nil {
		return
	}
	ordered := make([]*pendingAsk, 0, len(a.byReqID))
	for _, p := range a.byReqID {
		ordered = append(ordered, p)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return askOlder(ordered[i].createdAt, ordered[i].requestID, ordered[j].createdAt, ordered[j].requestID)
	})
	out := make([]persistedAsk, 0, len(ordered))
	for _, p := range ordered {
		out = append(out, persistedAsk{
			RequestID:     p.requestID,
			SessionKey:    p.sessionKey,
			Questions:     p.acc.Questions(),
			Idx:           p.acc.Index(),
			Answers:       p.acc.Answers(),
			OriginalInput: p.originalInput,
			CreatedAt:     p.createdAt,
			Grader: persistedGrader{
				Path:        p.grader.path,
				TimeoutSecs: int(p.grader.timeout / time.Second),
				OnError:     p.grader.onError,
				Args:        p.grader.args,
			},
			PlatformMsgID: p.platformMsgID,
			Paused:        p.paused,
			Batched:       p.batched,
			Queued:        p.queued,
		})
	}
	data, err := json.Marshal(out)
	if err != nil {
		askLog.Warnf("marshal pending asks: %v", err)
		return
	}
	if err := a.store.SetAgentMetadata(a.agentID, askMetaKey, string(data)); err != nil {
		askLog.Warnf("persist pending asks: %v", err)
	}
}

// askOlder orders two asks by created_at, breaking ties on requestID so the order
// is total. Shared by persistLocked and restorePending so the written and the read
// order cannot disagree.
func askOlder(aAt time.Time, aID string, bAt time.Time, bID string) bool {
	if !aAt.Equal(bAt) {
		return aAt.Before(bAt)
	}
	return aID < bID
}

// restorePending rehydrates asks saved before a restart and re-attaches each
// one's interactive callback to the buttons the platform still displays. Stale
// asks (older than pendingAskTTL) and malformed/complete entries are dropped, and
// the cleaned set is re-persisted. Best-effort throughout: any decode failure
// leaves the tool empty rather than blocking startup.
//
// The saved entries are SORTED by created_at first (#1711). The stored order is
// whatever a Go map range produced when it was written — a real artifact holds
// three asks for one session stored 19:58 / 08:00 / 02:14 — so restoring in slice
// order re-elected a different primary on every restart, silently moving which ask
// owns typed answers and /pause. Sorted, the session's queue rehydrates
// oldest-first and the primary is the same every time.
func (a *askState) restorePending() {
	if a.store == nil {
		return
	}
	raw, err := a.store.GetAgentMetadata(a.agentID, askMetaKey)
	if err != nil || raw == "" {
		return
	}
	var saved []persistedAsk
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		askLog.Warnf("unmarshal pending asks: %v", err)
		return
	}

	sort.SliceStable(saved, func(i, j int) bool {
		return askOlder(saved[i].CreatedAt, saved[i].RequestID, saved[j].CreatedAt, saved[j].RequestID)
	})

	now := time.Now()
	restored := make([]*pendingAsk, 0, len(saved))
	sessions := make([]string, 0, len(saved))
	a.mu.Lock()
	for _, s := range saved {
		if now.Sub(s.CreatedAt) > pendingAskTTL {
			continue // stale — its buttons have expired on the platform too
		}
		if len(s.Questions) == 0 || s.Idx < 0 || s.Idx >= len(s.Questions) {
			continue // malformed or already complete — nothing to wait on
		}
		p := &pendingAsk{
			requestID:     s.RequestID,
			sessionKey:    s.SessionKey,
			acc:           question.NewAccumulatorAt(s.Questions, s.Idx, s.Answers),
			originalInput: s.OriginalInput,
			createdAt:     s.CreatedAt,
			grader: graderConfig{
				path:    s.Grader.Path,
				timeout: time.Duration(s.Grader.TimeoutSecs) * time.Second,
				onError: s.Grader.OnError,
				args:    s.Grader.Args,
			},
			platformMsgID: s.PlatformMsgID,
			paused:        s.Paused,
			batched:       s.Batched,
			queued:        s.Queued,
		}
		a.byReqID[p.requestID] = p
		if _, seen := a.bySession[p.sessionKey]; !seen {
			sessions = append(sessions, p.sessionKey)
		}
		a.bySession[p.sessionKey] = append(a.bySession[p.sessionKey], p.requestID)
		restored = append(restored, p)
	}
	a.persistLocked() // drop stale entries from the durable set
	a.mu.Unlock()

	// Re-attach outside the lock: the restore fn reaches into the platform layer.
	// A QUEUED ask has nothing on screen to re-attach to — re-binding it would
	// claim buttons that were never sent.
	for _, p := range restored {
		if p.queued {
			continue
		}
		a.reattach(p)
	}
	// A session whose live ask did not survive (expired, or every entry was
	// queued) still has a queue to drain — promoting here applies the same
	// presentation-time TTL as the live path rather than stranding it.
	for _, sk := range sessions {
		a.promoteQueued(sk)
	}
	if len(restored) > 0 {
		askLog.Debugf("restored %d pending ask(s) from state", len(restored))
	}
}

// reattach rebinds the interactive callback for p's CURRENT question to the
// buttons already on screen. It uses the same msgID presentCurrent first sent the
// question with, so the existing buttons' "im:<msgID>:<idx>" data still routes
// here. No new message is sent.
func (a *askState) reattach(p *pendingAsk) {
	// One restore mechanism, parameterised by how the ask was presented. A batched
	// (native-app) ask re-registers its server-side batched callback WITHOUT sending
	// a frame (the app still shows the form); a sequential ask re-binds its
	// on-screen buttons. Both rebind the ask-layer handler the LIVE path uses
	// (handleBatchResponse / handleResponse), so the two cannot drift.
	if p.acc.Index() == 0 && p.batched && a.restoreBatch != nil {
		reqID := p.requestID
		a.restoreBatch(p.sessionKey, questionMsgID(p.requestID, 0), p.acc.Total(), func(answers []string) {
			a.handleBatchResponse(reqID, answers)
		})
		return
	}
	if a.restore == nil {
		return
	}
	q := p.acc.Current()
	if q == nil {
		return
	}
	msgID := questionMsgID(p.requestID, p.acc.Index())
	a.restore(p.sessionKey, msgID, p.platformMsgID, question.Choices(q), func(data string) {
		a.handleResponse(p.requestID, data)
	})
}

// handleBatchByPrompt routes a batched answer identified by its on-screen promptID
// (rather than the requestID) to the SAME entry point a registered batched answer
// uses (handleBatchResponse). It is the ask-layer target for the app-hub fallback
// that catches a batched reply whose server-side registration a restart dropped
// (#1473, fix B). The batched promptID is question 0's msg id, so it maps back to
// the ask whose requestID yields it.
func (a *askState) handleBatchByPrompt(promptID string, answers []string) {
	a.mu.Lock()
	var reqID string
	for id := range a.byReqID {
		if questionMsgID(id, 0) == promptID {
			reqID = id
			break
		}
	}
	a.mu.Unlock()
	if reqID == "" {
		askLog.Warnf("batched answer for unregistered prompt %q: no matching pending ask (dropped)", promptID)
		return
	}
	a.handleBatchResponse(reqID, answers)
}

// deliverMsg delivers a message produced BY an ask back into its session.
// requestID names the ask it came from, so the inbox can tell this apart from a
// proactive interruption when deciding whether a pending ask should hold it
// (#1712).
func (a *askState) deliverMsg(sessionKey, requestID, msg string) {
	if a.deliver != nil {
		a.deliver(sessionKey, requestID, msg)
	}
}

// pendingForSession returns the request id of the session's PRIMARY ask — the
// oldest one actually on screen — or "" when the session has no live ask. Used by
// the inbound path to route a typed reply to the waiting ask instead of starting a
// fresh turn, and by the inbox's ask-deferral gate.
//
// A QUEUED ask deliberately reads as "nothing pending" (#1711 ruling Q4): it is on
// nobody's screen and can be answered by nothing, so it is racing no injection.
func (a *askState) pendingForSession(sessionKey string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p := a.primaryLocked(sessionKey); p != nil {
		return p.requestID
	}
	return ""
}

// setPaused toggles the pause flag on the session's PRIMARY ask (#1711 ruling 2)
// and checkpoints it. Returns false (a no-op) when no ask is live — the caller
// uses this to report "no active question". While paused, the inbound path's
// answer-capture guard is skipped so the user's typed replies run as normal turns.
func (a *askState) setPaused(sessionKey string, paused bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.primaryLocked(sessionKey)
	if p == nil {
		return false
	}
	p.paused = paused
	a.persistLocked()
	return true
}

// isPaused reports whether the session's PRIMARY ask is paused. False when
// nothing is live (so a stale flag can never strand a session).
func (a *askState) isPaused(sessionKey string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.primaryLocked(sessionKey)
	return p != nil && p.paused
}

// completeSession ends the session's pending ask early, delivering ONLY the
// already-answered questions to the agent and dropping the unanswered ones.
// Returns (answered, total, true) on success; (0, 0, false) when no ask is
// pending (caller reports "no active question"); (0, total, false) when an ask
// is pending but nothing has been answered yet (caller nudges — delivering an
// empty batch would wake the agent for nothing). A configured grader still runs,
// told the set is partial so it can grade a short answer set.
func (a *askState) completeSession(sessionKey string) (answered, total int, ok bool) {
	a.mu.Lock()
	p := a.primaryLocked(sessionKey)
	if p == nil {
		a.mu.Unlock()
		return 0, 0, false
	}
	idx := p.acc.Index()
	total = p.acc.Total()
	if idx == 0 {
		a.mu.Unlock()
		return 0, total, false // nothing answered yet
	}
	// Snapshot the answered subset under the lock. acc isn't mutated after the
	// ask is removed, so these stay safe to read from the grader goroutine below.
	answeredQs := p.acc.Questions()[:idx]
	answers := p.acc.Answers()
	sess := p.sessionKey
	curMsgID := questionMsgID(p.requestID, idx) // the still-displayed question
	hasGrader := p.grader.path != ""
	a.resolveLocked(p)

	// Close the still-live current question so its Cancel/option buttons can't be
	// clicked now that the ask is gone (the button/typed paths self-close; a
	// /complete is neither, so it must close explicitly).
	if a.closeMsg != nil {
		a.closeMsg(curMsgID, "⏹ Completed early")
	}

	raw := formatPartialBatch(answeredQs, answers, idx, total)
	if !hasGrader {
		a.deliverMsg(sess, p.requestID, raw)
		return idx, total, true
	}
	// Grader configured: run it on the partial set off the caller's goroutine
	// (mirrors the full-answer path) so the slash-command handler returns at once;
	// the graded result is delivered to the agent when ready.
	go func() {
		a.deliverMsg(sess, p.requestID, runGrader(p, answeredQs, answers, raw, true, idx, total))
	}()
	return idx, total, true
}

// formatAnswerBatch renders the collected answers as the inbound user message
// the asking agent receives. Includes a readable Q→A list plus a compact JSON
// line so the agent can parse the result deterministically if it wants to.
func formatAnswerBatch(qs []question.Question, answers map[string]string) string {
	var b strings.Builder
	b.WriteString("[SYSTEM: the user answered your `ask` request. These are their selections — act on them.]\n")
	writeAnswerBody(&b, qs, answers)
	return b.String()
}

// formatPartialBatch renders the inbound message for an ask completed EARLY (via
// /complete): the same Q→A body + JSON payload as a full batch, but a preamble
// that tells the agent the remaining questions were left unanswered on purpose,
// so it acts on what it has rather than re-asking the rest.
func formatPartialBatch(qs []question.Question, answers map[string]string, answered, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[SYSTEM: the user completed your `ask` request early, answering %d of %d questions. Only the answered selections follow; the remaining %d were left unanswered on purpose — act on what you have, don't re-ask the rest unless genuinely needed.]\n", answered, total, total-answered)
	writeAnswerBody(&b, qs, answers)
	return b.String()
}

// writeAnswerBody appends the human-readable "• header → answer" lines and the
// compact JSON payload (answers + optional answers_by_id) to b. Shared by the
// full and partial batch formatters so their body shape can't drift.
func writeAnswerBody(b *strings.Builder, qs []question.Question, answers map[string]string) {
	for _, q := range qs {
		ans := answers[q.Question]
		label := q.Header
		if label == "" {
			label = q.Question
		}
		fmt.Fprintf(b, "\n• %s → %s", label, ans)
	}
	payload := map[string]any{"answers": answers}
	if byID := answersByID(qs, answers); len(byID) > 0 {
		payload["answers_by_id"] = byID
	}
	if js, err := json.Marshal(payload); err == nil {
		b.WriteString("\n\n")
		b.Write(js)
	}
}

// answersByID maps each question's optional opaque ID to its answer, including
// only questions that supplied an id. Empty when no question carried one.
func answersByID(qs []question.Question, answers map[string]string) map[string]string {
	byID := make(map[string]string, len(qs))
	for _, q := range qs {
		if q.ID != "" {
			byID[q.ID] = answers[q.Question]
		}
	}
	return byID
}

// graderInput is the JSON document handed to a grader executable on stdin. It
// carries the full question objects (for context: headers, offered options) plus
// the user's answers keyed by question text. request_id is also passed as argv[1].
//
// When the ask was completed EARLY (via /complete) only the answered questions
// are present and Partial is true, with Answered/Total describing how short the
// set is — a grader is expected to handle this (e.g. score only what was given).
// For a fully-answered ask Partial is false and the count fields are omitted, so
// existing graders see an unchanged payload.
type graderInput struct {
	RequestID   string              `json:"request_id"`
	Questions   []question.Question `json:"questions"`
	Answers     map[string]string   `json:"answers"`
	AnswersByID map[string]string   `json:"answers_by_id,omitempty"`
	Partial     bool                `json:"partial,omitempty"`
	Answered    int                 `json:"answered,omitempty"`
	Total       int                 `json:"total,omitempty"`
}

// runGrader executes the configured grader with the answers as JSON on stdin and
// returns the message to deliver to the asking agent. On success the grader's
// stdout (verbatim, capped) replaces the raw answer batch. On any failure —
// missing/blank output is allowed; non-zero exit, timeout, or launch error is not
// — it applies the on-error policy so the user's real answers are never lost.
func runGrader(p *pendingAsk, qs []question.Question, answers map[string]string, rawBatch string, partial bool, answered, total int) string {
	payload, err := json.Marshal(graderInput{
		RequestID:   p.requestID,
		Questions:   qs,
		Answers:     answers,
		AnswersByID: answersByID(qs, answers),
		Partial:     partial,
		Answered:    answered,
		Total:       total,
	})
	if err != nil {
		return graderErrorMsg(p, rawBatch, fmt.Sprintf("encode grader input: %v", err))
	}

	timeout := p.grader.timeout
	if timeout <= 0 {
		timeout = defaultGraderTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// procx.Spawn (not raw exec): strips the foci-secrets supplementary group so
	// the grader cannot inherit secret-file access from the foci process.
	// argv = [request_id, ...grader_args]: request_id stays at the stable argv[1]
	// slot; any agent-supplied grader_args follow as a pure vector (no shell, so
	// no injection surface), letting the grader learn context like the source file.
	spawnArgs := append([]string{p.requestID}, p.grader.args...)
	var stdout, stderr bytes.Buffer
	// procx.RunWithETXTBSYRetry retries a transient "text file busy" from
	// concurrent fork/exec (golang/go#22315); the build closure rebuilds the
	// Cmd with a fresh stdin reader and reset buffers each attempt. Safe to
	// retry: the grader has no side effects until it actually starts.
	runErr := procx.RunWithETXTBSYRetry(ctx, func() *exec.Cmd {
		stdout.Reset()
		stderr.Reset()
		cmd := procx.Spawn(ctx, p.grader.path, spawnArgs...)
		cmd.Stdin = bytes.NewReader(payload)
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		return cmd
	})

	if ctx.Err() == context.DeadlineExceeded {
		return graderErrorMsg(p, rawBatch, fmt.Sprintf("grader timed out after %s", timeout))
	}
	if runErr != nil {
		msg := fmt.Sprintf("grader failed: %v", runErr)
		if se := strings.TrimSpace(stderr.String()); se != "" {
			msg += "; stderr: " + truncateStr(se, 1024)
		}
		return graderErrorMsg(p, rawBatch, msg)
	}

	out := truncateStr(strings.TrimRight(stdout.String(), "\n"), maxGraderOutput)
	return fmt.Sprintf("[SYSTEM: graded result for your `ask` request (req %s) — act on this.]\n%s", p.requestID, out)
}

// graderErrorMsg builds the delivery message when a grader cannot produce a valid
// result. Default ("fallback") delivers the raw answer batch plus a brief note;
// "report" leads with the failure so the agent can decide, still including the raw
// answers. Either way the user's selections survive a broken grader.
func graderErrorMsg(p *pendingAsk, rawBatch, reason string) string {
	askLog.Warnf("session=%s req=%s grader failure: %s", p.sessionKey, p.requestID, reason)
	if p.grader.onError == graderOnErrorReport {
		return fmt.Sprintf("[SYSTEM: your `ask` grader FAILED (%s). The user's raw answers follow — decide how to proceed.]\n\n%s", reason, rawBatch)
	}
	return fmt.Sprintf("%s\n\n[note: ask grader could not run (%s); showing raw answers.]", rawBatch, reason)
}

// truncateStr caps s at max bytes, appending an elision marker when it cuts.
func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n…[truncated]"
}

// askInput is the tool's input — identical shape to AskUserQuestion, minus any
// item cap.
type askInput struct {
	Questions []question.Question `json:"questions"`
	// Grader, if set, is an absolute path to an executable. When the user finishes
	// answering, foci runs it with {request_id, questions, answers} as JSON on stdin
	// and delivers its stdout to the agent instead of the raw answers.
	Grader               string   `json:"grader,omitempty"`
	GraderArgs           []string `json:"grader_args,omitempty"`
	GraderTimeoutSeconds int      `json:"grader_timeout_seconds,omitempty"`
	GraderOnError        string   `json:"grader_on_error,omitempty"`
}

// AskRouter exposes the typed-answer routing hooks so the inbound-message path
// can divert a user's typed reply to a waiting ask (instead of starting a fresh
// turn). PendingForSession reports the request id of the session's PRIMARY ask —
// the oldest one on screen, "" if none is live and "" for a session holding only
// QUEUED asks; HandleResponse feeds a typed answer to it.
type AskRouter struct {
	PendingForSession func(sessionKey string) string
	HandleResponse    func(requestID, data string)
	// HandleBatchByPrompt feeds a batched (native-app) answer set — identified by
	// its on-screen promptID — into the waiting ask. Used by the app hub as a
	// fallback when a restart dropped the in-memory batched registration (#1473).
	HandleBatchByPrompt func(promptID string, answers []string)
	// PauseSession / ResumeSession toggle answer-capture for a session's PRIMARY
	// ask (#1711 ruling 2 — the platforms carrying these commands only ever have
	// one live ask). While paused, the inbound routing guard skips answer-capture so the
	// user's typed replies run as normal turns; buttons still resolve the ask.
	// Both return false when no ask is pending (the command reports a no-op).
	PauseSession  func(sessionKey string) bool
	ResumeSession func(sessionKey string) bool
	// IsPaused reports whether the session's pending ask is paused. Read by the
	// inbound routing guard (run_turn.go) and the statusline paused-reminder field.
	IsPaused func(sessionKey string) bool
	// CompleteSession ends the session's pending ask early, delivering only the
	// already-answered questions to the agent (a grader still runs, told the set
	// is partial). Returns (answered, total, true) on success; ok=false with
	// total==0 when no ask is pending, or total>0 when nothing is answered yet.
	CompleteSession func(sessionKey string) (answered, total int, ok bool)
}

// NewAskTool builds the `ask` / `foci_ask` tool. present shows questions to the
// user; restore re-attaches a pending question's buttons after a restart; deliver
// injects the answer batch back into the calling session; closeMsg edits a
// question's message shut when it is answered by typing (the button path closes
// itself). All are supplied by cmd/foci-gw where the platform + agent wiring
// lives. store+agentID persist
// in-flight asks across restarts (store may be nil to disable). The returned
// AskRouter is wired into the inbound path for typed ("Other") answers.
//
// Construction rehydrates any asks that were in flight before a restart and
// re-binds their interactive callbacks, so an outstanding question keeps working.
func NewAskTool(present AskPresentFn, restore AskRestoreFn, deliver AskDeliverFn, closeMsg AskCloseFn, store *session.SessionIndex, agentID string, opts ...AskOption) (*Tool, *AskRouter) {
	state := newAskState(present, restore, deliver, closeMsg, store, agentID)
	for _, opt := range opts {
		opt(state)
	}
	state.restorePending()
	t := &Tool{
		Name:        "ask",
		ExecExport:  true,
		Positional:  []string{"questions"},
		Description: "Ask the user one or more questions with selectable options, and receive their answers. Mirrors the built-in AskUserQuestion but with NO limit on the number of questions or options. Options are OPTIONAL per question: omit them for an open-ended, typed-answer-only prompt (presented with just a Cancel button; the user types their reply). ASYNC: this call returns immediately after posting the first question — it does NOT block waiting for an answer. End your turn after calling it; the user's answers arrive later as a new message. Input is JSON only: pass {\"questions\":[...]} as a positional arg, via --json, or piped on stdin.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"questions": {
					"type": "array",
					"description": "Questions to ask, presented one at a time. No cap on count.",
					"items": {
						"type": "object",
						"properties": {
							"question": {"type": "string", "description": "The question text shown to the user"},
							"id": {"type": "string", "description": "Optional opaque identifier, NOT shown to the user. If supplied it is preserved in the output under \"answers_by_id\" (id → answer) so you can correlate answers deterministically without matching on question text."},
							"header": {"type": "string", "description": "Short label/category for the question (used as the prompt summary)"},
							"multiSelect": {"type": "boolean", "description": "Reserved; currently single-select only"},
							"options": {
								"type": "array",
								"description": "Selectable options, shown as buttons. No cap on count. OPTIONAL: omit (or pass an empty array) for a typed-answer-only question — it is presented with just a Cancel button and the user answers by typing their reply.",
								"items": {
									"type": "object",
									"properties": {
										"label": {"type": "string", "description": "The option text shown on the button"},
										"description": {"type": "string", "description": "Optional longer explanation shown in the question body"}
									},
									"required": ["label"]
								}
							}
						},
						"required": ["question"]
					}
				},
				"grader": {"type": "string", "description": "Optional absolute path to an executable. When the user finishes answering, foci runs it with {request_id, questions, answers} as JSON on stdin; its stdout is delivered to you INSTEAD of the raw answers. Use for deterministic post-processing (quiz grading, answer normalization, lookups)."},
				"grader_args": {"type": "array", "items": {"type": "string"}, "description": "Optional extra argv for the grader, appended AFTER request_id: the grader is run as [path, request_id, ...grader_args]. Pure vector (no shell), so safe for arbitrary strings — use it to pass context the grader needs, e.g. the source quiz filename. Ignored if 'grader' is unset."},
				"grader_timeout_seconds": {"type": "integer", "description": "Hard timeout for the grader in seconds (default 15). Past it the grader is killed and the user's raw answers are delivered instead."},
				"grader_on_error": {"type": "string", "enum": ["fallback", "report"], "description": "What to deliver if the grader fails (non-zero exit / timeout / launch error). 'fallback' (default): the raw answers plus a brief note. 'report': a failure report plus the raw answers. The user's answers are never lost either way."}
			},
			"required": ["questions"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			sessionKey := SessionKeyFromContext(ctx)
			if sessionKey == "" {
				return ToolResult{}, fmt.Errorf("ask: no session in context (cannot route answers back)")
			}
			var in askInput
			if err := json.Unmarshal(params, &in); err != nil {
				return ToolResult{}, fmt.Errorf("ask: parse input: %w", err)
			}
			if len(in.Questions) == 0 {
				return ToolResult{}, fmt.Errorf("ask: at least one question is required")
			}
			for i, q := range in.Questions {
				if q.Question == "" {
					return ToolResult{}, fmt.Errorf("ask: question %d has an empty %q field", i+1, "question")
				}
				// A question with no options is allowed: it is presented with only a
				// Cancel button and answered by typing (the typed-answer path resolves
				// the freeform reply). Use this for open-ended prompts where buttons
				// don't fit.
			}

			// Validate the optional grader up front so the agent learns about a bad
			// path now (at call time), not later when the answers arrive.
			var grader graderConfig
			if in.Grader != "" {
				if !filepath.IsAbs(in.Grader) {
					return ToolResult{}, fmt.Errorf("ask: grader must be an absolute path, got %q", in.Grader)
				}
				fi, err := os.Stat(in.Grader)
				if err != nil {
					return ToolResult{}, fmt.Errorf("ask: grader %q: %w", in.Grader, err)
				}
				if fi.IsDir() || fi.Mode()&0o111 == 0 {
					return ToolResult{}, fmt.Errorf("ask: grader %q is not an executable file", in.Grader)
				}
				grader.path = in.Grader
				grader.args = in.GraderArgs
				if in.GraderTimeoutSeconds > 0 {
					grader.timeout = time.Duration(in.GraderTimeoutSeconds) * time.Second
				}
				grader.onError = graderOnErrorFallback
				if in.GraderOnError != "" {
					if in.GraderOnError != graderOnErrorFallback && in.GraderOnError != graderOnErrorReport {
						return ToolResult{}, fmt.Errorf("ask: grader_on_error must be %q or %q, got %q", graderOnErrorFallback, graderOnErrorReport, in.GraderOnError)
					}
					grader.onError = in.GraderOnError
				}
			} else if len(in.GraderArgs) > 0 {
				return ToolResult{}, fmt.Errorf("ask: grader_args set but no grader executable given")
			}

			reqID, queued := state.start(sessionKey, in.Questions, params, grader)
			status, note := "asked", "Posted to the user. Their answers will arrive later as a new message — end your turn now; do not wait."
			if queued {
				status, note = "queued", "NOT posted yet: this session already has a question on the user's screen, and this transport cannot show two at once. Your ask is queued and will be presented when that one is answered. Its answers still arrive later as a new message — end your turn now; do not wait, and do not re-ask."
			}
			out, _ := json.Marshal(map[string]any{
				"status":     status,
				"request_id": reqID,
				"questions":  len(in.Questions),
				"note":       note,
			})
			return TextResult(string(out)), nil
		},
	}
	router := &AskRouter{
		PendingForSession:   state.pendingForSession,
		HandleResponse:      state.handleResponse,
		HandleBatchByPrompt: state.handleBatchByPrompt,
		PauseSession:        func(sk string) bool { return state.setPaused(sk, true) },
		ResumeSession:       func(sk string) bool { return state.setPaused(sk, false) },
		IsPaused:            state.isPaused,
		CompleteSession:     state.completeSession,
	}
	return t, router
}

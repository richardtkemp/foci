package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/log"
	"foci/internal/procx"
	"foci/internal/question"
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
type AskPresentFn func(sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string))

// AskDeliverFn delivers the assembled answer batch into sessionKey as a new
// inbound user message, waking the agent. Backend-agnostic (delegated + API).
type AskDeliverFn func(sessionKey, message string)

// pendingAsk is one in-flight ask: a sequential accumulator plus the context
// needed to present follow-up questions and deliver the final answers.
type pendingAsk struct {
	requestID     string
	sessionKey    string
	acc           *question.Accumulator
	originalInput json.RawMessage
	createdAt     time.Time
	grader        graderConfig
}

// askState is the in-memory registry of in-flight asks. Keyed by requestID for
// click routing and by sessionKey for typed-answer routing (latest ask wins).
// Purely in-memory: a foci restart drops pending asks (acceptable — the buttons
// also live in the platform's in-memory store with the same 24h lifetime).
type askState struct {
	mu        sync.Mutex
	byReqID   map[string]*pendingAsk
	bySession map[string]string // sessionKey → latest requestID
	present   AskPresentFn
	deliver   AskDeliverFn
	seq       atomic.Int64
}

func newAskState(present AskPresentFn, deliver AskDeliverFn) *askState {
	return &askState{
		byReqID:   make(map[string]*pendingAsk),
		bySession: make(map[string]string),
		present:   present,
		deliver:   deliver,
	}
}

// nextRequestID returns a unique, colon-free request id. Colon-free matters:
// the platform encodes button data as "<id>:<index>" and splits on the first
// colon, so an id containing ':' would break click routing.
func (a *askState) nextRequestID() string {
	return fmt.Sprintf("ask-%d", a.seq.Add(1))
}

// start registers a new ask and presents its first question.
func (a *askState) start(sessionKey string, qs []question.Question, originalInput json.RawMessage, grader graderConfig) string {
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
	a.byReqID[reqID] = p
	a.bySession[sessionKey] = reqID
	a.mu.Unlock()

	a.presentCurrent(p)
	return reqID
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
	msgID := fmt.Sprintf("%s-q%d", p.requestID, idx)
	if a.present != nil {
		a.present(p.sessionKey, msgID, text, summary, question.Choices(q), func(data string) {
			a.handleResponse(p.requestID, data)
		})
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
		log.Warnf("ask", "session=%s req=%s invalid response %q: %v", p.sessionKey, requestID, data, err)
		return
	}
	if cancelled {
		a.removeLocked(p)
		a.mu.Unlock()
		a.deliverMsg(p.sessionKey, fmt.Sprintf(
			"[SYSTEM: the user CANCELLED your `ask` request after %d of %d answers. Do not retry unless they ask.]",
			p.acc.Index(), p.acc.Total()))
		return
	}
	p.acc.Record(answer)
	done := p.acc.Done()
	a.mu.Unlock()

	if !done {
		a.presentCurrent(p)
		return
	}

	// All answered — assemble and deliver the batch.
	a.mu.Lock()
	a.removeLocked(p)
	a.mu.Unlock()

	if p.grader.path == "" {
		a.deliverMsg(p.sessionKey, formatAnswerBatch(p.acc.Questions(), p.acc.Answers()))
		return
	}
	// A grader is set: it may run for up to its timeout. Run it (and deliver
	// its result) off the caller's goroutine so neither the platform button
	// callback nor a typed-answer turn blocks on the subprocess.
	go func() {
		raw := formatAnswerBatch(p.acc.Questions(), p.acc.Answers())
		a.deliverMsg(p.sessionKey, runGrader(p, p.acc.Questions(), p.acc.Answers(), raw))
	}()
}

// removeLocked deletes a pending ask from both indexes. Caller holds a.mu.
func (a *askState) removeLocked(p *pendingAsk) {
	delete(a.byReqID, p.requestID)
	if a.bySession[p.sessionKey] == p.requestID {
		delete(a.bySession, p.sessionKey)
	}
}

func (a *askState) deliverMsg(sessionKey, msg string) {
	if a.deliver != nil {
		a.deliver(sessionKey, msg)
	}
}

// pendingForSession returns the request id of the latest in-flight ask for a
// session (empty if none). Used by the inbound path to route a typed reply to a
// waiting ask instead of starting a fresh turn.
func (a *askState) pendingForSession(sessionKey string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bySession[sessionKey]
}

// formatAnswerBatch renders the collected answers as the inbound user message
// the asking agent receives. Includes a readable Q→A list plus a compact JSON
// line so the agent can parse the result deterministically if it wants to.
func formatAnswerBatch(qs []question.Question, answers map[string]string) string {
	var b strings.Builder
	b.WriteString("[SYSTEM: the user answered your `ask` request. These are their selections — act on them.]\n")
	for _, q := range qs {
		ans := answers[q.Question]
		label := q.Header
		if label == "" {
			label = q.Question
		}
		fmt.Fprintf(&b, "\n• %s → %s", label, ans)
	}
	payload := map[string]any{"answers": answers}
	if byID := answersByID(qs, answers); len(byID) > 0 {
		payload["answers_by_id"] = byID
	}
	if js, err := json.Marshal(payload); err == nil {
		b.WriteString("\n\n")
		b.Write(js)
	}
	return b.String()
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
type graderInput struct {
	RequestID   string              `json:"request_id"`
	Questions   []question.Question `json:"questions"`
	Answers     map[string]string   `json:"answers"`
	AnswersByID map[string]string   `json:"answers_by_id,omitempty"`
}

// runGrader executes the configured grader with the answers as JSON on stdin and
// returns the message to deliver to the asking agent. On success the grader's
// stdout (verbatim, capped) replaces the raw answer batch. On any failure —
// missing/blank output is allowed; non-zero exit, timeout, or launch error is not
// — it applies the on-error policy so the user's real answers are never lost.
func runGrader(p *pendingAsk, qs []question.Question, answers map[string]string, rawBatch string) string {
	payload, err := json.Marshal(graderInput{
		RequestID:   p.requestID,
		Questions:   qs,
		Answers:     answers,
		AnswersByID: answersByID(qs, answers),
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
	cmd := procx.Spawn(ctx, p.grader.path, p.requestID)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

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
	log.Warnf("ask", "session=%s req=%s grader failure: %s", p.sessionKey, p.requestID, reason)
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
	Grader               string `json:"grader,omitempty"`
	GraderTimeoutSeconds int    `json:"grader_timeout_seconds,omitempty"`
	GraderOnError        string `json:"grader_on_error,omitempty"`
}

// AskRouter exposes the typed-answer routing hooks so the inbound-message path
// can divert a user's typed reply to a waiting ask (instead of starting a fresh
// turn). PendingForSession reports the request id of the latest in-flight ask
// for a session (empty if none); HandleResponse feeds a typed answer to it.
type AskRouter struct {
	PendingForSession func(sessionKey string) string
	HandleResponse    func(requestID, data string)
}

// NewAskTool builds the `ask` / `foci_ask` tool. present shows questions to the
// user; deliver injects the answer batch back into the calling session. Both
// are supplied by cmd/foci-gw where the platform + agent wiring lives. The
// returned AskRouter is wired into the inbound path for typed ("Other") answers.
func NewAskTool(present AskPresentFn, deliver AskDeliverFn) (*Tool, *AskRouter) {
	state := newAskState(present, deliver)
	t := &Tool{
		Name:        "ask",
		ExecExport:  true,
		Positional:  []string{"questions"},
		Description: "Ask the user one or more questions with selectable options, and receive their answers. Mirrors the built-in AskUserQuestion but with NO limit on the number of questions or options. ASYNC: this call returns immediately after posting the first question — it does NOT block waiting for an answer. End your turn after calling it; the user's answers arrive later as a new message. Input is JSON only: pass {\"questions\":[...]} as a positional arg, via --json, or piped on stdin.",
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
								"description": "Selectable options. No cap on count.",
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
						"required": ["question", "options"]
					}
				},
				"grader": {"type": "string", "description": "Optional absolute path to an executable. When the user finishes answering, foci runs it with {request_id, questions, answers} as JSON on stdin; its stdout is delivered to you INSTEAD of the raw answers. Use for deterministic post-processing (quiz grading, answer normalization, lookups)."},
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
					return ToolResult{}, fmt.Errorf("ask: question %d has empty text", i+1)
				}
				if len(q.Options) == 0 {
					return ToolResult{}, fmt.Errorf("ask: question %d (%q) has no options", i+1, q.Question)
				}
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
			}

			reqID := state.start(sessionKey, in.Questions, params, grader)
			out, _ := json.Marshal(map[string]any{
				"status":     "asked",
				"request_id": reqID,
				"questions":  len(in.Questions),
				"note":       "Posted to the user. Their answers will arrive later as a new message — end your turn now; do not wait.",
			})
			return TextResult(string(out)), nil
		},
	}
	router := &AskRouter{
		PendingForSession: state.pendingForSession,
		HandleResponse:    state.handleResponse,
	}
	return t, router
}

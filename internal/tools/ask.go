package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/log"
	"foci/internal/question"
)

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
func (a *askState) start(sessionKey string, qs []question.Question, originalInput json.RawMessage) string {
	reqID := a.nextRequestID()
	p := &pendingAsk{
		requestID:     reqID,
		sessionKey:    sessionKey,
		acc:           question.NewAccumulator(qs),
		originalInput: originalInput,
		createdAt:     time.Now(),
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
	a.deliverMsg(p.sessionKey, formatAnswerBatch(p.acc.Questions(), p.acc.Answers()))
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
	if js, err := json.Marshal(map[string]any{"answers": answers}); err == nil {
		b.WriteString("\n\n")
		b.Write(js)
	}
	return b.String()
}

// askInput is the tool's input — identical shape to AskUserQuestion, minus any
// item cap.
type askInput struct {
	Questions []question.Question `json:"questions"`
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
				}
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

			reqID := state.start(sessionKey, in.Questions, params)
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

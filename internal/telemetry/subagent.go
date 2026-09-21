package telemetry

import (
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// A CC subagent run is a child of the turn that spawned it, but its events
// can arrive on a LATER turn's sink: a background Agent outlives its parent,
// and its text/end events reach whichever sink the session router holds at
// the time. So open subagent spans live in a package registry keyed by
// (session, group key, run), not on the Turn — the turn that sees the end
// closes a span the turn that saw the start opened.

type subagentRun struct {
	span    trace.Span
	turnID  string // the SPAWNING turn — the trace this run belongs to
	label   string
	texts   []string
	prompts []string
	started time.Time
}

var (
	subMu   sync.Mutex
	subRuns = map[string]*subagentRun{}
)

// subagentTTL bounds how long an un-ended run is kept; the ccstream tracker
// prunes its own pending subagents at 30 min, so anything older here has
// lost its end signal and is closed as unresolved.
const subagentTTL = 2 * time.Hour

func subKey(session, group string, run int) string {
	if run <= 1 {
		return session + "|" + group
	}
	return session + "|" + group + "|" + itoa(run)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// SubagentStart opens an "agent" child under this turn's root for the run.
// groupKey is the Agent tool's tool_use id — also api.db's subagent_id for
// the run's spend (#1946; agent_id there is the OWNING agent, not this),
// which is why SubagentSpanID keys on it.
func (t *Turn) SubagentStart(groupKey, label, prompt string, run int) {
	if t == nil {
		return
	}
	tr, o, r, ok := current()
	if !ok {
		return
	}
	t.mu.Lock()
	if !t.begun || t.ended {
		t.mu.Unlock()
		return
	}
	t.subagents++
	turnID, sessionKey, agentID, rootCtx := t.info.TurnID, t.info.SessionKey, t.info.AgentID, t.ctx
	t.mu.Unlock()

	subMu.Lock()
	defer subMu.Unlock()
	pruneSubagentsLocked()
	key := subKey(sessionKey, groupKey, run)
	if _, dup := subRuns[key]; dup {
		return
	}
	name := "subagent"
	if label != "" {
		name = "subagent: " + label
	}
	attrs := []attribute.KeyValue{
		attribute.String(attrObsType, "agent"),
		attribute.String(attrObsMetaPrefix+"tool_use_id", groupKey),
		attribute.Int(attrObsMetaPrefix+"run", run),
		attribute.String(attrObsMetaPrefix+"label", label),
		attribute.Int(attrObsMetaPrefix+"input_chars", len(prompt)),
		attribute.String(attrUserID, agentID),
		attribute.String(attrSessionID, sessionKey),
		attribute.String(attrEnvironment, o.Environment),
	}
	if o.Content {
		in, n := field(o, r, prompt)
		attrs = append(attrs, attribute.String(attrObsInput, in))
		if n > 0 {
			attrs = append(attrs, attribute.Int(attrObsMetaPrefix+"input_redactions", n))
		}
	}
	ctx := withIDs(rootCtx, trace.TraceID{}, SubagentSpanID(turnID, groupKey, run))
	_, span := tr.Start(ctx, name, trace.WithAttributes(attrs...))
	subRuns[key] = &subagentRun{span: span, turnID: turnID, label: label, started: time.Now()}
}

// SubagentText appends a text block the run produced.
func (t *Turn) SubagentText(groupKey, text string, run int) {
	if t == nil || text == "" {
		return
	}
	if _, _, _, ok := current(); !ok {
		return
	}
	session := t.session()
	if session == "" {
		return
	}
	subMu.Lock()
	defer subMu.Unlock()
	if sr := subRuns[subKey(session, groupKey, run)]; sr != nil {
		sr.texts = append(sr.texts, text)
	}
}

// SubagentPrompt records a SendMessage follow-up to an already-running run.
func (t *Turn) SubagentPrompt(groupKey, prompt string, run int) {
	if t == nil || prompt == "" {
		return
	}
	if _, _, _, ok := current(); !ok {
		return
	}
	session := t.session()
	if session == "" {
		return
	}
	subMu.Lock()
	defer subMu.Unlock()
	if sr := subRuns[subKey(session, groupKey, run)]; sr != nil {
		sr.prompts = append(sr.prompts, prompt)
	}
}

// SubagentEnd closes the run's span with its collected output.
func (t *Turn) SubagentEnd(groupKey string, run int) {
	if t == nil {
		return
	}
	_, o, r, ok := current()
	if !ok {
		return
	}
	session := t.session()
	if session == "" {
		return
	}
	subMu.Lock()
	sr := subRuns[subKey(session, groupKey, run)]
	delete(subRuns, subKey(session, groupKey, run))
	subMu.Unlock()
	if sr == nil {
		return
	}
	endSubagent(sr, o, r, false)
}

func endSubagent(sr *subagentRun, o Options, r *Redactor, unresolved bool) {
	out := strings.Join(sr.texts, "\n\n")
	sr.span.SetAttributes(
		attribute.Int(attrObsMetaPrefix+"text_blocks", len(sr.texts)),
		attribute.Int(attrObsMetaPrefix+"output_chars", len(out)),
		attribute.Int(attrObsMetaPrefix+"follow_up_prompts", len(sr.prompts)),
	)
	if unresolved {
		sr.span.SetAttributes(attribute.Bool(attrObsMetaPrefix+"end_unobserved", true))
	}
	if o.Content {
		s, n := field(o, r, out)
		sr.span.SetAttributes(attribute.String(attrObsOutput, s))
		if n > 0 {
			sr.span.SetAttributes(attribute.Int(attrObsMetaPrefix+"output_redactions", n))
		}
		if len(sr.prompts) > 0 {
			p, _ := field(o, r, strings.Join(sr.prompts, "\n\n---\n\n"))
			sr.span.SetAttributes(attribute.String(attrObsMetaPrefix+"follow_ups", p))
		}
	}
	sr.span.End()
}

// pruneSubagentsLocked closes runs that never received an end signal.
func pruneSubagentsLocked() {
	_, o, r, ok := current()
	if !ok {
		return
	}
	cutoff := time.Now().Add(-subagentTTL)
	for k, sr := range subRuns {
		if sr.started.Before(cutoff) {
			delete(subRuns, k)
			endSubagent(sr, o, r, true)
		}
	}
}

func (t *Turn) session() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.begun {
		return ""
	}
	return t.info.SessionKey
}

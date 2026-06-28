// inject.go — Inject routing + sendPrompt / sendCommand / flushSteerBuf.
//
// Inject is the single funnel for every user-role event foci sends to
// opencode (primary user turns, urgent steers, queued follow-ups, slash
// commands). It dispatches on inj.Source + IsTurnInFlight per the
// routing matrix in OPENCODE_DELEGATOR_PLAN.md §6.
//
// opencode's wire shape for prompts is POST /session/:id/prompt_async
// with body {parts: [...], noReply?: true}. Attachments become file
// parts with data: URLs. Slash commands go through POST /session/:id/
// command. The async endpoint returns 204 immediately — turn
// completion arrives as session.idle SSE events handled in Step 7.
//
// Steer divergence from ccstream (plan §Steer): opencode has no
// mid-turn queue, so SourceSteer arriving mid-turn is buffered in
// b.steerBuf and flushed via flushSteerBuf when the dispatcher's
// OnSessionIdle fires (Step 7). ccstream's "fold into the current
// ask()" has no opencode equivalent.

package opencode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"foci/internal/delegator"
	"foci/internal/log"
)

// ---------------------------------------------------------------------------
// Turn-lifecycle primitives (plan §6.2: "identical semantics to ccstream")
// ---------------------------------------------------------------------------

// AttachSessionEvents installs the session-scoped delivery sink. Called
// once by the agent layer when the Backend is acquired for a session;
// text/tool events flow through it regardless of whether a per-turn
// TurnEvents handler is armed. Mirrors ccstream's implementation.
func (b *Backend) AttachSessionEvents(events *delegator.SessionEvents) {
	b.sessionEvents.Store(events)
}

// beginTurn sets per-turn bookkeeping under turnMu. Resets accumulated
// state from any prior turn so a fresh turn starts clean — part of the
// beginTurn contract asserted by TestBeginTurnResetsState. Called from
// injectUser / injectSteer / flushSteerBuf before sendPrompt, so events
// arriving in response to the POST find an active turn.
func (b *Backend) beginTurn(turn *delegator.TurnEvents) {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	b.turnActive = true
	b.turnEvents = turn
	b.turnText.Reset()
	b.turnTools = 0
	b.turnResultCh = make(chan *ResultMessage, 1)
	b.seenToolCalls = make(map[string]bool)
	b.seenTextParts = make(map[string]bool)

	// lastUsage/lastModel are reset under b.mu (they're read by
	// onSessionIdle on the next turn to build TurnResult — a stale
	// value would leak across turns).
	b.mu.Lock()
	b.lastUsage = nil
	b.lastModel = ""
	b.mu.Unlock()
}

// cancelTurn reverses beginTurn. Called from Close and Step 8's
// Interrupt (via the Delegator interface).
func (b *Backend) cancelTurn() {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	b.turnActive = false
	b.turnEvents = nil
}

// IsTurnInFlight reports whether a turn is registered but hasn't
// completed. Inject consults this to route between begin-turn and the
// steerBuf-queue path.
func (b *Backend) IsTurnInFlight() bool {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	return b.turnActive
}

// WaitForTurn blocks until the next turn completion (turnResultCh
// receives) or ctx expires. Returns immediately if no turn is in
// progress (turnResultCh == nil). Mirrors ccstream.
func (b *Backend) WaitForTurn(ctx context.Context) error {
	b.turnMu.Lock()
	ch := b.turnResultCh
	b.turnMu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ArmCompactionWait and WaitForCompaction live in compaction.go (Step 8).

// ---------------------------------------------------------------------------
// Inject routing matrix (plan §6)
// ---------------------------------------------------------------------------

// Inject delivers a user-role event to opencode. Routes based on
// inj.Source and IsTurnInFlight per the plan's matrix:
//
//	Source   | Turn state | Action
//	---------|------------|-------------------------------------------
//	User     | idle       | beginTurn + sendPrompt
//	User     | in-flight  | queue in steerBuf (flushed at session.idle)
//	Steer    | in-flight  | same as User in-flight
//	Steer    | idle + Turn      | degrade to User-idle (begin turn)
//	Steer    | idle + no Turn   | return ErrTurnNotInFlight
//	Compact  | any        | sendCommand("compact", arguments)
//	Pass     | any        | sendCommand(firstToken, rest)
//
// Attachments are honoured on the User-idle path (converted to file
// parts with data: URLs per §6.1).
func (b *Backend) Inject(ctx context.Context, inj delegator.Inject) error {
	if b.server == nil || b.sessionID == "" {
		return errors.New("opencode: Inject before Start (no session)")
	}

	switch inj.Source {
	case delegator.SourceUser:
		return b.injectUser(ctx, inj)

	case delegator.SourceSteer:
		return b.injectSteer(ctx, inj)

	case delegator.SourceCompact, delegator.SourcePass:
		return b.injectCommand(ctx, inj)

	default:
		return fmt.Errorf("opencode: unknown Inject source %v", inj.Source)
	}
}

// injectUser handles SourceUser — the most common path. At idle it
// begins a fresh turn and POSTs /prompt_async; mid-turn it queues in
// steerBuf for flushSteerBuf (Step 7's OnSessionIdle calls it).
func (b *Backend) injectUser(ctx context.Context, inj delegator.Inject) error {
	if b.IsTurnInFlight() {
		// Mid-turn — queue for the follow-up flush. We don't beginTurn
		// here; Step 7's OnSessionIdle calls flushSteerBuf which sends
		// the combined message and begins a fresh turn.
		b.turnMu.Lock()
		b.steerBuf = append(b.steerBuf, inj.Text)
		b.turnMu.Unlock()
		log.Debugf(b.logComponent(), "inject: user text queued mid-turn (%d in buffer)", len(b.steerBuf))
		return nil
	}
	// Idle — begin a fresh turn.
	b.beginTurn(inj.Turn)
	return b.sendPrompt(ctx, inj.Text, inj.Attachments)
}

// injectSteer handles SourceSteer. At idle + Turn present it degrades
// to User-idle (mirroring ccstream's race-fix semantics). At idle +
// no Turn it returns ErrTurnNotInFlight so the caller (Agent.Inbox)
// re-routes through the normal idle path. Mid-turn it queues in
// steerBuf identically to SourceUser (opencode has no priority
// channel — see plan §Steer).
func (b *Backend) injectSteer(ctx context.Context, inj delegator.Inject) error {
	if b.IsTurnInFlight() {
		b.turnMu.Lock()
		b.steerBuf = append(b.steerBuf, inj.Text)
		b.turnMu.Unlock()
		log.Debugf(b.logComponent(), "inject: steer queued mid-turn (%d in buffer)", len(b.steerBuf))
		return nil
	}
	// Idle.
	if inj.Turn == nil {
		// No Turn means the caller (Agent.Inbox) didn't pre-build a
		// TurnEvents for this steer. Beginning a turn here would lose
		// OnTurnComplete / usage accounting. Return ErrTurnNotInFlight
		// so the inbox re-routes through the normal idle path that
		// builds a proper Turn.
		return delegator.ErrTurnNotInFlight
	}
	// Steer-at-idle with Turn present — degrade to User-idle.
	log.Debugf(b.logComponent(), "inject: steer at idle — degrading to user-idle")
	b.beginTurn(inj.Turn)
	return b.sendPrompt(ctx, inj.Text, inj.Attachments)
}

// injectCommand handles SourceCompact and SourcePass — slash commands
// fire-and-forget via POST /session/:id/command. Compact maps to
// command:"compact"; Pass uses the first whitespace-separated token
// of inj.Text as the command name, rest as arguments.
func (b *Backend) injectCommand(ctx context.Context, inj delegator.Inject) error {
	command := ""
	arguments := ""
	if inj.Source == delegator.SourceCompact {
		// inj.Text arrives as "/compact <args>" from foci's command
		// layer. opencode's /command endpoint wants command:"compact"
		// + arguments:"<args>" — strip the leading slash + first token.
		command, arguments = splitSlashCommand(inj.Text)
	} else {
		// SourcePass — passthrough slash command like /context, /model.
		command, arguments = splitSlashCommand(inj.Text)
	}
	return b.sendCommand(ctx, command, arguments)
}

// splitSlashCommand parses "/name rest of line" into (name, rest).
// Used for SourceCompact ("/compact summarise everything" → "compact",
// "summarise everything") and SourcePass. Leading slash is stripped;
// missing name returns ("", "") which the caller surfaces as an error
// from sendCommand.
func splitSlashCommand(text string) (name, rest string) {
	if len(text) > 0 && text[0] == '/' {
		text = text[1:]
	}
	for i, r := range text {
		if r == ' ' || r == '\t' {
			return text[:i], text[i+1:]
		}
	}
	return text, ""
}

// ---------------------------------------------------------------------------
// HTTP primitives
// ---------------------------------------------------------------------------

// sendPrompt POSTs /session/:id/prompt_async with the user text + any
// attachments. The async endpoint returns 204 No Content immediately;
// turn completion arrives as session.idle SSE events (Step 7).
//
// Callers must call beginTurn BEFORE sendPrompt so events arriving in
// response to the POST find an active turn. sendPrompt itself is purely
// the HTTP call — it doesn't touch turn state.
//
// Attachments become file parts with data: URLs (per §6.1). opencode
// treats them as first-class multimodal content — same as if the user
// had pasted an image into the TUI.
func (b *Backend) sendPrompt(ctx context.Context, text string, attachments []delegator.Attachment) error {
	body := buildPromptBody(text, attachments, false)
	return b.postMessage(ctx, "/prompt_async", body)
}

// sendCommand POSTs /session/:id/command with the slash command + args.
// Used for /compact, /context, /model, etc. Response (if any) flows
// through SSE events.
func (b *Backend) sendCommand(ctx context.Context, command, arguments string) error {
	body, err := json.Marshal(map[string]string{
		"command":   command,
		"arguments": arguments,
	})
	if err != nil {
		return err
	}
	return b.postMessage(ctx, "/command", body)
}

// postMessage is the underlying HTTP primitive. URL is the suffix
// after /session/:id (e.g. "/prompt_async", "/command", "/message").
func (b *Backend) postMessage(ctx context.Context, suffix string, body []byte) error {
	url := fmt.Sprintf("%s/session/%s%s", b.server.baseURL, b.sessionID, suffix)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", suffix, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b.checkHTTP401(resp.StatusCode, suffix)
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: HTTP %d: %s", suffix, resp.StatusCode, string(respBody))
	}
	return nil
}

// buildPromptBody constructs the JSON body for /prompt_async (and
// /message, used by injectSystemPrompt). parts[0] is always a text
// block carrying the prompt; attachments append file blocks with data:
// URLs. noReply:true tells opencode to treat the message as context-
// only (used by injectSystemPrompt); false is the normal "expect a
// reply" path.
func buildPromptBody(text string, attachments []delegator.Attachment, noReply bool) []byte {
	type part struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
		Mime string `json:"mime,omitempty"`
		URL  string `json:"url,omitempty"`
	}
	parts := make([]part, 0, 1+len(attachments))
	parts = append(parts, part{Type: "text", Text: text})
	for _, att := range attachments {
		dataURL := "data:" + att.MimeType + ";base64," + base64.StdEncoding.EncodeToString(att.Data)
		parts = append(parts, part{
			Type: "file",
			Mime: att.MimeType,
			URL:  dataURL,
		})
	}
	body := map[string]any{"parts": parts}
	if noReply {
		body["noReply"] = true
	}
	out, _ := json.Marshal(body)
	return out
}

// ---------------------------------------------------------------------------
// Steer buffer flush (called by Step 7's OnSessionIdle)
// ---------------------------------------------------------------------------

// flushSteerBuf drains b.steerBuf and sends the combined text as a
// single follow-up turn. Called from Step 7's OnSessionIdle after a
// turn completes — any user/steer messages that arrived during the
// prior turn are flushed now as a new turn.
//
// Design decision (plan §6.2): combine into one message with \n\n
// separators — they were all meant for the same model context, and
// sending N separate turns would multiply model round-trips.
//
// Returns nil if steerBuf was empty (no-op). Returns the sendPrompt
// error otherwise — Step 7's caller decides whether to surface or
// retry. A nil turnEvents means "begin a fresh turn but with no
// completion callback"; in practice Step 7 always passes a non-nil
// TurnEvents that fires OnTurnComplete.
//
// flushSteerBuf is called from Step 7's dispatcher goroutine, so it
// holds the same serial-execution guarantee as the rest of the
// handler methods — no extra locking needed beyond steerBuf access.
func (b *Backend) flushSteerBuf(ctx context.Context, turnFactory func() *delegator.TurnEvents) error {
	b.turnMu.Lock()
	buf := b.steerBuf
	b.steerBuf = nil
	b.turnMu.Unlock()

	if len(buf) == 0 {
		return nil
	}

	// Combine buffered messages with \n\n separators.
	combined := buf[0]
	for _, msg := range buf[1:] {
		combined += "\n\n" + msg
	}

	turn := turnFactory()
	b.beginTurn(turn)
	log.Infof(b.logComponent(), "flushSteerBuf: sending %d queued message(s) as follow-up turn", len(buf))
	return b.sendPrompt(ctx, combined, nil)
}

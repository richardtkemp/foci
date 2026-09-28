package ccstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"

	"foci/internal/delegator"
)

// Delivery tracking (#2050). Every user-role line foci writes to CC's stdin
// carries a fresh uuid and sits in pendingInputs until CC proves it consumed
// it. The proof is CC's own: the first stdout record whose user_message_uuids
// names the uuid (the message_start of the first API response whose request
// contained it). CC writes the message into its transcript before that request,
// so at that moment a --resume would already have it.
//
// When the process goes away, whatever is still pending is checked against the
// dead process's transcript — the exact source --resume rebuilds from. Inputs
// found there were folded in after all (the ack just never arrived); the rest
// are handed back through DeliveryHooks.OnUndelivered to be written again as
// fresh turns. The ack is only an optimisation that keeps the pending set
// small: correctness rests on the transcript check, so a CC that never emits
// user_message_uuids still loses nothing and duplicates nothing.
//
// Slash commands (SourceCompact, SourcePass) are not tracked: they are
// commands to CC, not conversation input, and CC acknowledges none of them.

// errInputsClosed refuses a write once finalizeExit has begun: the process is
// gone or going, and the snapshot of pending inputs may already have been
// taken, so a write accepted now could be lost between the two.
var errInputsClosed = fmt.Errorf("ccstream: process exited, input not written: %w", delegator.ErrBackendClosed)

// SetDeliveryHooks implements delegator.DeliveryTracker.
func (b *Backend) SetDeliveryHooks(h delegator.DeliveryHooks) {
	b.deliveryHooks.Store(&h)
}

var _ delegator.DeliveryTracker = (*Backend)(nil)

func (b *Backend) hooks() delegator.DeliveryHooks {
	if h := b.deliveryHooks.Load(); h != nil {
		return *h
	}
	return delegator.DeliveryHooks{}
}

// trackedInput describes one write for trackWrite.
type trackedInput struct {
	id           string // reuse (redelivery); "" mints a fresh uuid
	text         string
	attachments  []delegator.Attachment
	refs         []delegator.InputRef
	source       string
	redeliveries int
}

// trackWrite registers in as pending and writes it: registerInput followed by
// writeInput. See those for the refusal and failure contracts.
func (b *Backend) trackWrite(in trackedInput, write func(id string) error) error {
	p, err := b.registerInput(in)
	if err != nil {
		return err
	}
	return b.writeInput(p, write)
}

// registerInput adds in to the pending set and reports it through OnPending
// (which persists it) before anything is written — write-ahead, so a gateway
// killed mid-write still knows about it.
//
// Registration is refused once finalizeExit has closed inputs, so every write
// either lands in the set finalizeExit hands back or is refused here and the
// caller re-routes it.
func (b *Backend) registerInput(in trackedInput) (delegator.PendingInput, error) {
	p := delegator.PendingInput{
		ID:           in.id,
		Text:         in.text,
		Attachments:  in.attachments,
		Refs:         in.refs,
		Source:       in.source,
		WrittenAt:    time.Now(),
		SessionID:    b.SessionID(),
		WorkDir:      b.workDir,
		Redeliveries: in.redeliveries,
	}
	if p.ID == "" {
		p.ID = uuid.NewString()
	}

	b.turnMu.Lock()
	if b.inputsClosed {
		b.turnMu.Unlock()
		return p, errInputsClosed
	}
	b.pendingInputs = append(b.pendingInputs, p)
	b.turnMu.Unlock()

	if h := b.hooks(); h.OnPending != nil {
		h.OnPending(p)
	}
	return p, nil
}

// writeInput performs the write of a registered input. A write that fails
// because the transport is gone stays pending and reports success: the process
// is dead, finalizeExit will hand it back, and returning an error too would
// give the message two owners. Any other failure abandons the entry and
// returns the error, whose handler then owns the message.
func (b *Backend) writeInput(p delegator.PendingInput, write func(id string) error) error {
	err := write(p.ID)
	if err == nil {
		return nil
	}
	// Only a started process is guaranteed a finalizeExit to hand it back.
	if b.cmd != nil && (transportGone(err) || errors.Is(err, errWriterClosed)) {
		b.logger().Infof("input %s (%s) written into a closing process — finalize hands it back: %v", p.ID, p.Source, err)
		return nil
	}
	b.abandonInput(p)
	return err
}

// abandonInput removes a registered input that will not be written after all
// and reports it through OnDropped.
func (b *Backend) abandonInput(p delegator.PendingInput) {
	if b.removePending(p.ID) {
		if h := b.hooks(); h.OnDropped != nil {
			h.OnDropped(p)
		}
	}
}

// removePending deletes id from the pending set, reporting whether it was there.
func (b *Backend) removePending(id string) bool {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	for i, p := range b.pendingInputs {
		if p.ID == id {
			b.pendingInputs = append(b.pendingInputs[:i], b.pendingInputs[i+1:]...)
			return true
		}
	}
	return false
}

// OnInputAck handles CC's consumption ack: a stdout record naming uuids in
// user_message_uuids. Called from the reader goroutine.
func (b *Backend) OnInputAck(ids []string) {
	if len(ids) == 0 {
		return
	}
	var acked []delegator.PendingInput
	b.turnMu.Lock()
	kept := b.pendingInputs[:0]
	for _, p := range b.pendingInputs {
		if containsString(ids, p.ID) {
			acked = append(acked, p)
			continue
		}
		kept = append(kept, p)
	}
	b.pendingInputs = kept
	b.turnMu.Unlock()

	h := b.hooks()
	for _, p := range acked {
		b.logger().Debugf("input %s (%s) consumed by CC", p.ID, p.Source)
		if h.OnConsumed != nil {
			h.OnConsumed(p)
		}
	}
}

// sweepPendingAtIdle runs when CC reports idle with inputs still unacked. CC
// can go idle holding one legitimately (a line written just as the run ended
// starts the next run), so nothing is handed back here: inputs the transcript
// already holds are marked consumed, the rest stay pending until an ack or the
// process's death decides them.
func (b *Backend) sweepPendingAtIdle() {
	b.turnMu.Lock()
	pending := append([]delegator.PendingInput(nil), b.pendingInputs...)
	b.turnMu.Unlock()
	if len(pending) == 0 {
		return
	}
	found, err := b.transcriptHolds(pending)
	if err != nil {
		b.logger().Infof("idle with %d unacked input(s); transcript unreadable (%v) — keeping them pending", len(pending), err)
		return
	}
	var consumed []string
	for _, p := range pending {
		if found[p.ID] {
			consumed = append(consumed, p.ID)
		} else {
			b.logger().Infof("idle with input %s (%s) not yet consumed — still pending (written %s ago)",
				p.ID, p.Source, time.Since(p.WrittenAt).Round(time.Millisecond))
		}
	}
	b.OnInputAck(consumed)
}

// handBackPending takes every still-pending input once the process is gone,
// drops the ones its transcript shows it folded in (reported as consumed), and
// hands the rest to OnUndelivered in write order. Must run after the process
// has been reaped, so nothing can write the transcript any more.
func (b *Backend) handBackPending() {
	b.turnMu.Lock()
	pending := b.pendingInputs
	b.pendingInputs = nil
	b.turnMu.Unlock()
	if len(pending) == 0 {
		return
	}

	h := b.hooks()
	found, err := b.transcriptHolds(pending)
	if err != nil {
		b.logger().Warnf("process gone with %d unconsumed input(s); transcript unreadable (%v) — redelivering all (at-least-once)", len(pending), err)
	}
	var undelivered []delegator.PendingInput
	for _, p := range pending {
		if found[p.ID] {
			b.logger().Infof("input %s (%s) was folded in before the process exited — not redelivering", p.ID, p.Source)
			if h.OnConsumed != nil {
				h.OnConsumed(p)
			}
			continue
		}
		if p.Redeliveries >= delegator.MaxRedeliveries {
			b.logger().Errorf("input %s (%s) never consumed after %d redeliveries — dropping it: %.80q", p.ID, p.Source, p.Redeliveries, p.Text)
			if h.OnDropped != nil {
				h.OnDropped(p)
			}
			continue
		}
		p.Redeliveries++
		undelivered = append(undelivered, p)
	}
	if len(undelivered) == 0 {
		return
	}
	b.logger().Warnf("process gone with %d input(s) CC never consumed — handing them back for redelivery", len(undelivered))
	if h.OnUndelivered != nil {
		h.OnUndelivered(undelivered)
	} else {
		b.logger().Warnf("no redelivery hook installed — %d input(s) lost", len(undelivered))
	}
}

// transcriptHolds reports which pending inputs the transcript(s) they were
// written to already contain. An input's own session and the backend's current
// one are both read, since a session id can change during a process's life.
func (b *Backend) transcriptHolds(pending []delegator.PendingInput) (map[string]bool, error) {
	ids := make([]string, 0, len(pending))
	sessions := map[string]bool{}
	for _, p := range pending {
		ids = append(ids, p.ID)
		if p.SessionID != "" {
			sessions[p.SessionID] = true
		}
	}
	if sid := b.SessionID(); sid != "" {
		sessions[sid] = true
	}
	if len(sessions) == 0 || b.workDir == "" {
		return nil, errors.New("no session id or workdir")
	}
	found := map[string]bool{}
	read := 0
	var lastErr error
	for sid := range sessions {
		path, err := ccTranscriptPath(b.workDir, sid)
		if err != nil {
			lastErr = err
			continue
		}
		got, err := transcriptInputs(path, ids)
		if err != nil {
			lastErr = err
			continue
		}
		read++
		for id := range got {
			found[id] = true
		}
	}
	if read == 0 {
		return nil, lastErr
	}
	return found, nil
}

// InputInTranscript implements delegator.TranscriptChecker for claude-code: it
// reports whether the transcript of sessionID in workDir holds the input id.
func InputInTranscript(workDir, sessionID, id string) (bool, error) {
	path, err := ccTranscriptPath(workDir, sessionID)
	if err != nil {
		return false, err
	}
	got, err := transcriptInputs(path, []string{id})
	if err != nil {
		return false, err
	}
	return got[id], nil
}

// transcriptRecord is the part of a CC transcript line that names an input.
type transcriptRecord struct {
	Type       string `json:"type"`
	UUID       string `json:"uuid"`
	Attachment *struct {
		Type       string `json:"type"`
		SourceUUID string `json:"source_uuid"`
	} `json:"attachment"`
}

// transcriptInputs returns which of ids the transcript at path holds. CC
// records a consumed stdin message in one of two shapes (verified on 2.1.280):
//
//   - {"type":"user","uuid":U,...}: a message that started an ask of its own
//     (a turn-starting message, or a queued one run after a text-only answer);
//   - {"type":"attachment","attachment":{"type":"queued_command","source_uuid":U}}:
//     a message folded mid-turn at a tool boundary.
//
// The "queue-operation" enqueue records carry no uuid and prove nothing — CC
// writes them on receipt, before any fold.
func transcriptInputs(path string, ids []string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	needles := make([][]byte, len(ids))
	for i, id := range ids {
		needles[i] = []byte(id)
	}
	found := map[string]bool{}
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			matchTranscriptLine(line, ids, needles, found)
		}
		if err == io.EOF {
			return found, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func matchTranscriptLine(line []byte, ids []string, needles [][]byte, found map[string]bool) {
	hit := false
	for _, n := range needles {
		if bytes.Contains(line, n) {
			hit = true
			break
		}
	}
	if !hit {
		return
	}
	var rec transcriptRecord
	if json.Unmarshal(line, &rec) != nil {
		return
	}
	var id string
	switch {
	case rec.Type == "user":
		id = rec.UUID
	case rec.Type == "attachment" && rec.Attachment != nil && rec.Attachment.Type == "queued_command":
		id = rec.Attachment.SourceUUID
	default:
		return
	}
	if containsString(ids, id) {
		found[id] = true
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

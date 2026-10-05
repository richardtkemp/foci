package ccstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"foci/internal/delegator"
)

// ArmCompactionWait sets up the one-shot outcome channel signalled when
// compaction settles: resolveCompactionWait(nil) on compact_boundary
// (success), resolveCompactionWait(ErrCompactionNoBoundary) when the /compact
// run goes idle without a boundary (backend declined — #1267). Must be called
// before the /compact command is sent so neither outcome is missed.
func (b *Backend) ArmCompactionWait() {
	path := b.SessionFilePath()
	var off int64
	if path != "" {
		if fi, err := os.Stat(path); err == nil {
			off = fi.Size()
		}
	}
	b.turnMu.Lock()
	b.compactCh = make(chan error, 1)
	b.compactTranscript = path
	b.compactTranscriptOff = off
	b.turnMu.Unlock()
}

// Compaction summary recovery (#1390). CC does not put the summary on the
// stream: it writes it to the transcript as a user record flagged
// isCompactSummary (right after the compact_boundary record), wrapped in a
// continuation preamble and trailer addressed to the model.
const (
	compactSummaryMarker   = `"isCompactSummary":true`
	compactSummaryPoll     = 100 * time.Millisecond
	compactSummaryMaxWait  = 3 * time.Second
	compactSummaryPreamble = "This session is being continued from a previous conversation"
	compactSummaryHeader   = "Summary:\n"
)

// compactSummaryTrailers open the model-facing text CC appends after the
// summary; the earliest one found ends the summary.
var compactSummaryTrailers = []string{
	"\n\nIf you need specific details from before compaction",
	"\nContinue the conversation from where it left off",
}

// CompactionSummary implements delegator.CompactionSummarizer: the summary CC
// wrote to the transcript since ArmCompactionWait. The compact_boundary stream
// event can race the transcript write, so an absent record is polled for
// briefly before giving up with "".
func (b *Backend) CompactionSummary(ctx context.Context) (string, error) {
	b.turnMu.Lock()
	path, off := b.compactTranscript, b.compactTranscriptOff
	b.turnMu.Unlock()
	if path == "" {
		return "", errors.New("ccstream: no transcript path recorded for this compaction")
	}
	deadline := time.Now().Add(compactSummaryMaxWait)
	for {
		s, err := readCompactSummary(path, off)
		if s != "" || err != nil || time.Now().After(deadline) {
			return s, err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(compactSummaryPoll):
		}
	}
}

// readCompactSummary returns the last isCompactSummary record's text at or
// after byte offset off of the transcript at path, "" if there is none.
func readCompactSummary(path string, off int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", err
	}
	marker := []byte(compactSummaryMarker)
	var last string
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, rerr := r.ReadBytes('\n')
		// Cheap filter first; the decode rejects text that merely quotes the
		// marker and a torn trailing record.
		if bytes.Contains(line, marker) {
			var rec struct {
				IsCompactSummary bool `json:"isCompactSummary"`
				Message          struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &rec) == nil && rec.IsCompactSummary {
				if s := trimCompactSummary(contentText(rec.Message.Content)); s != "" {
					last = s
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return last, nil
			}
			return last, fmt.Errorf("read %s: %w", path, rerr)
		}
	}
}

// contentText flattens a message content field: a plain string, or an array
// of blocks whose text blocks are joined.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, bl := range blocks {
		if bl.Type == "text" && bl.Text != "" {
			parts = append(parts, bl.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// trimCompactSummary strips CC's model-facing preamble and trailer, leaving
// the summary itself. Text in an unrecognised shape is returned whole.
func trimCompactSummary(s string) string {
	if strings.HasPrefix(s, compactSummaryPreamble) {
		if i := strings.Index(s, compactSummaryHeader); i >= 0 {
			s = s[i+len(compactSummaryHeader):]
		}
	}
	end := len(s)
	for _, t := range compactSummaryTrailers {
		if i := strings.Index(s, t); i >= 0 && i < end {
			end = i
		}
	}
	return strings.TrimSpace(s[:end])
}

// resolveCompactionWait records the resolved outcome for the current
// compaction wait (if one is armed) and wakes any blocked WaitForCompaction
// caller. Caller must hold turnMu.
//
// Deliberately does NOT clear b.compactCh — only a WaitForCompaction call
// that has actually received a value resets it (see there). That is what
// keeps this race-free (#1526): the old design signalled resolution by nil-ing
// shared fields, so a WaitForCompaction call that hadn't yet captured its
// local copies of those fields could observe them already nil'd by whichever
// path resolved first and misread "already resolved via abort" as "never
// armed", silently returning success for a declined compaction. Here the
// channel identity never changes underneath a late reader — it always
// captures the same non-nil channel and receives whatever value was (or will
// be) sent into it, so the outcome is never lost regardless of scheduling.
//
// The buffered(1) capacity also preserves the original exclusivity property
// (a legitimate boundary success and a later spurious abort must never both
// be observable, or a select would pick between them pseudo-randomly): once
// one outcome lands in the single buffer slot, a second resolveCompactionWait
// call for the same arm hits the full-buffer default case and no-ops.
func (b *Backend) resolveCompactionWait(err error) {
	if b.compactCh == nil {
		return
	}
	select {
	case b.compactCh <- err:
	default:
	}
}

// signalCompactionAbort resolves the compaction wait with
// ErrCompactionNoBoundary if one is still armed (compact_boundary never
// arrived). Called from the idle handler: since compact_boundary always
// precedes idle on a real compaction (it resolves the wait first), a still-
// armed waiter at idle means the backend declined to compact. No-op if no
// wait is armed or a boundary already resolved it, and while the /compact is
// still deferred behind another turn (that turn's idle says nothing about the
// compaction, #2147). Caller must hold turnMu.
func (b *Backend) signalCompactionAbort() {
	if b.compactDeferred {
		return
	}
	b.resolveCompactionWait(delegator.ErrCompactionNoBoundary)
}

// WaitForCompaction blocks until compact_boundary is received (nil), the
// /compact run goes idle without a boundary (ErrCompactionNoBoundary), or ctx
// expires. Returns immediately if no waiter is armed (ArmCompactionWait was not
// called, or a previous call already consumed this arm's outcome).
func (b *Backend) WaitForCompaction(ctx context.Context) error {
	b.turnMu.Lock()
	ch := b.compactCh
	b.turnMu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case err := <-ch:
		b.turnMu.Lock()
		if b.compactCh == ch {
			b.compactCh = nil
		}
		b.turnMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ArmCompactionStartWait sets up a one-shot channel that will be closed when
// status="compacting" is received. Must be called before the /compact command
// is sent so the signal is never missed.
func (b *Backend) ArmCompactionStartWait() {
	b.turnMu.Lock()
	b.compactStartCh = make(chan struct{}, 1)
	b.turnMu.Unlock()
}

// WaitForCompactionStart blocks until status="compacting" is received or ctx
// expires. Returns immediately if no waiter is armed.
func (b *Backend) WaitForCompactionStart(ctx context.Context) error {
	b.turnMu.Lock()
	ch := b.compactStartCh
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

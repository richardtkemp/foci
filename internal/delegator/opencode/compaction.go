// compaction.go — CompactionWaiter + CompactionStartWaiter implementation.
// All four compaction-wait methods live here.
//
// Implements delegator.CompactionWaiter (ArmCompactionWait /
// WaitForCompaction) + delegator.CompactionStartWaiter
// (ArmCompactionStartWait / WaitForCompactionStart).

package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ArmCompactionWait resets compactDoneCh for the next /compact cycle.
// Closed by onSessionCompacted (handlers.go).
func (b *Backend) ArmCompactionWait() {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	b.compactDoneCh = make(chan struct{}, 1)
	b.compactArmedAt = time.Now()
}

// WaitForCompaction blocks on compactDoneCh or ctx. Returns immediately
// (nil) if not armed — matches ccstream's no-arm semantics.
func (b *Backend) WaitForCompaction(ctx context.Context) error {
	b.turnMu.Lock()
	ch := b.compactDoneCh
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

// ArmCompactionStartWait arms the one-shot "compaction started" waiter.
// The channel is closed by handleCompactionPart (handlers.go) when the
// compaction part arrives — opencode's real start signal, emitted at
// summarize initiation, before the summary LLM streams (~2.5s ahead of
// the first reasoning token, measured on 1.17.11). Must be called before
// Inject(SourceCompact) so the signal is never missed; see
// agent.runDelegatedCompact.
func (b *Backend) ArmCompactionStartWait() {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	b.compactStartCh = make(chan struct{}, 1)
}

// WaitForCompactionStart blocks until the compaction part arrives
// (compactStartCh closed by handleCompactionPart) or ctx expires. Returns
// nil immediately if not armed (matching WaitForCompaction's no-arm
// contract). The ctx branch is reachable: if /summarize is accepted but
// opencode never emits a compaction part, this unblocks on the context
// deadline so the ⏳ notification fires anyway (non-fatal — the wait for
// completion below proceeds regardless).
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

// compactSummaryScan is how many of the session's newest messages
// CompactionSummary searches for the summary message; the summary is the
// newest assistant message once /summarize returns, so a few suffice.
const compactSummaryScan = 10

// compactArmSlack absorbs clock granularity between foci's arm time and
// opencode's message timestamp (same host, unix ms).
const compactArmSlack = time.Second

// messageWithParts is one element of GET /session/{id}/message: {info,
// parts}, with `limit` selecting the NEWEST n in ascending order (verified
// live on opencode 1.17.15). Only the info fields read here are decoded.
type messageWithParts struct {
	Info struct {
		Role    string      `json:"role"`
		Mode    string      `json:"mode"`
		Summary SummaryFlag `json:"summary"`
		Time    MessageTime `json:"time"`
	} `json:"info"`
	Parts []Part `json:"parts"`
}

// isCompactionSummary reports whether m is the assistant message a
// /summarize compaction produced (summary:true, mode "compaction").
func (m *messageWithParts) isCompactionSummary() bool {
	return m.Info.Role == "assistant" && (bool(m.Info.Summary) || m.Info.Mode == "compaction")
}

// CompactionSummary implements delegator.CompactionSummarizer: the text of
// the summary message /summarize created (assistant, summary:true or mode
// "compaction"), fetched from the session's message list (#1390).
func (b *Backend) CompactionSummary(ctx context.Context) (string, error) {
	if b.server == nil || b.sessionID == "" {
		return "", errors.New("opencode: no session to read a compaction summary from")
	}
	b.turnMu.Lock()
	armed := b.compactArmedAt
	b.turnMu.Unlock()

	url := fmt.Sprintf("%s/session/%s/message?limit=%d", b.server.baseURL, b.sessionID, compactSummaryScan)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("GET /message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read /message: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /message: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var msgs []messageWithParts
	if err := json.Unmarshal(body, &msgs); err != nil {
		return "", fmt.Errorf("decode /message: %w", err)
	}
	return pickCompactionSummary(msgs, armed), nil
}

// pickCompactionSummary returns the joined text of the newest summary message
// created at or after armed (less compactArmSlack), "" if there is none. A
// zero armed accepts any summary message.
func pickCompactionSummary(msgs []messageWithParts, armed time.Time) string {
	var best *messageWithParts
	for i := range msgs {
		m := &msgs[i]
		if !m.isCompactionSummary() {
			continue
		}
		if !armed.IsZero() && time.UnixMilli(m.Info.Time.Created).Before(armed.Add(-compactArmSlack)) {
			continue
		}
		if best == nil || m.Info.Time.Created >= best.Info.Time.Created {
			best = m
		}
	}
	if best == nil {
		return ""
	}
	var parts []string
	for _, p := range best.Parts {
		if p.Type == "text" && !p.Ignored && p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

package ccstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"foci/internal/procx"
	"foci/internal/tempdir"
)

// usageOneshotTimeout bounds the whole one-shot query, including process
// spawn. CC's get_usage is a real network round trip to its own
// account-usage backend and has been observed taking 15-20s.
const usageOneshotTimeout = 45 * time.Second

// UsageInfo is the parsed result of QueryUsage — the data behind CC's /usage
// command (session/weekly plan limits, cost, and the "what's contributing"
// behavior breakdown), in structured form.
type UsageInfo struct {
	SubscriptionType string
	FiveHour         UsageWindow // CC's "session" limit
	SevenDay         UsageWindow // CC's "week (all models)" limit
	SessionCostUSD   float64
	Day              UsageBehaviorWindow
	Week             UsageBehaviorWindow
	Raw              json.RawMessage // full get_usage response payload, for anything not modeled above
}

// UsageWindow is one rate-limit window's utilization. Percent is 0-100
// (CC's own scale for get_usage). ResetsAt is the zero Time if CC omitted
// or sent an unparseable resets_at.
type UsageWindow struct {
	Percent  int
	ResetsAt time.Time
}

// UsageBehaviorWindow is one window ("last 24h"/"last 7d") of what's
// contributing to plan-limit usage.
type UsageBehaviorWindow struct {
	RequestCount int
	SessionCount int
	Top          []UsageBehaviorItem // CC's own ordering
}

// UsageBehaviorItem is one contributing factor, e.g. {Key: "long_context", Pct: 88}.
type UsageBehaviorItem struct {
	Key   string
	Pct   int
	Count int
}

// QueryUsage runs an independent, throwaway `claude` subprocess and asks it
// for the account's plan/rate-limit usage via the get_usage control request
// — the data behind /usage, as structured JSON instead of rendered text.
//
// Deliberately does NOT touch any live per-session backend (unlike
// GetContextWindow, which queries a session's already-running CC process): a
// usage check must never interfere with, wait behind, or depend on an active
// session's turn state, and must work even when no foci session is currently
// running at all. It spawns its own scratch CC process, asks its one
// question, and kills it.
//
// Zero API/model cost: CC serves get_usage from its own account-usage
// backend, not the model — verified live (a bare initialize + get_usage
// round trip reports total_cost_usd: 0, model_usage: {}, no assistant turn
// ever runs).
func QueryUsage(ctx context.Context) (*UsageInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, usageOneshotTimeout)
	defer cancel()

	// A stable-but-unique scratch cwd: CC always creates a per-cwd session
	// transcript even in one-shot -p mode, so a fresh dir per call is
	// deliberate (never shares/pollutes a real project's or session's
	// history) and is removed immediately after. SpawnMkdir (not Mkdir):
	// this is exactly the "spawn isolation sandbox" case its doc comment
	// describes — a throwaway subprocess's scratch cwd.
	scratch, err := tempdir.SpawnMkdir("cc-usage-oneshot-*")
	if err != nil {
		return nil, fmt.Errorf("ccstream: usage oneshot: scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	cmd := procx.Spawn(ctx, "claude",
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--permission-prompt-tool", "stdio",
		"--verbose",
	)
	cmd.Dir = scratch

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ccstream: usage oneshot: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ccstream: usage oneshot: stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ccstream: usage oneshot: start claude: %w", err)
	}
	// Always tear the scratch process down — this is a fire-once query, never
	// a session left running.
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Route control_response lines to the caller by request_id. No live
	// Backend exists here, so this reads inline rather than dispatching
	// through Backend.OnControlResponse's pendingControls map.
	resCh := make(chan json.RawMessage, 4)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var env struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(line, &env) != nil || env.Type != "control_response" {
				continue
			}
			cp := make([]byte, len(line))
			copy(cp, line)
			select {
			case resCh <- cp:
			case <-ctx.Done():
				return
			}
		}
	}()

	w := NewWriter(stdin)

	// Handshake: CC won't answer most control requests before its own
	// initialize round trip completes (verified — see
	// verify-cc-stream-hooks skill's control_test_harness.py precedent).
	// Wait for its control_response before sending get_usage, rather than a
	// fixed sleep.
	if _, err := oneshotRoundTrip(ctx, w, resCh, readerDone, "initialize", &InitializeRequest{Subtype: "initialize"}); err != nil {
		return nil, err
	}

	raw, err := oneshotRoundTrip(ctx, w, resCh, readerDone, "get_usage", &GetUsageRequest{Subtype: "get_usage"})
	if err != nil {
		return nil, err
	}
	return parseUsagePayload(raw)
}

// oneshotRoundTrip sends one control request and waits for its response.
//
// A write failure that only means "the transport is already gone"
// (transportGone) is deliberately NOT returned. The one-shot subprocess can
// die at any point before the handshake completes — a `claude` that exits on
// bad flags, a failed auth check, a missing binary's shell wrapper — and which
// side notices first is a pure scheduling race:
//
//   - the write wins: the line lands in the pipe buffer and succeeds, then the
//     reader hits EOF on stdout and waitForControlResponseRaw reports
//     "claude exited before responding";
//   - the death wins: the kernel has already torn down the pipe's read end and
//     the write fails with EPIPE ("broken pipe").
//
// Measured, not assumed: with the child exiting immediately, a write issued in
// the same scheduling instant succeeds, and one issued 5ms later fails with
// EPIPE. Both orderings mean exactly one thing, so both must report it the same
// way — otherwise /mana's error text depends on a coin flip. Falling through to
// the wait produces the single canonical message from the reader's EOF.
//
// Note the sentinel differs from Start's version of this race (e1fe3779), which
// saw os.ErrClosed: there a concurrent waiter goroutine's cmd.Wait closed the
// PARENT's end of the pipe. Here cmd.Wait is deferred to teardown, so nothing
// closes our end and only the kernel's EPIPE is reachable. transportGone
// matches both, so it applies unchanged.
//
// A transport-gone write followed by a child that somehow stays alive with its
// stdout open would block here rather than erroring immediately; the whole call
// is bounded by usageOneshotTimeout, and CC does not close its stdin while
// running in stream-json input mode.
func oneshotRoundTrip(ctx context.Context, w *Writer, resCh <-chan json.RawMessage, readerDone <-chan struct{}, what string, req any) (json.RawMessage, error) {
	reqID := newRequestID()
	if err := w.SendControl(reqID, req); err != nil && !transportGone(err) {
		return nil, fmt.Errorf("ccstream: usage oneshot: send %s: %w", what, err)
	}
	raw, err := waitForControlResponseRaw(ctx, resCh, readerDone, reqID)
	if err != nil {
		return nil, fmt.Errorf("ccstream: usage oneshot: %s: %w", what, err)
	}
	return raw, nil
}

// waitForControlResponseRaw blocks until a control_response for reqID
// arrives, the reader goroutine exits (CC died), or ctx expires. Returns the
// success payload's raw `response.response` bytes.
func waitForControlResponseRaw(ctx context.Context, resCh <-chan json.RawMessage, readerDone <-chan struct{}, reqID string) (json.RawMessage, error) {
	for {
		select {
		case line := <-resCh:
			var inb controlResponseInbound
			if err := json.Unmarshal(line, &inb); err != nil {
				continue // malformed line — keep waiting
			}
			if inb.Response.RequestID != reqID {
				continue // some other in-flight request's response
			}
			if inb.Response.Subtype != "success" {
				if inb.Response.Error != "" {
					return nil, fmt.Errorf("%s", inb.Response.Error)
				}
				return nil, fmt.Errorf("returned subtype %q", inb.Response.Subtype)
			}
			return inb.Response.Response, nil
		case <-readerDone:
			return nil, fmt.Errorf("claude exited before responding")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// parseUsagePayload maps the wire usagePayload into the public UsageInfo.
func parseUsagePayload(raw json.RawMessage) (*UsageInfo, error) {
	var p usagePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("ccstream: usage oneshot: unmarshal get_usage payload: %w", err)
	}
	return &UsageInfo{
		SubscriptionType: p.SubscriptionType,
		FiveHour:         parseUsageWindow(p.RateLimits.FiveHour),
		SevenDay:         parseUsageWindow(p.RateLimits.SevenDay),
		SessionCostUSD:   p.Session.TotalCostUSD,
		Day:              parseUsageBehaviorWindow(p.Behaviors.Day),
		Week:             parseUsageBehaviorWindow(p.Behaviors.Week),
		Raw:              raw,
	}, nil
}

func parseUsageWindow(w usageWindowRaw) UsageWindow {
	out := UsageWindow{Percent: w.Utilization}
	if w.ResetsAt != "" {
		if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
			out.ResetsAt = t
		}
	}
	return out
}

func parseUsageBehaviorWindow(w usageBehaviorWindow) UsageBehaviorWindow {
	items := make([]UsageBehaviorItem, len(w.Behaviors))
	for i, b := range w.Behaviors {
		items[i] = UsageBehaviorItem{Key: b.Key, Pct: b.Pct, Count: b.Count}
	}
	return UsageBehaviorWindow{
		RequestCount: w.RequestCount,
		SessionCount: w.SessionCount,
		Top:          items,
	}
}

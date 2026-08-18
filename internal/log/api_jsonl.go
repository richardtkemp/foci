package log

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"foci/internal/modelinfo"
)

// APIEntry is a structured record for one API request.
type APIEntry struct {
	Timestamp  time.Time `json:"ts"`
	Provider   string    `json:"provider,omitempty"` // "anthropic" or "gemini" (empty = anthropic for backwards compat)
	Session    string    `json:"session"`
	Model      string    `json:"model"`
	Input      int       `json:"input"`
	Output     int       `json:"output"`
	CacheRead  int       `json:"cache_read"`
	CacheWrite int       `json:"cache_write"`
	// ProvidedCostUSD is the cost the backend reported for this call, when one
	// exists — CC's ModelUsage.CostUSD / opencode's Message.Cost, captured
	// verbatim and never a foci-side calculation. nil when the backend gave no
	// cost (e.g. foci's own direct Anthropic API calls, which report none).
	//
	// NOT AUTHORITATIVE, and not what any total should be built from (#1674).
	// CC's figure is cumulative over the CC process, so historical rows here
	// carry running totals rather than per-turn costs. It is retained for
	// forensics and as the reference for the cost-divergence warning.
	ProvidedCostUSD *float64 `json:"provided_cost_usd,omitempty"`

	// CalculatedCostUSD is foci's own priced figure for this call — the
	// authoritative cost (#1674). nil for rows written before the change, and
	// for backends that supply no per-call tokens to price; EffectiveCost falls
	// back to a live calculation in that case.
	CalculatedCostUSD *float64 `json:"calculated_cost_usd,omitempty"`
	DurationMS        int64    `json:"duration_ms"`
	StopReason        string   `json:"stop_reason"`
	CallType          string   `json:"call_type"`              // "conversation", "compaction", "summary", "spawn"
	SessionFile       string   `json:"session_file,omitempty"` // path to session JSONL file
	SessionLine       int      `json:"session_line,omitempty"` // line number in session file (conversation calls)
	PreMessages       int      `json:"pre_messages,omitempty"` // message count before compaction
}

// EffectiveCost returns this entry's cost for display: foci's own calculated
// figure when we have one, otherwise a LIVE estimate computed from the stored
// tokens using the price effective AT THE REQUEST'S TIMESTAMP
// (modelinfo.CostAsOf) — not today's latest price. Never cache or persist the
// result; call this at read time (foci_todo #1407).
//
// ProvidedCostUSD is deliberately NOT consulted (#1674). It used to win here,
// which is how CC's cumulative-per-process figure became every row's "cost"
// and inflated totals ~13x. Our own number is preferred precisely because
// token counts have unambiguous semantics where a provider's cost total does
// not; the provider's figure now only backs the divergence warning.
func (e APIEntry) EffectiveCost() float64 {
	if e.CalculatedCostUSD != nil {
		return *e.CalculatedCostUSD
	}
	return modelinfo.CostAsOf(e.Model, e.Timestamp, e.Input, e.Output, e.CacheRead, e.CacheWrite)
}

// PayloadEntry is a full API request/response record.
type PayloadEntry struct {
	Timestamp    time.Time       `json:"ts"`
	Session      string          `json:"session"`
	SeqNum       int             `json:"seq"`
	Model        string          `json:"model"`
	SystemHash   string          `json:"system_hash"`
	Request      json.RawMessage `json:"request"`
	Response     json.RawMessage `json:"response,omitempty"`
	Error        string          `json:"error,omitempty"`
	StatusCode   int             `json:"status_code,omitempty"`
	ResponseBody json.RawMessage `json:"response_body,omitempty"`
	RequestID    string          `json:"request_id,omitempty"`
	DurationMS   int64           `json:"duration_ms"`
}

// api writes a structured API log entry to JSONL and SQLite.
func (l *Logger) api(entry APIEntry) {
	if entry.CallType == "" {
		entry.CallType = "conversation"
	}

	// JSONL (backward compatible)
	l.mu.Lock()
	staleWarn := l.reopenAPIIfStaleLocked()
	if l.apiFile != nil {
		if data, err := json.Marshal(entry); err == nil {
			_, _ = l.apiFile.Write(append(data, '\n'))
		}
	}
	l.mu.Unlock()

	// Logged after releasing l.mu above — Warnf ultimately locks l.mu itself.
	if staleWarn != "" {
		Warnf("log", "%s", staleWarn)
	}

	// SQLite
	if apiLog != nil {
		apiLog.insert(entry)
	}
}

// payload writes a full API request/response record.
func (l *Logger) payload(entry PayloadEntry) {
	l.mu.Lock()
	staleWarn := l.reopenPayloadIfStaleLocked()
	if l.payloadFile != nil {
		if data, err := json.Marshal(entry); err == nil {
			_, _ = l.payloadFile.Write(append(data, '\n'))
		}
	}
	l.mu.Unlock()

	// Logged after releasing l.mu above — Warnf ultimately locks l.mu itself.
	if staleWarn != "" {
		Warnf("log", "%s", staleWarn)
	}
}

// PayloadEnabled returns true if full payload logging is active.
func PayloadEnabled() bool {
	std.mu.Lock()
	defer std.mu.Unlock()
	return std.payloadFile != nil
}

// API logs a structured API call entry (package-level).
func API(entry APIEntry) {
	// Auto-infer provider from model name when not explicitly set.
	if entry.Provider == "" {
		if strings.HasPrefix(entry.Model, "gemini-") {
			entry.Provider = "gemini"
		} else if modelinfo.IsOpenAI(entry.Model) {
			entry.Provider = "openai"
		} else if strings.HasPrefix(entry.Model, "claude-") {
			entry.Provider = "anthropic"
		}
	}
	std.api(entry)
}

// Payload logs a full API request/response record (package-level).
func Payload(entry PayloadEntry) {
	std.payload(entry)
}

// SystemHash computes a truncated SHA-256 hash (16 hex chars) of concatenated
// system block texts. Returns an empty string for nil/empty blocks.
func SystemHash(texts []string) string {
	if len(texts) == 0 {
		return ""
	}
	h := sha256.New()
	for _, t := range texts {
		h.Write([]byte(t))
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:8])
}

// ReadAPILog reads a JSONL API log file and returns all entries.
func ReadAPILog(path string) []APIEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var entries []APIEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var e APIEntry
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			entries = append(entries, e)
		}
	}
	return entries
}

// SetAPIWriter replaces the API log file (for testing).
// Exported for cross-package test use (agent/integration_test.go).
func SetAPIWriter(f *os.File) {
	std.mu.Lock()
	std.apiFile = f
	std.mu.Unlock()
}

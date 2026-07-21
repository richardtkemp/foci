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
	// GoldenCostUSD is the provider-reported cost for this call, when one
	// exists — CC's ModelUsage.CostUSD / opencode's Message.Cost, captured
	// verbatim and never a foci-side calculation. nil when the backend gave
	// no cost (e.g. foci's own direct Anthropic API calls, which report no
	// cost at all). Never populate this from modelinfo.Cost — a calculated
	// figure must NOT be persisted here (foci_todo #1407). Readers wanting a
	// display cost should call EffectiveCost, which computes live from stored
	// tokens (as-of the request time) when this is nil.
	GoldenCostUSD *float64 `json:"golden_cost_usd,omitempty"`
	DurationMS    int64    `json:"duration_ms"`
	StopReason    string   `json:"stop_reason"`
	CallType      string   `json:"call_type"`              // "conversation", "compaction", "summary", "spawn"
	SessionFile   string   `json:"session_file,omitempty"` // path to session JSONL file
	SessionLine   int      `json:"session_line,omitempty"` // line number in session file (conversation calls)
	PreMessages   int      `json:"pre_messages,omitempty"` // message count before compaction
}

// EffectiveCost returns this entry's cost for display: the golden
// (provider-reported) value verbatim if we have one, otherwise a LIVE
// estimate computed from the stored tokens using the price effective AT THE
// REQUEST'S TIMESTAMP (modelinfo.CostAsOf) — not today's latest price. Never
// cache or persist the result; call this at read time (foci_todo #1407).
func (e APIEntry) EffectiveCost() float64 {
	if e.GoldenCostUSD != nil {
		return *e.GoldenCostUSD
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
	if l.apiFile != nil {
		if data, err := json.Marshal(entry); err == nil {
			_, _ = l.apiFile.Write(append(data, '\n'))
		}
	}
	l.mu.Unlock()

	// SQLite
	if apiLog != nil {
		apiLog.insert(entry)
	}
}

// payload writes a full API request/response record.
func (l *Logger) payload(entry PayloadEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.payloadFile == nil {
		return
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = l.payloadFile.Write(append(data, '\n'))
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

package log

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

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

// AppendAPILine appends v, as one JSON line, to api.jsonl. The cost ledger
// (internal/delegator/accounting) writes one line per booked call; this
// package owns only the file (rotation, stale-inode reopen).
func AppendAPILine(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	std.mu.Lock()
	staleWarn := std.reopenAPIIfStaleLocked()
	if std.apiFile != nil {
		_, _ = std.apiFile.Write(append(data, '\n'))
	}
	std.mu.Unlock()

	// Logged after releasing std.mu above — Warnf ultimately locks it itself.
	if staleWarn != "" {
		Warnf("log", "%s", staleWarn)
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

// SetAPIWriter replaces the API log file (for testing).
// Exported for cross-package test use (agent/integration_test.go).
func SetAPIWriter(f *os.File) {
	std.mu.Lock()
	std.apiFile = f
	std.mu.Unlock()
}

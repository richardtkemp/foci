package log

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Level represents a log severity level.
type Level int

const (
	DEBUG Level = iota
	INFO
	WARN
	ERROR
)

func (l Level) String() string {
	switch l {
	case DEBUG:
		return "DEBUG"
	case INFO:
		return "INFO"
	case WARN:
		return "WARN"
	case ERROR:
		return "ERROR"
	default:
		return "???"
	}
}

// ParseLevel parses a level string. Returns INFO for unrecognized values.
func ParseLevel(s string) Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return DEBUG
	case "INFO":
		return INFO
	case "WARN":
		return WARN
	case "ERROR":
		return ERROR
	default:
		return INFO
	}
}

// APIEntry is a structured record for one Anthropic API request.
type APIEntry struct {
	Timestamp   time.Time `json:"ts"`
	Session     string    `json:"session"`
	Model       string    `json:"model"`
	Input       int       `json:"input"`
	Output      int       `json:"output"`
	CacheRead   int       `json:"cache_read"`
	CacheWrite  int       `json:"cache_write"`
	CostUSD     float64   `json:"cost_usd"`
	DurationMS  int64     `json:"duration_ms"`
	StopReason  string    `json:"stop_reason"`
	CallType    string    `json:"call_type"`              // "conversation", "compaction", "summary", "spawn"
	SessionFile string    `json:"session_file,omitempty"` // path to session JSONL file
	SessionLine int       `json:"session_line,omitempty"` // line number in session file (conversation calls)

}

// PayloadEntry is a full API request/response record.
type PayloadEntry struct {
	Timestamp  time.Time       `json:"ts"`
	Session    string          `json:"session"`
	Model      string          `json:"model"`
	Request    json.RawMessage `json:"request"`
	Response   json.RawMessage `json:"response"`
	DurationMS int64           `json:"duration_ms"`
}

// Logger writes event log lines and structured API log entries.
type Logger struct {
	level       Level
	eventOut    io.Writer // foci.log + stderr multiwriter
	eventFile   *os.File  // foci.log file handle (nil = stderr only)
	apiFile     *os.File  // api.jsonl (nil if disabled)
	payloadFile *os.File  // api-payload.jsonl (nil if disabled)
	eventPath   string    // path to foci.log
	apiPath     string    // path to api.jsonl
	payloadPath string    // path to api-payload.jsonl
	buffer      []string  // pre-Init event lines, replayed to event file on Init
	initialized bool      // true after Init completes
	mu          sync.Mutex
}

// apiDB is the SQLite API call log (separate from the main Logger to
// match the conversation.go pattern — independent init/close lifecycle).
type apiDB struct {
	db   *sql.DB
	stmt *sql.Stmt
	mu   sync.Mutex
}

var apiLog *apiDB

// std is the global logger instance.
var std = &Logger{level: INFO, eventOut: os.Stderr}

// Config holds logging configuration.
type Config struct {
	Level       string // DEBUG, INFO, WARN, ERROR
	EventFile   string // path to foci.log
	APIFile     string // path to api.jsonl
	PayloadFile string // path to api-payload.jsonl (empty = disabled)
}

// Init initializes the global logger. Call once at startup.
// Any events logged before Init are replayed to the event file so that
// early messages (e.g. config warnings) appear in the log.
func Init(cfg Config) error {
	level := ParseLevel(cfg.Level)

	// Event log: stderr always, plus file if configured
	var eventOut io.Writer = os.Stderr
	var eventFile *os.File
	if cfg.EventFile != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.EventFile), 0755); err != nil {
			return fmt.Errorf("create log dir for %s: %w", cfg.EventFile, err)
		}
		f, err := os.OpenFile(cfg.EventFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("open event log %s: %w", cfg.EventFile, err)
		}
		eventFile = f
		eventOut = io.MultiWriter(os.Stderr, f)
	}

	// API log
	var apiFile *os.File
	if cfg.APIFile != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.APIFile), 0755); err != nil {
			return fmt.Errorf("create log dir for %s: %w", cfg.APIFile, err)
		}
		f, err := os.OpenFile(cfg.APIFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("open API log %s: %w", cfg.APIFile, err)
		}
		apiFile = f
	}

	// Payload log (full request/response bodies)
	var payloadFile *os.File
	if cfg.PayloadFile != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.PayloadFile), 0755); err != nil {
			return fmt.Errorf("create log dir for %s: %w", cfg.PayloadFile, err)
		}
		f, err := os.OpenFile(cfg.PayloadFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("open payload log %s: %w", cfg.PayloadFile, err)
		}
		payloadFile = f
	}

	std.mu.Lock()
	// Replay buffered pre-Init events to the event file (not stderr —
	// they were already written there when originally logged).
	if eventFile != nil && len(std.buffer) > 0 {
		for _, line := range std.buffer {
			_, _ = eventFile.WriteString(line)
		}
	}
	std.buffer = nil
	std.initialized = true
	std.level = level
	std.eventOut = eventOut
	std.eventFile = eventFile
	std.apiFile = apiFile
	std.payloadFile = payloadFile
	std.eventPath = cfg.EventFile
	std.apiPath = cfg.APIFile
	std.payloadPath = cfg.PayloadFile
	std.mu.Unlock()

	return nil
}

// InitAPIDB opens (or creates) the SQLite API call log.
func InitAPIDB(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open api db: %w", err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		_ = db.Close()
		return fmt.Errorf("set busy timeout: %w", err)
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS api_calls (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		ts                 DATETIME NOT NULL,
		session            TEXT NOT NULL,
		model              TEXT NOT NULL,
		input_tokens       INTEGER,
		output_tokens      INTEGER,
		cache_read_tokens  INTEGER,
		cache_write_tokens INTEGER,
		cost_usd           REAL,
		duration_ms        INTEGER,
		stop_reason        TEXT,
		call_type          TEXT NOT NULL,
		session_file       TEXT,
		session_line       INTEGER
	)`)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("create api_calls table: %w", err)
	}

	// Indexes for common queries
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_api_calls_ts ON api_calls(ts)`); err != nil {
		std.event(WARN, "api_db", "create ts index: %v", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_api_calls_session ON api_calls(session)`); err != nil {
		std.event(WARN, "api_db", "create session index: %v", err)
	}

	stmt, err := db.Prepare(`INSERT INTO api_calls
		(ts, session, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		 cost_usd, duration_ms, stop_reason, call_type, session_file, session_line)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("prepare insert: %w", err)
	}

	apiLog = &apiDB{db: db, stmt: stmt}
	return nil
}

// CloseAPIDB closes the SQLite API call log.
func CloseAPIDB() {
	if apiLog != nil {
		_ = apiLog.stmt.Close()
		_ = apiLog.db.Close()
		apiLog = nil
	}
}

// Close closes log files.
func Close() {
	std.mu.Lock()
	defer std.mu.Unlock()
	if std.eventFile != nil {
		_ = std.eventFile.Close()
		std.eventFile = nil
	}
	if std.apiFile != nil {
		_ = std.apiFile.Close()
		std.apiFile = nil
	}
	if std.payloadFile != nil {
		_ = std.payloadFile.Close()
		std.payloadFile = nil
	}
}

// Reopen closes and reopens all log files. Used by rotation to pick up
// the new file after the old one has been atomically replaced.
func Reopen() error {
	return std.reopen()
}

func (l *Logger) reopen() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Event file
	if l.eventFile != nil {
		_ = l.eventFile.Close()
		f, err := os.OpenFile(l.eventPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("reopen event log %s: %w", l.eventPath, err)
		}
		l.eventFile = f
		l.eventOut = io.MultiWriter(os.Stderr, f)
	}

	// API file
	if l.apiFile != nil {
		_ = l.apiFile.Close()
		f, err := os.OpenFile(l.apiPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("reopen API log %s: %w", l.apiPath, err)
		}
		l.apiFile = f
	}

	// Payload file
	if l.payloadFile != nil {
		_ = l.payloadFile.Close()
		f, err := os.OpenFile(l.payloadPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("reopen payload log %s: %w", l.payloadPath, err)
		}
		l.payloadFile = f
	}

	return nil
}

// FilePaths returns the configured log file paths.
func FilePaths() (event, api, payload string) {
	std.mu.Lock()
	defer std.mu.Unlock()
	return std.eventPath, std.apiPath, std.payloadPath
}

// WarnHook is called for each WARN or ERROR log event, if set.
// The callback receives the severity level, component, and message.
// Used to inject warnings into the agent session.
var WarnHook func(level Level, component string, msg string)

// event writes a formatted log line if the level is at or above the configured level.
func (l *Logger) event(level Level, component string, format string, args ...interface{}) {
	if level < l.level {
		return
	}

	msg := fmt.Sprintf(format, args...)
	ts := time.Now().UTC().Format(time.RFC3339)

	// Pad level to 5 chars: "DEBUG", "INFO ", "WARN ", "ERROR"
	levelStr := fmt.Sprintf("%-5s", level.String())

	line := fmt.Sprintf("%s %s [%s] %s\n", ts, levelStr, component, msg)

	l.mu.Lock()
	_, _ = l.eventOut.Write([]byte(line))
	if !l.initialized {
		l.buffer = append(l.buffer, line)
	}
	l.mu.Unlock()

	// Fire warn hook for WARN and ERROR levels
	if (level == WARN || level == ERROR) && WarnHook != nil {
		WarnHook(level, component, msg)
	}
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

func (a *apiDB) insert(entry APIEntry) {
	ts := entry.Timestamp.UTC().Format(time.RFC3339)

	var sessionFile *string
	if entry.SessionFile != "" {
		sessionFile = &entry.SessionFile
	}
	var sessionLine *int
	if entry.SessionLine > 0 {
		sessionLine = &entry.SessionLine
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	_, err := a.stmt.Exec(
		ts, entry.Session, entry.Model,
		entry.Input, entry.Output, entry.CacheRead, entry.CacheWrite,
		entry.CostUSD, entry.DurationMS, entry.StopReason,
		entry.CallType, sessionFile, sessionLine,
	)
	if err != nil {
		std.event(ERROR, "api_db", "insert error: %v", err)
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

// ComponentLogger carries a fixed component prefix for structured logging.
type ComponentLogger struct {
	component string
}

// NewComponentLogger creates a logger with a fixed component prefix.
func NewComponentLogger(component string) *ComponentLogger {
	return &ComponentLogger{component: component}
}

func (cl *ComponentLogger) Debugf(format string, args ...interface{}) {
	std.event(DEBUG, cl.component, format, args...)
}
func (cl *ComponentLogger) Infof(format string, args ...interface{}) {
	std.event(INFO, cl.component, format, args...)
}
func (cl *ComponentLogger) Warnf(format string, args ...interface{}) {
	std.event(WARN, cl.component, format, args...)
}
func (cl *ComponentLogger) Errorf(format string, args ...interface{}) {
	std.event(ERROR, cl.component, format, args...)
}

// Package-level functions for the global logger.

func Debugf(component string, format string, args ...interface{}) {
	std.event(DEBUG, component, format, args...)
}

func Infof(component string, format string, args ...interface{}) {
	std.event(INFO, component, format, args...)
}

func Warnf(component string, format string, args ...interface{}) {
	std.event(WARN, component, format, args...)
}

func Errorf(component string, format string, args ...interface{}) {
	std.event(ERROR, component, format, args...)
}

func API(entry APIEntry) {
	std.api(entry)
}

func Payload(entry PayloadEntry) {
	std.payload(entry)
}

// Fatalf logs at ERROR level and exits.
func Fatalf(component string, format string, args ...interface{}) {
	std.event(ERROR, component, format, args...)
	os.Exit(1)
}

// SetLevel changes the log level at runtime.
func SetLevel(level Level) {
	std.mu.Lock()
	std.level = level
	std.mu.Unlock()
}

// GetLevel returns the current log level.
func GetLevel() Level {
	std.mu.Lock()
	defer std.mu.Unlock()
	return std.level
}

// SetOutput replaces the event output writer (for testing).
func SetOutput(w io.Writer) {
	std.mu.Lock()
	std.eventOut = w
	std.mu.Unlock()
}

// SetAPIWriter replaces the API log file (for testing).
func SetAPIWriter(f *os.File) {
	std.mu.Lock()
	std.apiFile = f
	std.mu.Unlock()
}

// CalculateCost returns the estimated cost in USD for an API request.
func CalculateCost(model string, input, output, cacheRead, cacheWrite int) float64 {
	type pricing struct {
		input, output, cacheRead, cacheWrite float64 // per million tokens
	}

	prices := map[string]pricing{
		"claude-haiku-4-5":  {1.00, 5.00, 0.10, 1.25},
		"claude-sonnet-4-5": {3.00, 15.00, 0.30, 3.75},
		"claude-opus-4-6":   {15.00, 75.00, 1.50, 18.75},
	}

	p, ok := prices[model]
	if !ok {
		p = prices["claude-haiku-4-5"]
	}

	mtok := 1_000_000.0
	return float64(input)/mtok*p.input +
		float64(output)/mtok*p.output +
		float64(cacheRead)/mtok*p.cacheRead +
		float64(cacheWrite)/mtok*p.cacheWrite
}

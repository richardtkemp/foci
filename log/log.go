package log

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
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
	Timestamp  time.Time `json:"ts"`
	Session    string    `json:"session"`
	Model      string    `json:"model"`
	Input      int       `json:"input"`
	Output     int       `json:"output"`
	CacheRead  int       `json:"cache_read"`
	CacheWrite int       `json:"cache_write"`
	CostUSD    float64   `json:"cost_usd"`
	DurationMS int64     `json:"duration_ms"`
}

// Logger writes event log lines and structured API log entries.
type Logger struct {
	level    Level
	eventOut io.Writer // clod.log + stderr multiwriter
	apiFile  *os.File  // api.jsonl (nil if disabled)
	mu       sync.Mutex
}

// std is the global logger instance.
var std = &Logger{level: INFO, eventOut: os.Stderr}

// Config holds logging configuration.
type Config struct {
	Level       string // DEBUG, INFO, WARN, ERROR
	EventFile   string // path to clod.log
	APIFile     string // path to api.jsonl
}

// Init initializes the global logger. Call once at startup.
func Init(cfg Config) error {
	level := ParseLevel(cfg.Level)

	// Event log: stderr always, plus file if configured
	var eventOut io.Writer = os.Stderr
	if cfg.EventFile != "" {
		f, err := os.OpenFile(cfg.EventFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("open event log %s: %w", cfg.EventFile, err)
		}
		eventOut = io.MultiWriter(os.Stderr, f)
	}

	// API log
	var apiFile *os.File
	if cfg.APIFile != "" {
		f, err := os.OpenFile(cfg.APIFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("open API log %s: %w", cfg.APIFile, err)
		}
		apiFile = f
	}

	std.mu.Lock()
	std.level = level
	std.eventOut = eventOut
	std.apiFile = apiFile
	std.mu.Unlock()

	return nil
}

// Close closes log files.
func Close() {
	std.mu.Lock()
	defer std.mu.Unlock()
	if std.apiFile != nil {
		std.apiFile.Close()
		std.apiFile = nil
	}
}

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
	l.eventOut.Write([]byte(line))
	l.mu.Unlock()
}

// api writes a structured API log entry.
func (l *Logger) api(entry APIEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.apiFile == nil {
		return
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	l.apiFile.Write(append(data, '\n'))
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

package log

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/timeutil"
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

// DebugLogKeySuffix controls whether API key suffixes are included in
// provider call logs. Set from config at startup (config.Debug.LogAPIKeySuffix).
var DebugLogKeySuffix bool

// FormatKeySuffix returns a formatted key suffix like "...agAA" for the last
// 4 characters of an API key. Returns "" when DebugLogKeySuffix is false,
// the key is too short, or the key is empty.
func FormatKeySuffix(key string) string {
	if !DebugLogKeySuffix || len(key) < 4 {
		return ""
	}
	return "..." + key[len(key)-4:]
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

// Logger writes event log lines and structured API log entries.
type Logger struct {
	level       Level
	eventOut    io.Writer   // foci.log + stderr multiwriter
	eventFile   *os.File    // foci.log file handle (nil = stderr only)
	apiFile     *os.File    // api.jsonl (nil if disabled)
	payloadFile *os.File    // api-payload.jsonl (nil if disabled)
	eventPath   string      // path to foci.log
	apiPath     string      // path to api.jsonl
	payloadPath string      // path to api-payload.jsonl
	fileMode    os.FileMode // permission bits for log files
	buffer      []string    // pre-Init event lines, replayed to event file on Init
	initialized bool        // true after Init completes
	mu          sync.Mutex
}

// std is the global logger instance.
var std = &Logger{level: INFO, eventOut: os.Stderr}

// Config holds logging configuration.
type Config struct {
	Level       string      // DEBUG, INFO, WARN, ERROR
	EventFile   string      // path to foci.log
	APIFile     string      // path to api.jsonl
	PayloadFile string      // path to api-payload.jsonl (empty = disabled)
	FileMode    os.FileMode // permission bits for log files (default 0600)
}

// Init initializes the global logger. Safe to call more than once — any
// previously opened file handles are closed before replacement. Events
// logged before the first Init are replayed to the event file so that
// early messages (e.g. config warnings) appear in the log.
func Init(cfg Config) error {
	// HACK: SetAPIWriter is only used by cross-package tests
	// (agent/integration_test.go) but can't live in a _test.go file because
	// Go doesn't allow cross-package access to test-only symbols. This
	// unreachable call prevents the deadcode linter from flagging it.
	if time.Now().Year() < 1900 {
		SetAPIWriter(nil)
	}

	level := ParseLevel(cfg.Level)

	fileMode := cfg.FileMode
	if fileMode == 0 {
		fileMode = 0600
	}

	// Event log: stderr always, plus file if configured
	var eventOut io.Writer = os.Stderr
	var eventFile *os.File
	if cfg.EventFile != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.EventFile), 0755); err != nil {
			return fmt.Errorf("create log dir for %s: %w", cfg.EventFile, err)
		}
		f, err := os.OpenFile(cfg.EventFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
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
		f, err := os.OpenFile(cfg.APIFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
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
		f, err := os.OpenFile(cfg.PayloadFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
		if err != nil {
			return fmt.Errorf("open payload log %s: %w", cfg.PayloadFile, err)
		}
		payloadFile = f
	}

	std.mu.Lock()
	// Close any previously opened file handles (safe to call Init twice,
	// e.g. early init with defaults then full init after config load).
	if std.eventFile != nil {
		_ = std.eventFile.Close()
	}
	if std.apiFile != nil {
		_ = std.apiFile.Close()
	}
	if std.payloadFile != nil {
		_ = std.payloadFile.Close()
	}
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
	std.fileMode = fileMode
	std.mu.Unlock()

	return nil
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

	fileMode := l.fileMode
	if fileMode == 0 {
		fileMode = 0600
	}

	// Event file
	if l.eventFile != nil {
		_ = l.eventFile.Close()
		f, err := os.OpenFile(l.eventPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
		if err != nil {
			return fmt.Errorf("reopen event log %s: %w", l.eventPath, err)
		}
		l.eventFile = f
		l.eventOut = io.MultiWriter(os.Stderr, f)
	}

	// API file
	if l.apiFile != nil {
		_ = l.apiFile.Close()
		f, err := os.OpenFile(l.apiPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
		if err != nil {
			return fmt.Errorf("reopen API log %s: %w", l.apiPath, err)
		}
		l.apiFile = f
	}

	// Payload file
	if l.payloadFile != nil {
		_ = l.payloadFile.Close()
		f, err := os.OpenFile(l.payloadPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
		if err != nil {
			return fmt.Errorf("reopen payload log %s: %w", l.payloadPath, err)
		}
		l.payloadFile = f
	}

	return nil
}

// warnHookEntry is a buffered warning from before the hook was set.
type warnHookEntry struct {
	level     Level
	component string
	msg       string
}

var (
	// warnHook is called for each WARN or ERROR log event, if set.
	// The callback receives the severity level, component, and message.
	// Used to inject warnings into the agent session.
	// Set via SetWarnHook, which replays any buffered early warnings.
	warnHook   func(level Level, component string, msg string)
	warnBuffer []warnHookEntry
	warnMu     sync.Mutex
)

// SetWarnHook sets the warn hook and replays any warnings that were
// buffered before the hook was ready.
func SetWarnHook(fn func(level Level, component string, msg string)) {
	warnMu.Lock()
	defer warnMu.Unlock()
	warnHook = fn
	for _, e := range warnBuffer {
		fn(e.level, e.component, e.msg)
	}
	warnBuffer = nil
}

// event writes a formatted log line if the level is at or above the configured level.
func (l *Logger) event(level Level, component string, format string, args ...interface{}) {
	if level < l.level {
		return
	}

	msg := strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", "\\n")
	ts := timeutil.Format(timeutil.Now())

	// Pad level to 5 chars: "DEBUG", "INFO ", "WARN ", "ERROR"
	levelStr := fmt.Sprintf("%-5s", level.String())

	line := fmt.Sprintf("%s %s [%s] %s\n", ts, levelStr, component, msg)

	l.mu.Lock()
	_, _ = l.eventOut.Write([]byte(line))
	if !l.initialized {
		l.buffer = append(l.buffer, line)
	}
	l.mu.Unlock()

	// Fire warn hook for WARN and ERROR levels, buffering if hook not yet set.
	if level == WARN || level == ERROR {
		warnMu.Lock()
		if warnHook != nil {
			warnMu.Unlock()
			warnHook(level, component, msg)
		} else {
			warnBuffer = append(warnBuffer, warnHookEntry{level, component, msg})
			warnMu.Unlock()
		}
	}
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

// Extra logs a verbose "xtra:" investigation line for this logger's component,
// gated by the package's [debug] extra_<package>_logging flag. No-op (one
// atomic load) when the package is not enabled. See package-level Extra.
func (cl *ComponentLogger) Extra(format string, args ...interface{}) {
	Extra(cl.component, format, args...)
}

// --- Per-package "extra" verbose logging (gated by [debug] config) ---
//
// Investigation-grade logging that is OFF by default and switched on per
// package via [debug] extra_<package>_logging flags (see config.DebugConfig).
// When a package is enabled, Extra() emits at INFO with the component tagged
// "xtra:<component>" — so the lines surface at the default log level (no need
// to drop the global level to DEBUG and flood every package) and are trivially
// greppable: "xtra:" for all of them, "xtra:ccstream" for one (also matches a
// labelled "xtra:ccstream:clutch"), "xtra:(ccstream|telegram)" for several.
//
// The enabled set is built once at startup (EnableExtra, from the resolved
// [debug] flags) and read lock-free on hot logging paths via an atomic
// snapshot pointer.
var extraLogging atomic.Pointer[map[string]bool]

// EnableExtra turns on "xtra:" verbose logging for a base package component
// (e.g. "ccstream", "telegram", "inbox"). Call at startup, before serving
// traffic. Idempotent; copy-on-write so concurrent readers never see a torn map.
func EnableExtra(component string) {
	next := map[string]bool{}
	if cur := extraLogging.Load(); cur != nil {
		for k, v := range *cur {
			next[k] = v
		}
	}
	next[component] = true
	extraLogging.Store(&next)
}

// ExtraEnabled reports whether verbose logging is on for a component. A labelled
// component ("ccstream:clutch") matches its base package enable ("ccstream"),
// so a single flag covers all instances of a package.
func ExtraEnabled(component string) bool {
	m := extraLogging.Load()
	if m == nil {
		return false
	}
	if (*m)[component] {
		return true
	}
	if i := strings.IndexByte(component, ':'); i > 0 {
		return (*m)[component[:i]]
	}
	return false
}

// Extra logs a verbose investigation line for component, but only when that
// package has been enabled via EnableExtra. Tagged "xtra:<component>" and
// emitted at INFO. Cost when disabled is a single atomic load.
func Extra(component string, format string, args ...interface{}) {
	if !ExtraEnabled(component) {
		return
	}
	std.event(INFO, "xtra:"+component, format, args...)
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

// Fatalf logs at ERROR level and exits.
func Fatalf(component string, format string, args ...interface{}) {
	std.event(ERROR, component, format, args...)
	os.Exit(1)
}

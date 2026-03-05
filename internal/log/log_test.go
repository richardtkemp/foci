package log

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resetGlobal restores the global logger to its initial state for test isolation.
func resetGlobal() {
	std.mu.Lock()
	std.level = INFO
	std.eventOut = os.Stderr
	std.apiFile = nil
	std.payloadFile = nil
	std.buffer = nil
	std.initialized = false
	std.mu.Unlock()
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  Level
	}{
		{"DEBUG", DEBUG},
		{"debug", DEBUG},
		{"INFO", INFO},
		{"info", INFO},
		{"WARN", WARN},
		{"warn", WARN},
		{"ERROR", ERROR},
		{"error", ERROR},
		{"  INFO  ", INFO},
		{"unknown", INFO},
		{"", INFO},
	}
	for _, tt := range tests {
		got := ParseLevel(tt.input)
		if got != tt.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestLevelString(t *testing.T) {
	tests := []struct {
		level Level
		want  string
	}{
		{DEBUG, "DEBUG"},
		{INFO, "INFO"},
		{WARN, "WARN"},
		{ERROR, "ERROR"},
	}
	for _, tt := range tests {
		if got := tt.level.String(); got != tt.want {
			t.Errorf("Level(%d).String() = %q, want %q", tt.level, got, tt.want)
		}
	}
}

func TestEventLogFormat(t *testing.T) {
	var buf bytes.Buffer
	SetOutput(&buf)
	SetLevel(DEBUG)
	defer SetLevel(INFO)
	defer SetOutput(os.Stderr)

	Infof("telegram", "bot started as @%s", "testbot")

	line := buf.String()

	// Should contain timestamp, level, component, message
	if !strings.Contains(line, "INFO") {
		t.Errorf("missing INFO in %q", line)
	}
	if !strings.Contains(line, "[telegram]") {
		t.Errorf("missing [telegram] in %q", line)
	}
	if !strings.Contains(line, "bot started as @testbot") {
		t.Errorf("missing message in %q", line)
	}
	// Timestamp should be RFC3339
	if !strings.Contains(line, "T") || !strings.Contains(line, "Z") {
		t.Errorf("timestamp not RFC3339 in %q", line)
	}
	// Should end with newline
	if !strings.HasSuffix(line, "\n") {
		t.Errorf("missing trailing newline in %q", line)
	}
}

func TestEventLogLevelPadding(t *testing.T) {
	var buf bytes.Buffer
	SetOutput(&buf)
	SetLevel(DEBUG)
	defer SetLevel(INFO)
	defer SetOutput(os.Stderr)

	Debugf("test", "debug msg")
	buf.Reset()
	Infof("test", "info msg")
	line := buf.String()
	if !strings.Contains(line, "INFO ") {
		t.Errorf("INFO not padded to 5 chars in %q", line)
	}

	buf.Reset()
	Warnf("test", "warn msg")
	line = buf.String()
	if !strings.Contains(line, "WARN ") {
		t.Errorf("WARN not padded to 5 chars in %q", line)
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	SetOutput(&buf)
	defer SetOutput(os.Stderr)

	SetLevel(WARN)
	defer SetLevel(INFO)

	Debugf("test", "debug")
	Infof("test", "info")
	Warnf("test", "warn")
	Errorf("test", "error")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (warn + error): %q", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "WARN") {
		t.Errorf("line 0 should be WARN: %q", lines[0])
	}
	if !strings.Contains(lines[1], "ERROR") {
		t.Errorf("line 1 should be ERROR: %q", lines[1])
	}
}

func TestDebugFilteredAtInfoLevel(t *testing.T) {
	var buf bytes.Buffer
	SetOutput(&buf)
	defer SetOutput(os.Stderr)

	SetLevel(INFO)

	Debugf("test", "should not appear")

	if buf.Len() != 0 {
		t.Errorf("debug message should be filtered at INFO level: %q", buf.String())
	}
}

func TestAPILog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api.jsonl")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("create api log: %v", err)
	}
	SetAPIWriter(f)
	defer func() {
		SetAPIWriter(nil)
		f.Close()
	}()

	entry := APIEntry{
		Timestamp:  time.Date(2026, 2, 21, 3, 52, 41, 0, time.UTC),
		Session:    "agent:main:main",
		Model:      "claude-haiku-4-5",
		Input:      1119,
		Output:     164,
		CacheRead:  0,
		CacheWrite: 1119,
		CostUSD:    0.003,
		DurationMS: 1240,
	}

	API(entry)

	// Close and re-read
	f.Close()
	data, _ := os.ReadFile(path)

	var decoded APIEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal API entry: %v\nraw: %s", err, string(data))
	}

	if decoded.Session != "agent:main:main" {
		t.Errorf("Session = %q", decoded.Session)
	}
	if decoded.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q", decoded.Model)
	}
	if decoded.Input != 1119 {
		t.Errorf("Input = %d", decoded.Input)
	}
	if decoded.Output != 164 {
		t.Errorf("Output = %d", decoded.Output)
	}
	if decoded.CacheWrite != 1119 {
		t.Errorf("CacheWrite = %d", decoded.CacheWrite)
	}
	if decoded.DurationMS != 1240 {
		t.Errorf("DurationMS = %d", decoded.DurationMS)
	}
}

func TestAPILogDisabled(t *testing.T) {
	// With no API file set, API() should not panic
	SetAPIWriter(nil)
	API(APIEntry{Session: "test"})
	// No panic = pass
}

func TestInitWithFiles(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	dir := t.TempDir()
	eventPath := filepath.Join(dir, "foci.log")
	apiPath := filepath.Join(dir, "api.jsonl")

	err := Init(Config{
		Level:     "DEBUG",
		EventFile: eventPath,
		APIFile:   apiPath,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	Infof("test", "hello from init test")
	API(APIEntry{Session: "init-test", Model: "test", DurationMS: 100})

	// Event log should exist on disk
	data, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	if !strings.Contains(string(data), "hello from init test") {
		t.Errorf("event log missing message: %s", string(data))
	}

	// API log should exist on disk
	data, err = os.ReadFile(apiPath)
	if err != nil {
		t.Fatalf("read api log: %v", err)
	}
	if !strings.Contains(string(data), "init-test") {
		t.Errorf("api log missing entry: %s", string(data))
	}
}

func TestInitBadEventPath(t *testing.T) {
	err := Init(Config{EventFile: "/nonexistent/dir/foci.log"})
	if err == nil {
		t.Fatal("expected error for bad event file path")
	}
}

func TestInitBadAPIPath(t *testing.T) {
	err := Init(Config{APIFile: "/nonexistent/dir/api.jsonl"})
	if err == nil {
		t.Fatal("expected error for bad API file path")
	}
}

func TestCalculateCost(t *testing.T) {
	// 1M input tokens on Haiku = $1.00
	cost := CalculateCost("claude-haiku-4-5", 1_000_000, 0, 0, 0)
	if cost != 1.0 {
		t.Errorf("1M input haiku = %f, want 1.0", cost)
	}

	// 1M output tokens on Haiku = $5.00
	cost = CalculateCost("claude-haiku-4-5", 0, 1_000_000, 0, 0)
	if cost != 5.0 {
		t.Errorf("1M output haiku = %f, want 5.0", cost)
	}

	// 1M cache read on Haiku = $0.10
	cost = CalculateCost("claude-haiku-4-5", 0, 0, 1_000_000, 0)
	if cost != 0.1 {
		t.Errorf("1M cache read haiku = %f, want 0.1", cost)
	}

	// 1M cache write on Haiku = $1.25
	cost = CalculateCost("claude-haiku-4-5", 0, 0, 0, 1_000_000)
	if cost != 1.25 {
		t.Errorf("1M cache write haiku = %f, want 1.25", cost)
	}

	// Mixed: realistic request
	cost = CalculateCost("claude-haiku-4-5", 500, 100, 2000, 1000)
	expected := 500.0/1e6*1.0 + 100.0/1e6*5.0 + 2000.0/1e6*0.1 + 1000.0/1e6*1.25
	if cost != expected {
		t.Errorf("mixed cost = %f, want %f", cost, expected)
	}

	// Unknown model uses haiku pricing
	cost = CalculateCost("unknown-model", 1_000_000, 0, 0, 0)
	if cost != 1.0 {
		t.Errorf("unknown model = %f, want 1.0 (haiku fallback)", cost)
	}
}

func TestCalculateCostOpus(t *testing.T) {
	cost := CalculateCost("claude-opus-4-6", 1_000_000, 0, 0, 0)
	if cost != 15.0 {
		t.Errorf("1M input opus = %f, want 15.0", cost)
	}
}

func TestCalculateCostGemini(t *testing.T) {
	// 1M input on gemini-2.5-flash = $0.15
	cost := CalculateCost("gemini-2.5-flash", 1_000_000, 0, 0, 0)
	if cost != 0.15 {
		t.Errorf("1M input flash = %f, want 0.15", cost)
	}

	// 1M output on gemini-2.5-pro = $10.00
	cost = CalculateCost("gemini-2.5-pro", 0, 1_000_000, 0, 0)
	if cost != 10.0 {
		t.Errorf("1M output pro = %f, want 10.0", cost)
	}

	// Unknown gemini model uses flash pricing
	cost = CalculateCost("gemini-3.0-ultra", 1_000_000, 0, 0, 0)
	if cost != 0.15 {
		t.Errorf("unknown gemini = %f, want 0.15 (flash fallback)", cost)
	}
}

func TestMultipleAPIEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api.jsonl")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	SetAPIWriter(f)
	defer func() {
		SetAPIWriter(nil)
		f.Close()
	}()

	for i := 0; i < 3; i++ {
		API(APIEntry{Session: "test", DurationMS: int64(i * 100)})
	}

	f.Close()
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Errorf("got %d lines, want 3", len(lines))
	}
}

func TestPreInitBufferReplay(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	dir := t.TempDir()
	eventPath := filepath.Join(dir, "foci.log")

	// Log before Init — should go to stderr (captured by SetOutput)
	// and be buffered for replay.
	var stderrBuf bytes.Buffer
	SetOutput(&stderrBuf)

	Warnf("config", "unknown key: foo.bar")
	Infof("startup", "loading config from foci.toml")

	// Verify buffer has two entries
	std.mu.Lock()
	bufLen := len(std.buffer)
	std.mu.Unlock()
	if bufLen != 2 {
		t.Fatalf("buffer len = %d, want 2", bufLen)
	}

	// Verify stderr got the messages
	if !strings.Contains(stderrBuf.String(), "unknown key: foo.bar") {
		t.Errorf("stderr missing pre-Init warning: %q", stderrBuf.String())
	}

	// Now Init — should replay buffered events to the event file
	err := Init(Config{
		Level:     "DEBUG",
		EventFile: eventPath,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	// Event file should contain the replayed pre-Init messages
	data, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "unknown key: foo.bar") {
		t.Errorf("event file missing replayed warning: %s", content)
	}
	if !strings.Contains(content, "loading config from foci.toml") {
		t.Errorf("event file missing replayed info: %s", content)
	}

	// Buffer should be cleared after Init
	std.mu.Lock()
	bufLen = len(std.buffer)
	std.mu.Unlock()
	if bufLen != 0 {
		t.Errorf("buffer should be cleared after Init, got %d entries", bufLen)
	}

	// Post-Init messages should NOT be buffered
	Infof("test", "post-init message")
	std.mu.Lock()
	bufLen = len(std.buffer)
	std.mu.Unlock()
	if bufLen != 0 {
		t.Errorf("buffer should stay empty after Init, got %d entries", bufLen)
	}
}

func TestPreInitBufferNoFile(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	// Log before Init
	var buf bytes.Buffer
	SetOutput(&buf)

	Warnf("test", "pre-init warning")

	// Init without an event file — buffer is cleared but not replayed
	err := Init(Config{Level: "INFO"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	std.mu.Lock()
	bufLen := len(std.buffer)
	std.mu.Unlock()
	if bufLen != 0 {
		t.Errorf("buffer should be cleared after Init, got %d entries", bufLen)
	}
}

func TestFilePaths(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	dir := t.TempDir()
	eventPath := filepath.Join(dir, "foci.log")
	apiPath := filepath.Join(dir, "api.jsonl")
	payloadPath := filepath.Join(dir, "payload.jsonl")

	err := Init(Config{
		Level:       "INFO",
		EventFile:   eventPath,
		APIFile:     apiPath,
		PayloadFile: payloadPath,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	gotEvent, gotAPI, gotPayload := FilePaths()
	if gotEvent != eventPath {
		t.Errorf("event path = %q, want %q", gotEvent, eventPath)
	}
	if gotAPI != apiPath {
		t.Errorf("api path = %q, want %q", gotAPI, apiPath)
	}
	if gotPayload != payloadPath {
		t.Errorf("payload path = %q, want %q", gotPayload, payloadPath)
	}
}

func TestGetLevel(t *testing.T) {
	SetLevel(WARN)
	defer SetLevel(INFO)

	if got := GetLevel(); got != WARN {
		t.Errorf("GetLevel() = %v, want WARN", got)
	}

	SetLevel(DEBUG)
	if got := GetLevel(); got != DEBUG {
		t.Errorf("GetLevel() = %v, want DEBUG", got)
	}
}

func TestPayloadEnabled(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	// No payload file — should be false
	if PayloadEnabled() {
		t.Error("PayloadEnabled() should be false with no payload file")
	}

	// With payload file — should be true
	dir := t.TempDir()
	err := Init(Config{
		Level:       "INFO",
		PayloadFile: filepath.Join(dir, "payload.jsonl"),
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	if !PayloadEnabled() {
		t.Error("PayloadEnabled() should be true after Init with PayloadFile")
	}
}

func TestPayloadLog(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	dir := t.TempDir()
	payloadPath := filepath.Join(dir, "payload.jsonl")

	err := Init(Config{
		Level:       "INFO",
		PayloadFile: payloadPath,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	Payload(PayloadEntry{
		Session:    "test-session",
		Model:      "test-model",
		Request:    json.RawMessage(`{"prompt":"hello"}`),
		Response:   json.RawMessage(`{"text":"world"}`),
		DurationMS: 500,
	})

	// Force close to flush
	Close()

	data, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !strings.Contains(string(data), "test-session") {
		t.Errorf("payload missing session: %s", string(data))
	}
	if !strings.Contains(string(data), "test-model") {
		t.Errorf("payload missing model: %s", string(data))
	}
}

func TestAPIDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_api.db")

	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	// Insert entries of different call types
	entries := []APIEntry{
		{
			Timestamp:  time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			Session:    "agent:main:chat:123",
			Model:      "claude-haiku-4-5",
			Input:      1000,
			Output:     200,
			CacheRead:  500,
			CacheWrite: 300,
			CostUSD:    0.005,
			DurationMS: 1200,
			StopReason: "end_turn",
			CallType:   "conversation",
		},
		{
			Timestamp:  time.Date(2026, 3, 1, 10, 1, 0, 0, time.UTC),
			Session:    "agent:main:chat:123",
			Model:      "claude-haiku-4-5",
			Input:      2000,
			Output:     400,
			CostUSD:    0.01,
			DurationMS: 2400,
			StopReason: "end_turn",
			CallType:   "compaction",
		},
		{
			Timestamp:  time.Date(2026, 3, 1, 10, 2, 0, 0, time.UTC),
			Session:    "agent:main:chat:123",
			Model:      "claude-haiku-4-5",
			Input:      500,
			Output:     100,
			CostUSD:    0.002,
			DurationMS: 800,
			StopReason: "end_turn",
			CallType:   "summary",
		},
		{
			Timestamp:   time.Date(2026, 3, 1, 10, 3, 0, 0, time.UTC),
			Session:     "agent:main:spawn:456",
			Model:       "claude-sonnet-4-5",
			Input:       3000,
			Output:      600,
			CostUSD:     0.02,
			DurationMS:  3600,
			StopReason:  "end_turn",
			CallType:    "spawn",
			SessionFile: "/data/sessions/agent/main/spawn/456.jsonl",
		},
	}

	for _, e := range entries {
		apiLog.insert(e)
	}

	// Query by call_type
	rows, err := apiLog.db.Query("SELECT call_type, count(*) FROM api_calls GROUP BY call_type ORDER BY call_type")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var ct string
		var n int
		if err := rows.Scan(&ct, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		counts[ct] = n
	}

	if counts["conversation"] != 1 {
		t.Errorf("conversation count = %d, want 1", counts["conversation"])
	}
	if counts["compaction"] != 1 {
		t.Errorf("compaction count = %d, want 1", counts["compaction"])
	}
	if counts["summary"] != 1 {
		t.Errorf("summary count = %d, want 1", counts["summary"])
	}
	if counts["spawn"] != 1 {
		t.Errorf("spawn count = %d, want 1", counts["spawn"])
	}

	// Verify session_file was stored
	var sf sql.NullString
	err = apiLog.db.QueryRow("SELECT session_file FROM api_calls WHERE call_type = 'spawn'").Scan(&sf)
	if err != nil {
		t.Fatalf("query session_file: %v", err)
	}
	if !sf.Valid || sf.String != "/data/sessions/agent/main/spawn/456.jsonl" {
		t.Errorf("session_file = %v, want /data/sessions/agent/main/spawn/456.jsonl", sf)
	}

	// Verify session_file is NULL for entries without it
	err = apiLog.db.QueryRow("SELECT session_file FROM api_calls WHERE call_type = 'conversation'").Scan(&sf)
	if err != nil {
		t.Fatalf("query session_file: %v", err)
	}
	if sf.Valid {
		t.Errorf("session_file should be NULL for conversation, got %q", sf.String)
	}

	// Query by session index
	var total int
	err = apiLog.db.QueryRow("SELECT count(*) FROM api_calls WHERE session = 'agent:main:chat:123'").Scan(&total)
	if err != nil {
		t.Fatalf("query by session: %v", err)
	}
	if total != 3 {
		t.Errorf("session count = %d, want 3", total)
	}
}

func TestAPIDBDisabled(t *testing.T) {
	// With no API DB initialized, API() should not panic
	old := apiLog
	apiLog = nil
	defer func() { apiLog = old }()

	API(APIEntry{Session: "test", CallType: "conversation"})
	// No panic = pass
}

func TestPreInitFilteredByLevel(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	// Default level is INFO, so DEBUG should not be buffered
	var buf bytes.Buffer
	SetOutput(&buf)

	Debugf("test", "debug before init")
	Infof("test", "info before init")

	std.mu.Lock()
	bufLen := len(std.buffer)
	std.mu.Unlock()
	if bufLen != 1 {
		t.Fatalf("buffer len = %d, want 1 (DEBUG filtered by INFO level)", bufLen)
	}
}

func TestNewComponentLogger(t *testing.T) {
	cl := NewComponentLogger("test-component")
	if cl == nil {
		t.Fatal("NewComponentLogger returned nil")
	}
	if cl.component != "test-component" {
		t.Errorf("component = %q, want test-component", cl.component)
	}
}

func TestComponentLoggerDebugf(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	var buf bytes.Buffer
	SetOutput(&buf)
	SetLevel(DEBUG)

	cl := NewComponentLogger("comp")
	cl.Debugf("test message")

	if !strings.Contains(buf.String(), "test message") {
		t.Errorf("debug output missing message: %s", buf.String())
	}
}

func TestComponentLoggerInfof(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	var buf bytes.Buffer
	SetOutput(&buf)

	cl := NewComponentLogger("comp")
	cl.Infof("info message")

	if !strings.Contains(buf.String(), "info message") {
		t.Errorf("info output missing message: %s", buf.String())
	}
}

func TestComponentLoggerWarnf(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	var buf bytes.Buffer
	SetOutput(&buf)

	cl := NewComponentLogger("comp")
	cl.Warnf("warn message")

	if !strings.Contains(buf.String(), "warn message") {
		t.Errorf("warn output missing message: %s", buf.String())
	}
}

func TestComponentLoggerErrorf(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	var buf bytes.Buffer
	SetOutput(&buf)

	cl := NewComponentLogger("comp")
	cl.Errorf("error message")

	if !strings.Contains(buf.String(), "error message") {
		t.Errorf("error output missing message: %s", buf.String())
	}
}

func TestPackageLevelDebugf(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	var buf bytes.Buffer
	SetOutput(&buf)
	SetLevel(DEBUG)

	Debugf("pkg", "pkg debug")

	if !strings.Contains(buf.String(), "pkg debug") {
		t.Errorf("package debug output missing message: %s", buf.String())
	}
}

func TestPackageLevelInfof(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	var buf bytes.Buffer
	SetOutput(&buf)

	Infof("pkg", "pkg info")

	if !strings.Contains(buf.String(), "pkg info") {
		t.Errorf("package info output missing message: %s", buf.String())
	}
}

func TestPackageLevelWarnf(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	var buf bytes.Buffer
	SetOutput(&buf)

	Warnf("pkg", "pkg warn")

	if !strings.Contains(buf.String(), "pkg warn") {
		t.Errorf("package warn output missing message: %s", buf.String())
	}
}

func TestPackageLevelErrorf(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	var buf bytes.Buffer
	SetOutput(&buf)

	Errorf("pkg", "pkg error")

	if !strings.Contains(buf.String(), "pkg error") {
		t.Errorf("package error output missing message: %s", buf.String())
	}
}

func TestSetWarnHook(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	var buf bytes.Buffer
	SetOutput(&buf)

	hookCalled := false
	SetWarnHook(func(level Level, component string, msg string) {
		if level == WARN && component == "test" && msg == "warn message" {
			hookCalled = true
		}
	})

	Warnf("test", "warn message")

	if !hookCalled {
		t.Error("warn hook not called with correct parameters")
	}
}

func TestIsOpenAIModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"gpt-4", true},
		{"gpt-3.5-turbo", true},
		{"o1", true},
		{"o3", true},
		{"o4", true},
		{"chatgpt-4", true},
		{"claude-3-sonnet", false},
		{"gemini-2-flash", false},
		{"", false},
	}

	for _, tt := range tests {
		got := isOpenAIModel(tt.model)
		if got != tt.want {
			t.Errorf("isOpenAIModel(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}

func TestAPIWithGemini(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	// Create temp DB for API logging
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "api.db")
	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	// API call with gemini model should auto-infer provider
	API(APIEntry{
		Session:  "test",
		Model:    "gemini-2-flash",
		CallType: "conversation",
	})
	// No error = pass
}

func TestAPIWithOpenAI(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "api.db")
	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	// API call with OpenAI model should auto-infer provider
	API(APIEntry{
		Session:  "test",
		Model:    "gpt-4",
		CallType: "conversation",
	})
	// No error = pass
}

func TestPayload(t *testing.T) {
	resetGlobal()
	defer resetGlobal()

	// Payload should not panic when file is nil
	Payload(PayloadEntry{
		Session: "test",
		Model:   "test-model",
	})
	// No panic = pass
}

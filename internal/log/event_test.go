package log

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestEventLogFormat(t *testing.T) {
	// Verifies the structured log line includes timestamp (RFC3339),
	// level, component tag, message, and trailing newline.
	buf := captureOutput(t)
	withDebugLevel(t)

	Infof("telegram", "bot started as @%s", "testbot")

	line := buf.String()

	if !strings.Contains(line, "INFO") {
		t.Errorf("missing INFO in %q", line)
	}
	if !strings.Contains(line, "[telegram]") {
		t.Errorf("missing [telegram] in %q", line)
	}
	if !strings.Contains(line, "bot started as @testbot") {
		t.Errorf("missing message in %q", line)
	}
	// Timestamp should be RFC3339 (with Z or +/-offset)
	if !strings.Contains(line, "T") || (!strings.Contains(line, "Z") && !strings.Contains(line, "+") && !strings.Contains(line, "-")) {
		t.Errorf("timestamp not RFC3339 in %q", line)
	}
	if !strings.HasSuffix(line, "\n") {
		t.Errorf("missing trailing newline in %q", line)
	}
}

func TestEventLogLevelPadding(t *testing.T) {
	// Verifies that INFO and WARN are padded to 5 characters
	// so that log columns align consistently.
	buf := captureOutput(t)
	withDebugLevel(t)

	NewComponentLogger("test").Debugf("debug msg")
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
	// Verifies that messages below the current level are suppressed:
	// at WARN level, DEBUG and INFO are dropped, WARN and ERROR pass through.
	buf := captureOutput(t)

	setLevel(WARN)
	defer setLevel(INFO)

	NewComponentLogger("test").Debugf("debug")
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
	// Verifies that DEBUG messages are not emitted at the default INFO level.
	buf := captureOutput(t)

	setLevel(INFO)

	NewComponentLogger("test").Debugf("should not appear")

	if buf.Len() != 0 {
		t.Errorf("debug message should be filtered at INFO level: %q", buf.String())
	}
}

func TestPackageLevelInfof(t *testing.T) {
	// Verifies the package-level Infof emits output at INFO level.
	resetGlobal()
	t.Cleanup(resetGlobal)
	buf := captureOutput(t)

	Infof("pkg", "pkg info")

	if !strings.Contains(buf.String(), "pkg info") {
		t.Errorf("package info output missing message: %s", buf.String())
	}
}

func TestPackageLevelWarnf(t *testing.T) {
	// Verifies the package-level Warnf emits output.
	resetGlobal()
	t.Cleanup(resetGlobal)
	buf := captureOutput(t)

	Warnf("pkg", "pkg warn")

	if !strings.Contains(buf.String(), "pkg warn") {
		t.Errorf("package warn output missing message: %s", buf.String())
	}
}

func TestPackageLevelErrorf(t *testing.T) {
	// Verifies the package-level Errorf emits output.
	resetGlobal()
	t.Cleanup(resetGlobal)
	buf := captureOutput(t)

	Errorf("pkg", "pkg error")

	if !strings.Contains(buf.String(), "pkg error") {
		t.Errorf("package error output missing message: %s", buf.String())
	}
}

func TestNewComponentLogger(t *testing.T) {
	// Verifies NewComponentLogger returns a non-nil logger
	// with the correct component name set.
	cl := NewComponentLogger("test-component")
	if cl == nil {
		t.Fatal("NewComponentLogger returned nil")
	}
	if cl.component != "test-component" {
		t.Errorf("component = %q, want test-component", cl.component)
	}
}

func TestComponentLoggerDebugf(t *testing.T) {
	// Verifies the component logger's Debugf emits at DEBUG level.
	resetGlobal()
	t.Cleanup(resetGlobal)
	buf := captureOutput(t)
	withDebugLevel(t)

	cl := NewComponentLogger("comp")
	cl.Debugf("test message")

	if !strings.Contains(buf.String(), "test message") {
		t.Errorf("debug output missing message: %s", buf.String())
	}
}

func TestComponentLoggerInfof(t *testing.T) {
	// Verifies the component logger's Infof emits at INFO level.
	resetGlobal()
	t.Cleanup(resetGlobal)
	buf := captureOutput(t)

	cl := NewComponentLogger("comp")
	cl.Infof("info message")

	if !strings.Contains(buf.String(), "info message") {
		t.Errorf("info output missing message: %s", buf.String())
	}
}

func TestComponentLoggerWarnf(t *testing.T) {
	// Verifies the component logger's Warnf emits output.
	resetGlobal()
	t.Cleanup(resetGlobal)
	buf := captureOutput(t)

	cl := NewComponentLogger("comp")
	cl.Warnf("warn message")

	if !strings.Contains(buf.String(), "warn message") {
		t.Errorf("warn output missing message: %s", buf.String())
	}
}

func TestComponentLoggerErrorf(t *testing.T) {
	// Verifies the component logger's Errorf emits output.
	resetGlobal()
	t.Cleanup(resetGlobal)
	buf := captureOutput(t)

	cl := NewComponentLogger("comp")
	cl.Errorf("error message")

	if !strings.Contains(buf.String(), "error message") {
		t.Errorf("error output missing message: %s", buf.String())
	}
}

func TestSetWarnHook(t *testing.T) {
	// Verifies that a registered warn hook is called with the correct
	// level, component, and message when Warnf is invoked.
	resetGlobal()
	t.Cleanup(resetGlobal)
	captureOutput(t)

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

func TestWarnHookBuffering(t *testing.T) {
	// Verifies that warnings logged before SetWarnHook is called
	// are buffered and replayed when the hook is registered, and that subsequent
	// warnings fire directly without buffering.
	resetGlobal()
	t.Cleanup(resetGlobal)

	// Reset warn hook state
	warnMu.Lock()
	warnHook = nil
	warnBuffer = nil
	warnMu.Unlock()

	captureOutput(t)

	// Log warnings before hook is set
	Warnf("config", "early warning 1")
	Errorf("config", "early error 2")

	// Now set the hook — buffered warnings should be replayed
	var replayed []warnHookEntry
	SetWarnHook(func(level Level, component string, msg string) {
		replayed = append(replayed, warnHookEntry{level, component, msg})
	})

	if len(replayed) != 2 {
		t.Fatalf("replayed %d buffered warnings, want 2", len(replayed))
	}
	if replayed[0].level != WARN || replayed[0].msg != "early warning 1" {
		t.Errorf("replayed[0] = %+v", replayed[0])
	}
	if replayed[1].level != ERROR || replayed[1].msg != "early error 2" {
		t.Errorf("replayed[1] = %+v", replayed[1])
	}

	// After hook is set, new warnings should fire directly
	Warnf("test", "live warning")
	if len(replayed) != 3 {
		t.Errorf("total hook calls = %d, want 3", len(replayed))
	}

	// Clean up
	warnMu.Lock()
	warnHook = nil
	warnBuffer = nil
	warnMu.Unlock()
}

func TestMultilineMessageCollapsed(t *testing.T) {
	// Verifies that newlines in log messages are replaced with literal \n
	// so that each log entry remains a single line for reliable parsing.
	buf := captureOutput(t)

	Warnf("mana", "API error (status 429): {\n  \"error\": {\n    \"type\": \"rate_limit_error\"\n  }\n}")

	output := buf.String()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 log line, got %d:\n%s", len(lines), output)
	}
	if !strings.Contains(output, `\n`) {
		t.Errorf("expected literal \\n in output: %s", output)
	}
}

func TestFatalf(t *testing.T) {
	// Verifies that Fatalf logs a message and exits with code 1.
	// Uses the subprocess test pattern since Fatalf calls os.Exit.
	if os.Getenv("TEST_FATALF_SUBPROCESS") == "1" {
		setOutput(os.Stderr)
		Fatalf("test", "fatal error: %s", "boom")
		return // unreachable
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestFatalf$")
	cmd.Env = append(os.Environ(), "TEST_FATALF_SUBPROCESS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		t.Fatal("expected non-zero exit")
	}

	// Verify exit code 1
	if exitErr, ok := err.(*exec.ExitError); ok {
		if exitErr.ExitCode() != 1 {
			t.Errorf("exit code = %d, want 1", exitErr.ExitCode())
		}
	} else {
		t.Fatalf("unexpected error type: %v", err)
	}

	// Verify the error message was logged
	if !strings.Contains(stderr.String(), "fatal error: boom") {
		t.Errorf("stderr missing fatal message: %s", stderr.String())
	}
}

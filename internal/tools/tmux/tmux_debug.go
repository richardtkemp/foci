package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"foci/internal/tempdir"
)

// tmuxDebugLog provides comprehensive debugging for tmux operations.
type tmuxDebugLog struct {
	mu        sync.Mutex
	file      *os.File
	startTime time.Time
}

var debugLog *tmuxDebugLog
var debugOnce sync.Once

// initDebugLog initializes the debug logger on first use.
func initDebugLog() error { // nolint:unparam
	var err error
	debugOnce.Do(func() {
		// Under the foci temp root, never cwd-relative: under `go test` the cwd
		// is the package dir, so a relative path left a 0600 file owned by the
		// test's uid inside the worktree, blocking other uids from removing it
		// (#1094). tempdir.Dir() is also per-run hermetic under make's
		// FOCI_TMPDIR.
		debugPath := filepath.Join(tempdir.Dir(), "tmux.debug")
		f, err := os.OpenFile(debugPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			tmux_debugLog.Warnf("failed to open debug log: %v", err)
			return
		}

		debugLog = &tmuxDebugLog{
			file:      f,
			startTime: time.Now(),
		}
	})
	return err
}

// logDebug writes a debug message to the tmux debug log.
func logDebug(operation, details string) {
	if err := initDebugLog(); err != nil {
		return
	}
	if debugLog == nil || debugLog.file == nil {
		return
	}

	debugLog.mu.Lock()
	defer debugLog.mu.Unlock()

	elapsed := time.Since(debugLog.startTime).Seconds()
	timestamp := fmt.Sprintf("%.3f", elapsed)
	msg := fmt.Sprintf("[%s] %s: %s\n", timestamp, operation, details)

	if _, err := debugLog.file.WriteString(msg); err != nil {
		tmux_debugLog.Warnf("failed to write debug log: %v", err)
	}
}

// logDebugf writes a formatted debug message.
// operation parameter is kept for future extensibility, currently only "send" is used.
func logDebugf(_ /* operation */, format string, args ...interface{}) {
	logDebug("send", fmt.Sprintf(format, args...))
}

// LogSendEntry logs entry to send function.
func LogSendEntry(name string, keysLen int, enter bool) {
	logDebugf("send", "ENTRY name=%s keys_len=%d enter=%v", name, keysLen, enter)
}

// LogSendRateLimiting logs rate-limiting sleep.
func LogSendRateLimiting(gap, wait time.Duration) {
	logDebugf("send", "rate-limiting: gap=%v, sleeping=%v", gap, wait)
}

// LogSendSendKeys logs the send-keys operation.
func LogSendSendKeys(keysLen int) {
	logDebugf("send", "send-keys: %d bytes", keysLen)
}

// LogSendSendEnter logs the send Enter operation.
func LogSendSendEnter() {
	logDebugf("send", "send-keys Enter (after 200ms pause)")
}

// LogSendExit logs exit from send function.
func LogSendExit(success bool, errMsg string) {
	if success {
		logDebugf("send", "EXIT: success")
	} else {
		logDebugf("send", "EXIT: failed (%s)", errMsg)
	}
}

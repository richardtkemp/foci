package tools

import (
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"foci/log"
)

// tmuxDebugLog provides comprehensive debugging for tmux operations.
type tmuxDebugLog struct {
	mu       sync.Mutex
	file     *os.File
	startTime time.Time
}

var debugLog *tmuxDebugLog
var debugOnce sync.Once

// initDebugLog initializes the debug logger on first use.
func initDebugLog() error {
	var err error
	debugOnce.Do(func() {
		logsDir := "logs"
		if _, err := os.Stat(logsDir); os.IsNotExist(err) {
			if err := os.MkdirAll(logsDir, 0755); err != nil {
				log.Warnf("tmux_debug", "failed to create logs directory: %v", err)
				return
			}
		}

		debugPath := filepath.Join(logsDir, "tmux.debug")
		f, err := os.OpenFile(debugPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Warnf("tmux_debug", "failed to open debug log: %v", err)
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
		log.Warnf("tmux_debug", "failed to write debug log: %v", err)
	}
}

// logDebugf writes a formatted debug message.
func logDebugf(operation, format string, args ...interface{}) {
	logDebug(operation, fmt.Sprintf(format, args...))
}

// formatContentPreview returns a preview of pane content (first 100 + last 100 chars).
func formatContentPreview(content string) string {
	if len(content) <= 200 {
		return fmt.Sprintf("%q", escapeSpecial(content))
	}
	first := content[:100]
	last := content[len(content)-100:]
	return fmt.Sprintf("(%d bytes) %q ... %q", len(content), escapeSpecial(first), escapeSpecial(last))
}

// escapeSpecial escapes newlines and control characters for display.
func escapeSpecial(s string) string {
	return strings.NewReplacer(
		"\n", "\\n",
		"\r", "\\r",
		"\t", "\\t",
	).Replace(s)
}

// hashContent returns the MD5 hash of content as a hex string.
func hashContent(content string) string {
	h := md5.Sum([]byte(content))
	return fmt.Sprintf("%x", h[:8]) // first 8 chars for readability
}

// LogExecuteKeysSequenceEntry logs entry to executeKeysSequence.
func LogExecuteKeysSequenceEntry(name, command, keys string) {
	logDebugf("executeKeysSequence", "ENTRY name=%s command_len=%d keys_len=%d", name, len(command), len(keys))
}

// LogExecuteKeysSendCommand logs the send-keys command operation.
func LogExecuteKeysSendCommand(commandLen int) {
	logDebugf("executeKeysSequence", "send-keys command: %d bytes", commandLen)
}

// LogExecuteKeysBaseline logs the baseline capture.
func LogExecuteKeysBaseline(baselineLen int, normalized string) {
	hash := hashContent(normalized)
	logDebugf("executeKeysSequence", "baseline: %d bytes (normalized), hash=%s, preview=%s", len(normalized), hash, formatContentPreview(normalized))
}

// LogExecuteKeysSendEnter logs the send-keys Enter operation.
func LogExecuteKeysSendEnter() {
	logDebugf("executeKeysSequence", "send-keys Enter")
}

// LogExecuteKeysPollStarting logs the start of polling.
func LogExecuteKeysPollStarting() {
	logDebugf("executeKeysSequence", "POLL START: waiting for command output to appear")
}

// LogExecuteKeysPollTick logs each poll attempt during start wait.
func LogExecuteKeysPollTick(pollNum int, elapsed float64, hash, normalized string, changed bool) {
	status := "changed"
	if !changed {
		status = "unchanged"
	}
	logDebugf("executeKeysSequence", "poll #%d: elapsed=%.1fs, hash=%s, %s, preview=%s",
		pollNum, elapsed, hash, status, formatContentPreview(normalized))
}

// LogExecuteKeysTransitionToStable logs transition to wait-for-stable phase.
func LogExecuteKeysTransitionToStable(pollCount int) {
	logDebugf("executeKeysSequence", "TRANSITION: output started appearing after %d polls, moving to stability wait", pollCount)
}

// LogExecuteKeysPollStable logs each poll during the stability wait.
func LogExecuteKeysPollStable(pollNum, stableCount, requiredCount int, elapsed float64, hash, normalized string, changed bool) {
	status := "changed"
	if !changed {
		status = "unchanged"
	}
	logDebugf("executeKeysSequence", "stable-poll #%d: elapsed=%.1fs, hash=%s, %s (stable %d/%d), preview=%s",
		pollNum, elapsed, hash, status, stableCount, requiredCount, formatContentPreview(normalized))
}

// LogExecuteKeysSendKeys logs the send-keys operation.
func LogExecuteKeysSendKeys(keysLen int) {
	logDebugf("executeKeysSequence", "send-keys keys: %d bytes", keysLen)
}

// LogExecuteKeysSendFinalEnter logs the final send-keys Enter.
func LogExecuteKeysSendFinalEnter() {
	logDebugf("executeKeysSequence", "send-keys final Enter")
}

// LogExecuteKeysSequenceExit logs successful exit from executeKeysSequence.
func LogExecuteKeysSequenceExit(success bool, errMsg string) {
	if success {
		logDebugf("executeKeysSequence", "EXIT: success")
	} else {
		logDebugf("executeKeysSequence", "EXIT: failed (%s)", errMsg)
	}
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

// CloseDebugLog closes the debug log file.
func CloseDebugLog() error {
	if debugLog == nil || debugLog.file == nil {
		return nil
	}
	debugLog.mu.Lock()
	defer debugLog.mu.Unlock()
	return debugLog.file.Close()
}

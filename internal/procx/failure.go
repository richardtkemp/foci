package procx

import "strings"

// maxDetailBytes caps each captured stream in a failure message: enough for a
// CLI refusal line, not enough for a truncated transcript to flood the log.
const maxDetailBytes = 400

// FailureDetail renders what a failed child process said, from BOTH streams.
//
// A CLI that reports its own refusals on stdout — `claude --print` does this
// for "usage limit reached", expired auth and bad flags — leaves stderr empty
// when it exits non-zero, so a stderr-only error message degrades to a bare
// "exit status 1" carrying no reason at all. Observed 2026-09-19: three silent
// nudge-extraction failures during an account-wide five-hour quota window,
// diagnosable only by correlating timestamps against another agent's
// rate-limit event.
//
// Returns "" when both streams are blank, so callers can fall back to the
// plain exit-status error.
func FailureDetail(stderrStr, stdoutStr string) string {
	var parts []string
	if s := truncateDetail(stderrStr); s != "" {
		parts = append(parts, "stderr: "+s)
	}
	if s := truncateDetail(stdoutStr); s != "" {
		parts = append(parts, "stdout: "+s)
	}
	return strings.Join(parts, "; ")
}

func truncateDetail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxDetailBytes {
		return s
	}
	return s[:maxDetailBytes] + "… (truncated)"
}

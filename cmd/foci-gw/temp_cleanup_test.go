package main

import (
	"errors"
	"testing"

	"foci/internal/log"
	"foci/internal/tempdir"
)

// TestLogTempCleanup_SkipIsOneLineWithReason proves requirement 8 of #2482:
// a skipped wipe logs exactly one "skipped" line carrying the reason (Info
// when another gateway lives, Warn when the lock could not be checked), and
// never the "reclaimed" line.
func TestLogTempCleanup_SkipIsOneLineWithReason(t *testing.T) {
	cases := []struct {
		name  string
		r     tempdir.CleanStaleResult
		level log.Level
	}{
		{"other gateway", tempdir.CleanStaleResult{SkipReason: runToken("tmpclean") + " another gateway"}, log.INFO},
		{"lock failure", tempdir.CleanStaleResult{SkipReason: runToken("tmpclean") + " cannot open", LockErr: errors.New("EACCES")}, log.WARN},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := captureLog(t, log.DEBUG)

			logTempCleanup(tc.r)

			lines := c.Lines("[startup]", tc.r.SkipReason)
			if len(lines) != 1 {
				t.Fatalf("want one line with the reason, got:\n%s", c)
			}
			if lines[0].Level != tc.level || lines[0].Msg != "temp cleanup skipped: "+tc.r.SkipReason {
				t.Errorf("got %s", lines[0])
			}
			if got := c.Lines("[startup]", "temp cleanup: reclaimed"); len(got) != 0 {
				t.Errorf("a skipped wipe logged a reclaimed line: %v", got)
			}
		})
	}
}

// TestLogTempCleanup_WipeKeepsReclaimedLine is a characterisation of the
// unchanged "reclaimed" line when the wipe runs.
func TestLogTempCleanup_WipeKeepsReclaimedLine(t *testing.T) {
	c := captureLog(t, log.DEBUG)

	logTempCleanup(tempdir.CleanStaleResult{Removed: 6, Bytes: 2048, Failed: 0})

	if got := c.Lines("INFO", "[startup]", "temp cleanup: reclaimed 6 entries (2.0 KB), 0 failed"); len(got) == 0 {
		t.Errorf("reclaimed line missing:\n%s", c)
	}
	if got := c.Lines("[startup]", "skipped"); len(got) != 0 {
		t.Errorf("a wipe that ran logged a skip: %v", got)
	}
}

// TestLogTempCleanup_DowngradeFailureWarns proves a lock error after a wipe
// that ran (the post-wipe downgrade failed) is reported, not dropped.
func TestLogTempCleanup_DowngradeFailureWarns(t *testing.T) {
	c := captureLog(t, log.DEBUG)
	tok := runToken("tmpclean")

	logTempCleanup(tempdir.CleanStaleResult{LockErr: errors.New(tok)})

	if got := c.Lines("WARN", "[startup]", "temp cleanup:", tok); len(got) != 1 {
		t.Errorf("want one warn naming the lock error, got:\n%s", c)
	}
}

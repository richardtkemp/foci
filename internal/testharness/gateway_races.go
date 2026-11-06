package testharness

import (
	"os"
	"strings"
	"testing"
)

// A -race foci-gw is a separate process, so go test's own race check never sees
// what its detector reports: the report goes to the gateway's stderr, which the
// harness captures and prints only for a test that already failed, and the
// exit status 66 comes after the harness has stopped looking (#2909).

const (
	raceReportStart = "WARNING: DATA RACE"
	raceReportRule  = "=================="
)

// gatewayRaceReports returns each data race report in a captured foci-gw stderr,
// as the lines the race detector wrote, without the rules around them. A report
// cut off at the end of the capture is returned as far as it got.
func gatewayRaceReports(stderr string) []string {
	var reports []string
	var cur []string
	inReport := false
	flush := func() {
		if len(cur) > 0 {
			reports = append(reports, strings.Join(cur, "\n"))
		}
		cur = nil
	}
	for _, line := range strings.Split(stderr, "\n") {
		switch {
		case strings.HasPrefix(line, raceReportStart):
			flush()
			inReport = true
			cur = append(cur, line)
		case inReport && strings.HasPrefix(line, raceReportRule):
			flush()
			inReport = false
		case inReport:
			cur = append(cur, line)
		}
	}
	flush()
	return reports
}

// GatewayRaces returns the data race reports in the running gateway's stderr so
// far, as the harness's own race check reads them.
func (h *Harness) GatewayRaces() []string {
	return gatewayRaceReports(h.stderrBuf.String())
}

// failOnGatewayRaces fails t when the gateway's stderr holds race reports. Each
// report goes to stderr unindented, in the detector's own layout, because
// shared/scripts/foci-race-nightly.py reads the two code sites from it, and
// t.Log would indent it. The failure message holds the same words as go test's
// own race failure, which is what the nightly job reads to count this test as
// covered by the race's finding and not as a second finding.
func failOnGatewayRaces(t *testing.T, stderr string) {
	t.Helper()
	reports := gatewayRaceReports(stderr)
	if len(reports) == 0 {
		return
	}
	var b strings.Builder
	for _, r := range reports {
		b.WriteString(raceReportRule + "\n" + r + "\n" + raceReportRule + "\n")
	}
	_, _ = os.Stderr.WriteString(b.String())
	t.Errorf("foci-gw: race detected during execution of test: %d report(s), printed above", len(reports))
}

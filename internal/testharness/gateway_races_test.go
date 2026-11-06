package testharness

import "testing"

const sampleRaceReport = `WARNING: DATA RACE
Write at 0x00c000012345 by goroutine 7:
  foci/internal/foo.(*Bar).set()
      /tmp/x/foo.go:10 +0x44

Previous read at 0x00c000012345 by goroutine 8:
  foci/internal/foo.(*Bar).get()
      /tmp/x/foo.go:20 +0x3c

Goroutine 7 (running) created at:
  foci/internal/foo.Start()
      /tmp/x/foo.go:30 +0x1a`

func TestGatewayRaceReports(t *testing.T) {
	rule := "=================="
	for _, tc := range []struct {
		name   string
		stderr string
		want   []string
	}{
		{"none", "INFO startup complete\nINFO bye\n", nil},
		{"a rule alone is not a report", rule + "\n", nil},
		{"one, among log lines", "INFO a\n" + rule + "\n" + sampleRaceReport + "\n" + rule + "\nINFO b\n", []string{sampleRaceReport}},
		{"two", rule + "\n" + sampleRaceReport + "\n" + rule + "\n" + rule + "\n" + sampleRaceReport + "\n" + rule + "\n", []string{sampleRaceReport, sampleRaceReport}},
		{"cut off when the gateway was killed", rule + "\n" + sampleRaceReport, []string{sampleRaceReport}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := gatewayRaceReports(tc.stderr)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d reports, want %d: %q", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("report %d:\n%s\nwant:\n%s", i, got[i], tc.want[i])
				}
			}
		})
	}
}

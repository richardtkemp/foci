//go:build race

package main

import "os"

// raceCanaryValue is the data race that proves the detector runs in this binary.
var raceCanaryValue int

// This file exists only in a -race build, so a production gateway has none of
// it. With FOCI_TESTHARNESS_RACE_CANARY set, the gateway commits one real data
// race at startup. TestL2_RaceCanary reads the report on stderr: that report is
// the proof a green nightly -race run needs that the gateway under test really
// had the detector on (#2909).
func init() {
	if os.Getenv("FOCI_TESTHARNESS_RACE_CANARY") == "" {
		return
	}
	done := make(chan struct{})
	go func() {
		raceCanaryValue = 1
		close(done)
	}()
	raceCanaryValue = 2
	<-done
}

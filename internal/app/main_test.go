package app

import (
	"os"
	"testing"
)

// TestMain turns the activity rate limit (#1064, transient.go) off for the
// package: most tests assert each activity transition a socket receives. The
// rate-limit tests turn it back on (withActivityRateLimit).
func TestMain(m *testing.M) {
	activityInterval = 0
	os.Exit(m.Run())
}

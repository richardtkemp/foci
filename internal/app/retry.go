package app

import (
	"fmt"
	"strconv"
	"time"
)

// retryDetail is the "retrying" activity's text for one Claude Code api_retry
// (#2260), built once per event (Dick: no countdown): the cause, the attempt
// as CC gives it, and the wait until the next try. max_retries is left out:
// under CC's retry watchdog the attempt count passes it.
func retryDetail(status, attempt int, delay time.Duration) string {
	var cause string
	switch status {
	case 0:
		cause = "Connection error"
	case 529:
		cause = "API overloaded (529)"
	case 429:
		cause = "Rate limited (429)"
	default:
		cause = "API error " + strconv.Itoa(status)
	}
	next := "retrying now"
	if delay > 0 {
		next = "next try in " + shortWait(delay)
	}
	return fmt.Sprintf("%s, retry %d, %s", cause, attempt, next)
}

// shortWait renders a positive wait in one short human unit: seconds under a
// minute (rounded up, so it never shows 0s), minutes under an hour, else hours
// and minutes.
func shortWait(d time.Duration) string {
	if d < time.Minute {
		return strconv.FormatInt(int64((d+time.Second-1)/time.Second), 10) + "s"
	}
	m := int64(d.Round(time.Minute) / time.Minute)
	if m < 60 {
		return strconv.FormatInt(m, 10) + "m"
	}
	if m%60 == 0 {
		return strconv.FormatInt(m/60, 10) + "h"
	}
	return fmt.Sprintf("%dh %dm", m/60, m%60)
}

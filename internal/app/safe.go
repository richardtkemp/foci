package app

import (
	"runtime/debug"

)

// Panic isolation for the app provider.
//
// The app provider is a large, comparatively new subsystem that runs in the
// same process as the telegram/discord providers. A panic in an app goroutine
// would otherwise kill the whole foci gateway (in Go an unrecovered panic in
// any goroutine terminates the process — recover() only catches panics in its
// own goroutine). These helpers wrap every app goroutine and HTTP handler so a
// bug here degrades the app feature without taking the gateway down.
//
// This mirrors the inline recover pattern used elsewhere in the codebase
// (telegram/bot_poll.go, discord/gateway.go, cmd/foci-gw/agents_notify.go),
// consolidated into one helper because the app package has several call sites.

// recoverApp recovers a panic and logs it with a stack trace. Use it as the
// first deferred call at a goroutine or handler entry point:
//
//	defer recoverApp("blob-reaper")
//
// where names the site so the log line is actionable.
func recoverApp(where string) {
	if r := recover(); r != nil {
		appLog.Errorf("recovered panic in %s: %v\n%s", where, r, debug.Stack())
	}
}

// safeGo runs fn in a new goroutine guarded by recoverApp, so a panic in fn is
// logged rather than crashing the process. where names the goroutine for logs.
func safeGo(where string, fn func()) {
	go func() {
		defer recoverApp(where)
		fn()
	}()
}

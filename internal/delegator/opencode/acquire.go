package opencode

// acquireServerFn is a package-level seam so tests can exercise the "no pooled
// server → acquire" branch without spawning a real opencode subprocess (stub it
// to pool a fake Server instead). Used by OpenCleanupScope (branch.go), which
// runs on a freshly constructed, unstarted Backend and must spawn the shared
// server if the agent is idle. Defaults to acquireServer itself — the exact
// function Backend.Start uses, so a cleanup-triggered spawn is
// indistinguishable from an interactive one: same pool, same key, same
// config-building path.
var acquireServerFn = acquireServer

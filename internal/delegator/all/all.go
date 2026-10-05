// Package all registers every delegated backend. Import it (blank) wherever
// the full set of backends must exist — the gateway, the CLI, and the tests
// that check every backend's Spec (#2154) — so "the set of backends" has one
// import site.
package all

import (
	_ "foci/internal/delegator/ccstream" // registers claude-code (stream-json)
	_ "foci/internal/delegator/codex"    // registers codex (app-server JSON-RPC)
	_ "foci/internal/delegator/opencode" // registers opencode (HTTP/SSE)
)

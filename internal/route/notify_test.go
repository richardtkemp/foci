package route

import (
	"strings"
	"testing"

	"foci/internal/log"
	"foci/internal/session"
)

// TestNotifySessionChat_NoConnectionIsLogged (#2769, audit C7): a notice with
// nowhere to go must not look delivered. Not parallel: the warn hook is global.
func TestNotifySessionChat_NoConnectionIsLogged(t *testing.T) {
	var logged []string
	log.SetWarnHook(func(_ log.Level, _, msg string) { logged = append(logged, msg) })
	t.Cleanup(func() { log.SetWarnHook(nil) })

	NotifySessionChat(&fakeConnMgr{}, "a", "a/c1", "you are queued")
	got := strings.Join(logged, "\n")
	if !strings.Contains(got, "you are queued") || !strings.Contains(got, "not delivered") {
		t.Errorf("warnings logged = %q, want the notice marked not delivered", got)
	}

	// A cross-agent spawn has no chat by design: nothing to warn about.
	logged = nil
	NotifySessionChat(&fakeConnMgr{}, "a", session.CrossAgentSpawnRoot("a"), "spawn notice")
	if len(logged) != 0 {
		t.Errorf("cross-agent spawn session logged warnings %q, want none", logged)
	}
}

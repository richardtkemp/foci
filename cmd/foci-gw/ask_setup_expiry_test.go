package main

import (
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/question"
)

// buttonStubConn is a stubConn that can post interactive buttons, so a sequential
// ask prompt lands in the platform's interactive store.
type buttonStubConn struct{ stubConn }

func (c *buttonStubConn) SendTextWithButtons(string, []platform.ButtonChoice, string) (string, error) {
	return "m-1", nil
}
func (c *buttonStubConn) EditMessageText(string, string) error { return nil }
func (c *buttonStubConn) EditMessageWithButtons(string, string, []platform.ButtonChoice, string) error {
	return nil
}

// oneConnMgr routes every session to one connection.
type oneConnMgr struct {
	stubConnMgr
	conn platform.Connection
}

func (m oneConnMgr) ForSessionOrPrimary(string, string) platform.Connection { return m.conn }

// TestAskPrompt_ExpiryResolvesAsExpired (#2091): when the platform's expiry sweep
// retires a sequential ask prompt — freshly presented or re-attached after a
// restart — the ask layer is fed question.ExpiredData, not the Cancel sentinel,
// so the agent is told the ask expired rather than that the user cancelled it.
// Not parallel: CleanupExpiredInteractive sweeps the process-global prompt store.
func TestAskPrompt_ExpiryResolvesAsExpired(t *testing.T) {
	mgr := oneConnMgr{conn: &buttonStubConn{stubConn{sessionKey: "a/s"}}}
	choices := []question.Choice{{Label: "Yes", Data: "qa:0"}, {Label: "Cancel", Data: question.CancelData}}

	var presented, restored []string
	newAskPresentFn("a", mgr)("a/s", "ask-2091-present-q0", "Q?", "Q", choices, func(d string) { presented = append(presented, d) })
	newAskRestoreFn("a", mgr)("a/s", "ask-2091-restore-q0", "m-2", choices, func(d string) { restored = append(restored, d) })

	platform.CleanupExpiredInteractive(-time.Hour) // every entry is older than a cutoff in the future

	for name, got := range map[string][]string{"presented": presented, "restored": restored} {
		if len(got) != 1 || got[0] != question.ExpiredData {
			t.Errorf("%s prompt: expiry fed the ask layer %q, want [%s]", name, got, question.ExpiredData)
		}
	}
}

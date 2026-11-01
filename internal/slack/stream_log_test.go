package slack

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"foci/internal/log"
)

// TestStreamSink_RefusedPostAndEditLogAtWarn proves a streamed post or edit
// that Slack refuses is visible at the default log level (#2459 A1 review
// F4): the announcement says a message Slack refuses "is logged and lost",
// so each refusal logs a WARN naming the channel and the session, as
// SendChunk's ERROR does for the terminal delivery.
func TestStreamSink_RefusedPostAndEditLogAtWarn(t *testing.T) {
	var refusePost atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "chat.postMessage") && !refusePost.Load():
			_, _ = w.Write([]byte(`{"ok":true,"channel":"D1","ts":"1700000000.000001"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":false,"error":"msg_too_long"}`))
		}
	}))
	t.Cleanup(srv.Close)
	core := &agentCore{
		agentID: "helen",
		api:     newWebAPI(srv.URL, "xoxb-x", srv.Client()),
		logger:  func() *log.ComponentLogger { return log.NewComponentLogger("slack-stream-test") },
	}
	target := Target{Kind: TargetDM, Channel: "D1", User: "U1"}
	sessionKey := sessionKeyFor("helen", target)
	warns := captureLogLines(t, log.WARN)
	countFor := func(op string) int {
		n := 0
		for _, l := range warns() {
			if strings.Contains(l, op) && strings.Contains(l, "msg_too_long") && strings.Contains(l, "D1") && strings.Contains(l, sessionKey) {
				n++
			}
		}
		return n
	}

	// Edit refused: the first Update posts, the second edits and is refused.
	edited := &slackStreamSink{core: core, target: target, sessionKey: sessionKey}
	edited.Update("partial")
	if !edited.Close() {
		t.Fatal("first stream post did not land; the edit leg would test nothing")
	}
	edited = &slackStreamSink{core: core, target: target, sessionKey: sessionKey, ref: edited.ref, sent: true}
	edited.Update("partial, longer")
	if n := countFor("stream edit"); n != 1 {
		t.Errorf("WARNs for the refused stream edit = %d, want 1; tapped: %q", n, warns())
	}

	// Post refused.
	refusePost.Store(true)
	posted := &slackStreamSink{core: core, target: target, sessionKey: sessionKey}
	posted.Update("partial")
	if n := countFor("stream post"); n != 1 {
		t.Errorf("WARNs for the refused stream post = %d, want 1; tapped: %q", n, warns())
	}
}

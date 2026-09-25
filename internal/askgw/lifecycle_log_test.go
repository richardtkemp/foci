package askgw

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	flog "foci/internal/log"
	"foci/internal/question"
)

// syncBuf is a goroutine-safe log sink: askgw logs from timer and connection
// goroutines while the test reads.
type syncBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog routes the event log into a buffer at INFO — the production
// default — so a lifecycle line still logged at DEBUG is absent and fails the
// assertion, exactly the #2021 symptom.
func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	b := &syncBuf{}
	flog.SetOutput(b)
	flog.SetLevel(flog.INFO)
	t.Cleanup(func() { flog.SetOutput(os.Stderr); flog.SetLevel(flog.INFO) })
	return b
}

// waitLogLine waits for a line containing every one of parts and returns it.
// Lines are written after the corresponding answer frame, so a reader that
// has just seen the frame may race the log write.
func waitLogLine(t *testing.T, b *syncBuf, parts ...string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, line := range strings.Split(b.String(), "\n") {
			ok := true
			for _, p := range parts {
				if !strings.Contains(line, p) {
					ok = false
					break
				}
			}
			if ok {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no log line containing %q; log:\n%s", parts, b.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func sudoAsk(id string, timeoutSeconds float64) *AskFrame {
	return &AskFrame{
		Protocol:       ProtocolVersion,
		Type:           TypeAsk,
		ID:             id,
		Source:         "aisudo",
		TimeoutSeconds: timeoutSeconds,
		Questions: []AskQuestion{{
			Key:      "sudo",
			Header:   "Sudo",
			Question: "Run   systemctl restart\nnginx?",
			Options:  []AskOption{{Label: "Approve"}, {Label: "Deny"}},
		}},
	}
}

func TestLifecycleLog_SocketAnswered(t *testing.T) {
	logs := captureLog(t)
	cb := &capturedCallback{}
	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, bool) {
		cb.set(onResponse)
		return "plat-42", true
	}
	resolve := func(string) (string, string) { return "agent1", "agent1/chat1" }
	_, sockPath := startTestServer(t, present, resolve)

	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)
	sendFrame(t, conn, sudoAsk("life-1", 60))
	_ = readFrame(t, r) // ack

	uid := "uid=" + strconv.Itoa(os.Getuid())
	arrived := waitLogLine(t, logs, "INFO  [askgw] ask arrived id=life-1")
	for _, want := range []string{"via=socket " + uid, `source="aisudo"`, "timeout=1m0s", `summary="Sudo: Run systemctl restart nginx?"`} {
		if !strings.Contains(arrived, want) {
			t.Errorf("arrival line lacks %q: %s", want, arrived)
		}
	}
	waitLogLine(t, logs, "INFO  [askgw] ask posted id=life-1", "session=agent1/chat1", `platform_msg="plat-42"`)

	cb.fire(question.OptionData(0))
	if ans := readFrame(t, r); ans["status"] != StatusAnswered {
		t.Fatalf("status = %v, want answered", ans["status"])
	}
	waitLogLine(t, logs, "INFO  [askgw] ask answered id=life-1", `answers=[sudo="Approve"]`, " after ")
}

func TestLifecycleLog_SocketTimeoutIsWarn(t *testing.T) {
	logs := captureLog(t)
	present := func(string, string, string, string, string, []question.Choice, func(string)) (string, bool) {
		return "", true
	}
	resolve := func(string) (string, string) { return "agent1", "agent1/chat1" }
	_, sockPath := startTestServer(t, present, resolve)

	conn := dialServer(t, sockPath)
	r := bufio.NewReader(conn)
	sendFrame(t, conn, sudoAsk("life-to", 0.05))
	_ = readFrame(t, r) // ack
	if ans := readFrame(t, r); ans["status"] != StatusTimeout {
		t.Fatalf("status = %v, want timeout", ans["status"])
	}
	waitLogLine(t, logs, "WARN  [askgw] ask timed out id=life-to", "after waiting ", `summary="Sudo: Run systemctl restart nginx?"`)
}

func TestLifecycleLog_HTTPArrivalAnswerTimeout(t *testing.T) {
	logs := captureLog(t)
	cb := &capturedCallback{}
	present := func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (string, bool) {
		cb.set(onResponse)
		return "", true
	}
	resolve := func(string) (string, string) { return "agent1", "agent1/chat1" }
	tr := NewHTTPTransport(newHTTPTestServer(t, present, resolve, 30*time.Millisecond))

	b, _ := Encode(sudoAsk("http-life", 60))
	if _, ok, code, msg := tr.Submit(b); !ok {
		t.Fatalf("submit: %s %s", code, msg)
	}
	waitLogLine(t, logs, "INFO  [askgw] ask arrived id=http-life", "via=http")
	waitLogLine(t, logs, "INFO  [askgw] ask posted id=http-life", "session=agent1/chat1")
	cb.fire(question.OptionData(1))
	if af, _ := tr.Poll("http-life", time.Second); af.Status != StatusAnswered {
		t.Fatalf("status = %q, want answered", af.Status)
	}
	waitLogLine(t, logs, "INFO  [askgw] ask answered id=http-life", `answers=[sudo="Deny"]`)

	// Default timeout (30ms) applies when the frame sets none.
	if _, ok, _, _ := tr.Submit(askBody("http-to")); !ok {
		t.Fatal("submit failed")
	}
	if af, _ := tr.Poll("http-to", time.Second); af.Status != StatusTimeout {
		t.Fatalf("status = %q, want timeout", af.Status)
	}
	waitLogLine(t, logs, "WARN  [askgw] ask timed out id=http-to")
}

func TestLifecycleLog_NotifyUnknownIsInfo(t *testing.T) {
	logs := captureLog(t)
	srv := newHTTPTestServer(t, nil, nil, 0)
	b, _ := Encode(&NotifyFrame{Protocol: ProtocolVersion, Type: TypeNotify, ID: "never-asked"})
	if _, ok, code, msg := srv.HandleNotifyFrame(b); !ok {
		t.Fatalf("notify: %s %s", code, msg)
	}
	waitLogLine(t, logs, "INFO  [askgw] notify for unknown or expired ask id=never-asked")
}

func TestAskSummary(t *testing.T) {
	long := strings.Repeat("x", 300)
	cases := []struct {
		name string
		ask  AskFrame
		want string
	}{
		{"title wins", AskFrame{Title: "Approve deploy", Questions: []AskQuestion{{Header: "H", Question: "Q"}}}, "Approve deploy"},
		{"header and question", AskFrame{Questions: []AskQuestion{{Header: "Sudo", Question: "ls\t -la"}}}, "Sudo: ls -la"},
		{"no header", AskFrame{Questions: []AskQuestion{{Question: "Q?"}}}, "Q?"},
		{"truncated", AskFrame{Title: long}, strings.Repeat("x", askSummaryMax-1) + "…"},
	}
	for _, tc := range cases {
		if got := askSummary(&tc.ask); got != tc.want {
			t.Errorf("%s: askSummary = %q, want %q", tc.name, got, tc.want)
		}
	}
}

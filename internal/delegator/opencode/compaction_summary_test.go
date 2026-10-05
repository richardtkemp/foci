package opencode

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestCompactionSummary_FromMessageList proves the summary is the text of the
// newest /summarize message created since ArmCompactionWait, read from GET
// /session/{id}/message (#1390). The fixture mirrors the live 1.17.15 shape,
// including a USER message whose `summary` is an object, which must not fail
// the whole list.
func TestCompactionSummary_FromMessageList(t *testing.T) {
	var gotPath string
	b := &Backend{sessionID: "ses_1"}
	b.ArmCompactionWait()
	armed := b.compactArmedAt.UnixMilli()
	old := armed - 60_000

	body := fmt.Sprintf(`[
	 {"info":{"id":"m0","role":"assistant","mode":"compaction","summary":true,"time":{"created":%d,"completed":%d}},
	  "parts":[{"type":"text","text":"OLD summary"}]},
	 {"info":{"id":"m1","role":"user","summary":{"diffs":[]},"time":{"created":%d}},
	  "parts":[{"type":"text","text":"user text"}]},
	 {"info":{"id":"m2","role":"assistant","mode":"compaction","summary":true,"time":{"created":%d,"completed":%d}},
	  "parts":[{"type":"step-start"},{"type":"reasoning","text":"thinking"},
	           {"type":"text","text":"## NEW summary"},{"type":"text","text":"ignored","ignored":true},
	           {"type":"text","text":"- item"},{"type":"step-finish"}]},
	 {"info":{"id":"m3","role":"assistant","mode":"build","time":{"created":%d}},
	  "parts":[{"type":"text","text":"normal reply"}]}
	]`, old, old, armed, armed+10, armed+20, armed+30)

	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		_, _ = w.Write([]byte(body))
	}))
	defer hs.Close()
	b.server = &Server{baseURL: hs.URL, http: hs.Client()}

	got, err := b.CompactionSummary(context.Background())
	if err != nil {
		t.Fatalf("CompactionSummary: %v", err)
	}
	if want := "## NEW summary\n- item"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if want := fmt.Sprintf("/session/ses_1/message?limit=%d", compactSummaryScan); gotPath != want {
		t.Errorf("request = %q, want %q", gotPath, want)
	}
}

func TestPickCompactionSummary_NoneSinceArm(t *testing.T) {
	armed := time.Now()
	var m messageWithParts
	m.Info.Role, m.Info.Mode, m.Info.Summary = "assistant", "compaction", true
	m.Info.Time.Created = armed.Add(-time.Hour).UnixMilli()
	m.Parts = []Part{{Type: "text", Text: "stale"}}
	if got := pickCompactionSummary([]messageWithParts{m}, armed); got != "" {
		t.Errorf("got %q, want \"\" (summary predates the arm)", got)
	}
}

func TestCompactionSummary_HTTPError(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer hs.Close()
	b := &Backend{sessionID: "ses_1", server: &Server{baseURL: hs.URL, http: hs.Client()}}
	if got, err := b.CompactionSummary(context.Background()); got != "" || err == nil {
		t.Errorf("got (%q, %v), want (\"\", error)", got, err)
	}
}

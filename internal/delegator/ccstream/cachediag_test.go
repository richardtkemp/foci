package ccstream

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/sqlite"
)

// An assistant line's diagnostics.cache_miss_reason and its requestRef are
// read off the transcript (#3221). The shape is CC 2.1.325's.
func TestParseCCRecord_CacheDiagnostics(t *testing.T) {
	cases := []struct {
		diag string
		want accounting.CacheEvidence
	}{
		{`,"diagnostics":{"cache_miss_reason":{"type":"system_changed","cache_missed_input_tokens":45347}}`,
			accounting.CacheEvidence{MissReason: "system_changed", MissedTokens: 45347}},
		{`,"diagnostics":{"cache_miss_reason":{"type":"unavailable"}}`, accounting.CacheEvidence{MissReason: "unavailable"}},
		{`,"diagnostics":{"cache_miss_reason":null}`, accounting.CacheEvidence{}}, // comparison still running
		{`,"diagnostics":null`, accounting.CacheEvidence{}},
		{``, accounting.CacheEvidence{}},
	}
	for _, c := range cases {
		raw := `{"type":"assistant","requestRef":"r-2","timestamp":"2026-11-11T01:01:19Z","message":{"id":"m","model":"claude-opus-5-5",` +
			`"stop_reason":"tool_use","usage":{"input_tokens":5,"cache_read_input_tokens":39400}` + c.diag + `}}`
		l, _ := parseCCRecord([]byte(raw))
		if l == nil {
			t.Fatalf("%s: no line", c.diag)
		}
		if l.cache != c.want || l.requestRef != "r-2" {
			t.Errorf("%s: cache %+v ref %q, want %+v ref r-2", c.diag, l.cache, l.requestRef, c.want)
		}
	}
}

// apiRequest renders one CC api-request record, as CC 2.1.325 writes it.
func apiRequest(id, base string, keep, tail int, shape, model string, extra string) []byte {
	tails := make([]string, tail)
	for i := range tails {
		tails[i] = fmt.Sprintf("h%d", i)
	}
	tj, _ := json.Marshal(tails)
	b := ""
	if base != "" {
		b = `"base":"` + base + `",`
	}
	return []byte(`{"type":"api-request","id":"` + id + `","querySource":"sdk","shapeHash":"` + shape + `",` +
		`"params":{"model":"` + model + `","max_tokens":128000` + extra + `},` + b +
		fmt.Sprintf(`"keep":%d,"tail":%s}`, keep, tj))
}

// The request digests say what a request changed against its base (#3221):
// nothing when it kept every message the base sent and the same shape,
// model and parameters; each differing part otherwise; and "" when there is
// no base to compare with.
func TestRequestDigests(t *testing.T) {
	d := newCCRequestDigests()
	for _, raw := range [][]byte{
		apiRequest("r1", "", 0, 2, "S", "opus", ""),
		apiRequest("r2", "r1", 2, 3, "S", "opus", ""),         // kept all 2: unchanged
		apiRequest("r3", "r2", 2, 4, "S", "opus", ""),         // base had 5, kept 2
		apiRequest("r4", "r3", 6, 1, "T", "opus", ""),         // shape changed
		apiRequest("r5", "r4", 7, 1, "T", "sonnet", ""),       // model changed
		apiRequest("r6", "r5", 8, 1, "T", "sonnet", `,"x":1`), // params changed
		apiRequest("r7", "gone", 3, 1, "T", "sonnet", ""),     // base not seen
		[]byte(`{"type":"api-request-blob","hash":"h","message":{}}`),
		[]byte(`{"type":"user","message":{"content":"api-request"}}`),
	} {
		d.observe(raw)
	}
	for ref, want := range map[string]string{
		"r1": "", "r2": accounting.RequestUnchanged, "r3": accounting.RequestChangedMessages,
		"r4": accounting.RequestChangedSystemOrTools, "r5": accounting.RequestChangedModel,
		"r6": accounting.RequestChangedParams, "r7": "", "nope": "", "": "",
	} {
		if got := d.change(ref); got != want {
			t.Errorf("change(%q) = %q, want %q", ref, got, want)
		}
	}
}

// The digests keep a bounded window of recent requests.
func TestRequestDigests_Bounded(t *testing.T) {
	d := newCCRequestDigests()
	for i := range ccRequestDigestCap + 10 {
		base := ""
		if i > 0 {
			base = fmt.Sprintf("r%d", i-1)
		}
		d.observe(apiRequest(fmt.Sprintf("r%d", i), base, i, 1, "S", "opus", ""))
	}
	if len(d.reqs) != ccRequestDigestCap || len(d.order) != ccRequestDigestCap {
		t.Fatalf("kept %d/%d requests, want %d", len(d.reqs), len(d.order), ccRequestDigestCap)
	}
	last := fmt.Sprintf("r%d", ccRequestDigestCap+9)
	if got := d.change(last); got != accounting.RequestUnchanged {
		t.Errorf("latest request's change = %q", got)
	}
}

// A booked call keeps its cache evidence in its detail (#3221).
func TestBookCall_KeepsCacheEvidence(t *testing.T) {
	tb := newTestBook(t, nil)
	tb.streamNamed("m1", "", "T1", tb.clock)
	tb.advance(time.Millisecond)
	l := line("m1", opus, tb.clock, "end_turn", 1, 1, 39400, 0, 12800)
	l.cache = accounting.CacheEvidence{MissReason: accounting.CacheMissUnavailable, RequestChange: accounting.RequestUnchanged}
	tb.mainLine(l)
	tb.advance(time.Millisecond)
	db, err := sqlite.OpenReadOnly(tb.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var detail string
	if err := db.QueryRow(`SELECT detail FROM api_calls WHERE call_key = 'm1'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(detail), &d); err != nil {
		t.Fatal(err)
	}
	if d["cache_miss_reason"] != "unavailable" || d["request_change"] != "unchanged" {
		t.Errorf("detail = %s", detail)
	}
}

// The main tail stamps each assistant line with what its request changed,
// from the api-request records before it (#3221).
func TestTailMain_StampsRequestChange(t *testing.T) {
	tb := newTestBook(t, nil)
	path := filepath.Join(t.TempDir(), "main.jsonl")
	asst := func(id, ref string) string {
		return `{"type":"assistant","requestRef":"` + ref + `","timestamp":"2026-11-11T01:01:19Z","message":{"id":"` + id +
			`","model":"claude-opus-5-5","stop_reason":"end_turn","usage":{"input_tokens":1,"cache_read_input_tokens":39400},` +
			`"diagnostics":{"cache_miss_reason":{"type":"unavailable"}}}}`
	}
	body := string(apiRequest("r1", "", 0, 2, "S", "opus", "")) + "\n" + asst("m1", "r1") + "\n" +
		string(apiRequest("r2", "r1", 2, 2, "S", "opus", "")) + "\n" + asst("m2", "r2") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &ccLedger{book: tb.ccBook, events: make(chan ccEvent, 64), tailStop: make(chan struct{}), tailDone: make(chan struct{}), tailPoke: make(chan struct{}, 1)}
	close(s.tailStop) // drain once and return
	s.tailMain(path, 0)
	close(s.events)
	got := map[string]accounting.CacheEvidence{}
	for e := range s.events {
		if e.kind == ccMainLine {
			got[e.line.id] = e.line.cache
		}
	}
	want := map[string]accounting.CacheEvidence{
		"m1": {MissReason: "unavailable"},
		"m2": {MissReason: "unavailable", RequestChange: accounting.RequestUnchanged},
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: cache = %+v, want %+v", id, got[id], w)
		}
	}
}

package ccstream

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
)

// #2260: CC's api_retry (main thread only) is logged at INFO and handed to the
// agent layer; a subagent's retries, carried on Agent tool_progress frames,
// are logged only (Dick 2026-10-23). Fixtures are the CC 2.1.306 wire lines
// captured against a stub API (notes/2260.md).

// captureLogs sends the process log to a buffer and records WARN/ERROR lines.
// Not parallel-safe: the log output and warn hook are process-global.
func captureLogs(t *testing.T) (out *bytes.Buffer, warned func() []string) {
	t.Helper()
	out = &bytes.Buffer{}
	log.SetOutput(out)
	t.Cleanup(func() { log.SetOutput(nil) })
	var mu sync.Mutex
	var w []string
	log.SetWarnHook(func(level log.Level, _ string, msg string) {
		mu.Lock()
		defer mu.Unlock()
		w = append(w, level.String()+" "+msg)
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })
	return out, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), w...)
	}
}

// linesWith is the log lines holding sub.
func linesWith(out *bytes.Buffer, sub string) []string {
	var got []string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.Contains(l, sub) {
			got = append(got, l)
		}
	}
	return got
}

func TestOnSystem_APIRetry_LogsAndHandsOn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       string
		wantLog   []string // substrings of the one INFO line
		wantRetry *delegator.APIRetry
	}{
		{"overloaded",
			`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":15,"retry_delay_ms":558,"error_status":529,"error":"overloaded","uuid":"u1","session_id":"s1"}`,
			[]string{"attempt=1", "max_retries=15", "retry_delay_ms=558", "status=529", "error=overloaded"},
			&delegator.APIRetry{Endpoint: "Claude Code", Attempt: 1, MaxRetries: 15, Delay: 558 * time.Millisecond, Status: 529, Category: "overloaded"}},
		{"null status (dropped connection)",
			`{"type":"system","subtype":"api_retry","attempt":2,"max_retries":15,"retry_delay_ms":4516,"error_status":null,"error":"unknown","uuid":"u2","session_id":"s1"}`,
			[]string{"attempt=2", "status=none", "error=unknown"},
			&delegator.APIRetry{Endpoint: "Claude Code", Attempt: 2, MaxRetries: 15, Delay: 4516 * time.Millisecond, Status: 0, Category: "unknown"}},
		{"first-byte timeout with no_response",
			`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":1,"retry_delay_ms":0,"error_status":null,"error":"unknown","no_response":{"waited_ms":60000,"retry_wait_ms":0},"uuid":"u3","session_id":"s1"}`,
			[]string{"attempt=1", "max_retries=1", "status=none", "no_response_waited_ms=60000", "no_response_retry_wait_ms=0"},
			&delegator.APIRetry{Endpoint: "Claude Code", Attempt: 1, MaxRetries: 1, Status: 0, Category: "unknown"}},
		{"malformed is dropped with a WARN",
			`{"type":"system","subtype":"api_retry","attempt":"three","max_retries":15}`,
			nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, warned := captureLogs(t)
			var got []delegator.APIRetry
			b := &Backend{}
			b.AttachSessionEvents(&delegator.SessionEvents{OnAPIRetry: func(r delegator.APIRetry) { got = append(got, r) }})

			b.OnSystem("api_retry", json.RawMessage(tc.raw))

			if tc.wantRetry == nil {
				if len(got) != 0 {
					t.Errorf("handed on %+v, want nothing", got)
				}
				w := warned()
				if len(w) != 1 || !strings.Contains(w[0], "malformed api_retry") {
					t.Errorf("warnings = %q, want one naming the malformed api_retry", w)
				}
				return
			}
			lines := linesWith(out, "api_retry attempt=")
			if len(lines) != 1 {
				t.Fatalf("api_retry log lines = %q, want one; log:\n%s", lines, out.String())
			}
			if !strings.Contains(lines[0], " INFO ") {
				t.Errorf("logged as %q, want INFO", lines[0])
			}
			for _, want := range tc.wantLog {
				if !strings.Contains(lines[0], want) {
					t.Errorf("log line missing %q: %s", want, lines[0])
				}
			}
			if w := warned(); len(w) != 0 {
				t.Errorf("warnings = %q, want none", w)
			}
			if len(got) != 1 || got[0] != *tc.wantRetry {
				t.Errorf("handed on %+v, want [%+v]", got, *tc.wantRetry)
			}
		})
	}
}

// TestOnSystem_APIRetry_NoSessionEvents: with no OnAPIRetry (another consumer,
// or none attached yet) the event is still logged and nothing panics.
func TestOnSystem_APIRetry_NoSessionEvents(t *testing.T) {
	out, _ := captureLogs(t)
	b := &Backend{}
	b.OnSystem("api_retry", json.RawMessage(`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":15,"retry_delay_ms":2000,"error_status":429,"error":"rate_limit"}`))
	if lines := linesWith(out, "api_retry attempt=1"); len(lines) != 1 {
		t.Fatalf("api_retry log lines = %q, want one", lines)
	}
}

func TestOnToolProgress_SubagentRetryLogged(t *testing.T) {
	out, warned := captureLogs(t)
	var retries []delegator.APIRetry
	b := &Backend{}
	b.AttachSessionEvents(&delegator.SessionEvents{OnAPIRetry: func(r delegator.APIRetry) { retries = append(retries, r) }})

	feed := func(raw string) {
		var msg ToolProgressMessage
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatal(err)
		}
		b.OnToolProgress(&msg)
	}
	// An ordinary Agent heartbeat before any retry: nothing logged.
	feed(`{"type":"tool_progress","tool_use_id":"agent_msg_stub2","tool_name":"Agent","parent_tool_use_id":"toolu_stub1","elapsed_time_seconds":0,"subagent_type":"general-purpose"}`)
	feed(`{"type":"tool_progress","tool_use_id":"agent_msg_stub2","tool_name":"Agent","parent_tool_use_id":"toolu_stub1","elapsed_time_seconds":0,"subagent_type":"general-purpose","subagent_retry":{"agent_id":"a20bc82bbdb257f7a","attempt":1,"max_retries":15,"retry_delay_ms":570,"error_status":529,"error_category":"overloaded"}}`)
	feed(`{"type":"tool_progress","tool_use_id":"agent_msg_stub2","tool_name":"Agent","parent_tool_use_id":"toolu_stub1","elapsed_time_seconds":0,"subagent_type":"general-purpose","subagent_retry":{"agent_id":"a20bc82bbdb257f7a","attempt":2,"max_retries":15,"retry_delay_ms":1134,"error_status":null,"error_category":"unknown"}}`)
	// The recovery frame: no subagent_retry.
	feed(`{"type":"tool_progress","tool_use_id":"agent_msg_stub2","tool_name":"Agent","parent_tool_use_id":"toolu_stub1","elapsed_time_seconds":0,"subagent_type":"general-purpose"}`)
	// A later heartbeat: the streak is over, nothing more.
	feed(`{"type":"tool_progress","tool_use_id":"agent_msg_stub2","tool_name":"Agent","parent_tool_use_id":"toolu_stub1","elapsed_time_seconds":5,"subagent_type":"general-purpose"}`)

	lines := linesWith(out, "subagent api_retry")
	if len(lines) != 3 {
		t.Fatalf("subagent api_retry lines = %q, want 2 retries and 1 recovery; log:\n%s", lines, out.String())
	}
	for i, want := range [][]string{
		{"group=toolu_stub1", "agent_id=a20bc82bbdb257f7a", "attempt=1", "max_retries=15", "retry_delay_ms=570", "status=529", "error_category=overloaded"},
		{"group=toolu_stub1", "attempt=2", "retry_delay_ms=1134", "status=none", "error_category=unknown"},
		{"subagent api_retry recovered group=toolu_stub1"},
	} {
		if !strings.Contains(lines[i], " INFO ") {
			t.Errorf("line %d logged as %q, want INFO", i, lines[i])
		}
		for _, w := range want {
			if !strings.Contains(lines[i], w) {
				t.Errorf("line %d missing %q: %s", i, w, lines[i])
			}
		}
	}
	if len(retries) != 0 {
		t.Errorf("a subagent retry reached the agent layer: %+v (logged only, Dick 2026-10-23)", retries)
	}
	if w := warned(); len(w) != 0 {
		t.Errorf("warnings = %q, want none", w)
	}
}

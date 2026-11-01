package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
	"foci/internal/provider"

	sdk "github.com/anthropics/anthropic-sdk-go"
)

// #2273: the direct Anthropic path records and bills server-tool requests, and
// raises invUnseenField for a usage key it does not know or a non-standard
// rate tier.

// liveLedger installs a scratch ledger as the live one and returns the alarms
// it raises. Not for parallel tests.
func liveLedger(t *testing.T) *[]accounting.Alarm {
	t.Helper()
	var alarms []accounting.Alarm
	l, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{OnAlarm: func(a accounting.Alarm) {
		alarms = append(alarms, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	accounting.SetLive(l)
	t.Cleanup(func() { accounting.SetLive(nil); _ = l.Close() })
	return &alarms
}

func sdkMessage(t *testing.T, id, usage string) *sdk.Message {
	t.Helper()
	var m sdk.Message
	raw := fmt.Sprintf(`{"id":%q,"type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":"end_turn","usage":%s}`, id, usage)
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

// sdkUsage is a usage object with every Usage field of the SDK, plus speed.
const sdkUsage = `{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":300,"cache_creation_input_tokens":40,` +
	`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":40},` +
	`"server_tool_use":{"web_search_requests":3,"web_fetch_requests":2},"service_tier":"standard","inference_geo":"global","speed":"standard"}`

// TestResponseFromSDK_ServerToolsAreBilled: usage.server_tool_use's request
// counts are recorded and priced as web_search / web_fetch, as CC's are.
func TestResponseFromSDK_ServerToolsAreBilled(t *testing.T) {
	resp := responseFromSDK(sdkMessage(t, "msg_st", sdkUsage))
	if resp.Usage.WebSearchRequests != 3 || resp.Usage.WebFetchRequests != 2 {
		t.Fatalf("usage = %+v, want 3 web searches and 2 web fetches", resp.Usage)
	}
	tk := resp.Usage.Tokens()
	if tk[modelinfo.ClassWebSearch] != 3 || tk[modelinfo.ClassWebFetch] != 2 {
		t.Errorf("tokens = %v, want web_search=3 web_fetch=2", tk)
	}
	with, _ := modelinfo.CostAsOf("claude-opus-5", time.Now(), tk)
	without, _ := modelinfo.CostAsOf("claude-opus-5", time.Now(), provider.Usage{InputTokens: 10, OutputTokens: 20,
		CacheReadInputTokens: 300, CacheCreationInputTokens: 40}.Tokens())
	if with <= without {
		t.Errorf("price with 3 web searches $%.6f is not above the price without $%.6f", with, without)
	}
}

// TestStreamMessage_ServerToolsFromMessageDelta: in a stream the final
// server-tool counts come in message_delta's usage, which the SDK's
// accumulator does not copy.
func TestStreamMessage_ServerToolsFromMessageDelta(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`event: message_start
data: {"type":"message_start","message":{"id":"msg_s","type":"message","role":"assistant","content":[],"model":"claude-haiku-4-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0}}}}`,
			`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5,"server_tool_use":{"web_search_requests":2,"web_fetch_requests":1}}}`,
			`event: message_stop
data: {"type":"message_stop"}`,
		} {
			fmt.Fprintf(w, "%s\n\n", ev)
		}
	}))
	defer server.Close()
	client := NewClient(func() (string, error) { return "test-key", nil }, 10*time.Second)
	client.SetBaseURL(server.URL)
	resp, err := client.StreamMessage(context.Background(), &MessageRequest{
		Model: "claude-haiku-4-5", MaxTokens: 256,
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
	}, &provider.StreamHandler{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.WebSearchRequests != 2 || resp.Usage.WebFetchRequests != 1 || resp.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want message_delta's 2 web searches, 1 web fetch, 5 output tokens", resp.Usage)
	}
}

func unseenAlarms(alarms []accounting.Alarm) []accounting.Alarm {
	var out []accounting.Alarm
	for _, a := range alarms {
		if a.Invariant == accounting.InvUnseenField {
			out = append(out, a)
		}
	}
	return out
}

// TestResponseFromSDK_UnseenUsageKey: every SDK Usage field (and speed) is
// known; a key the API adds alarms once, with its path and value, on backend
// api; a fast or priority call alarms too. The response is translated as
// before either way.
func TestResponseFromSDK_UnseenUsageKey(t *testing.T) {
	alarms := liveLedger(t)
	responseFromSDK(sdkMessage(t, "msg_ok", sdkUsage))
	if got := unseenAlarms(*alarms); len(got) != 0 {
		t.Fatalf("invUnseenField on a complete SDK usage: %+v", got)
	}

	withNew := strings.Replace(sdkUsage, `"speed":"standard"`, `"speed":"standard","cache_creation_24h_tokens":9`, 1)
	resp := responseFromSDK(sdkMessage(t, "msg_new1", withNew))
	responseFromSDK(sdkMessage(t, "msg_new2", withNew))
	got := unseenAlarms(*alarms)
	if len(got) != 1 {
		t.Fatalf("invUnseenField raised %d times, want once: %+v", len(got), got)
	}
	if a := got[0]; a.Backend != accounting.BackendAPI || !strings.Contains(a.Detail, "anthropic") ||
		!strings.Contains(a.Detail, "cache_creation_24h_tokens=9") || !strings.Contains(a.Detail, "msg_new1") {
		t.Errorf("alarm = %+v", a)
	}
	if resp.Usage.InputTokens != 10 {
		t.Errorf("usage = %+v: an unseen key must not change the translation", resp.Usage)
	}

	responseFromSDK(sdkMessage(t, "msg_fast", strings.Replace(sdkUsage, `"speed":"standard"`, `"speed":"fast"`, 1)))
	if got := unseenAlarms(*alarms); len(got) != 2 || !strings.Contains(got[1].Detail, `speed="fast"`) {
		t.Errorf("a fast-mode call must raise invUnseenField: %+v", got)
	}
}

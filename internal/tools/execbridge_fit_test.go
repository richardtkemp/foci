package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
)

// TestExecBridgeSpillsOverCapResult is #2818's repro: foci_todo list
// --limit 5000 --format jsonl returned ~2 MB, and foci-call rejected the
// response line ("response too large ... cap 1048576 bytes"). A quote-heavy
// result under 1 MB of text also crosses the cap once JSON escapes it. Every
// response line must fit foci-call's cap, and the full result must reach
// the caller through result_file.
func TestExecBridgeSpillsOverCapResult(t *testing.T) {
	t.Parallel()
	line := `{"id":1,"status":"open","title":"a \"quoted\" title"}` + "\n"
	escapeHeavy := strings.Repeat(line, 900*1024/len(line)) // ~900 KB text, ~1.15 MB escaped
	huge := strings.Repeat(line, 3*1024*1024/len(line))     // ~3 MB text
	small := "short result\n"

	r := NewRegistry()
	for name, text := range map[string]string{"escapeheavy": escapeHeavy, "huge": huge, "small": small} {
		text := text
		r.Register(&Tool{Name: name, ExecExport: true,
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
			Execute: func(context.Context, json.RawMessage) (ToolResult, error) {
				return TextResult(text), nil
			}})
	}
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	for _, c := range []struct {
		tool  string
		want  string
		spill bool
	}{{"escapeheavy", escapeHeavy, true}, {"huge", huge, true}, {"small", small, false}} {
		conn, err := net.Dial("unix", bridge.SockPath())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		fmt.Fprintf(conn, `{"tool":%q,"params":{}}`+"\n", c.tool)
		raw, err := bufio.NewReader(conn).ReadBytes('\n') // unbounded: measure the real line
		_ = conn.Close()
		if err != nil {
			t.Fatalf("%s: read: %v", c.tool, err)
		}
		if n := len(raw) - 1; n > bridgeMaxResponseBytes {
			t.Errorf("%s: response line is %d bytes, over foci-call's cap of %d", c.tool, n, bridgeMaxResponseBytes)
			continue
		}
		var resp bridgeResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("%s: parse: %v", c.tool, err)
		}
		if resp.Error != "" {
			t.Fatalf("%s: error %q", c.tool, resp.Error)
		}
		if !c.spill {
			if resp.ResultFile != "" || resp.Result != c.want {
				t.Errorf("%s: an under-cap result must stay inline; got file=%q result=%q", c.tool, resp.ResultFile, resp.Result)
			}
			continue
		}
		if resp.ResultFile == "" {
			t.Errorf("%s: over-cap result has no result_file", c.tool)
			continue
		}
		got, err := os.ReadFile(resp.ResultFile)
		_ = os.Remove(resp.ResultFile)
		if err != nil {
			t.Fatalf("%s: read result_file: %v", c.tool, err)
		}
		if string(got) != c.want || resp.ResultSize != int64(len(c.want)) {
			t.Errorf("%s: result_file holds %d bytes (size field %d), want %d", c.tool, len(got), resp.ResultSize, len(c.want))
		}
	}
}

func TestTruncateUTF8(t *testing.T) {
	t.Parallel()
	if got := truncateUTF8("aé", 2); got != "a" {
		t.Errorf("truncateUTF8 split a rune: %q", got)
	}
	if got := truncateUTF8("abc", 5); got != "abc" {
		t.Errorf("truncateUTF8 short = %q", got)
	}
}

package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// #2656: a tool called over the exec bridge is cancelled when its caller
// closes the connection before the result comes back (foci-call killed, its
// Bash call stopped), so a synchronous spawn does not work on for nobody.
func TestExecBridgeCancelsCallWhenCallerCloses(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	cancelled := make(chan error, 1)
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "block_tool",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
			close(started)
			select {
			case <-ctx.Done():
				cancelled <- ctx.Err()
				return ToolResult{}, ctx.Err()
			case <-time.After(30 * time.Second): // hang guard only
				cancelled <- nil
				return TextResult("never cancelled"), nil
			}
		},
	})
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	conn, err := net.Dial("unix", bridge.SockPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte(`{"tool":"block_tool","params":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	<-started
	_ = conn.Close() // the caller goes away mid-call

	if err := <-cancelled; err == nil {
		t.Fatal("tool ran to its end: the caller's close did not cancel its ctx")
	}
}

// The other arm: a caller that waits gets its result, and its ctx is not
// cancelled while the tool runs.
func TestExecBridgeWaitingCallerIsNotCancelled(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "slow_tool",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
			select {
			case <-ctx.Done():
				return ToolResult{}, ctx.Err()
			case <-time.After(200 * time.Millisecond):
				return TextResult("done"), nil
			}
		},
	})
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()
	conn, err := net.Dial("unix", bridge.SockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"tool":"slow_tool","params":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp bridgeResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "" || resp.Result != "done" {
		t.Fatalf("response = %+v, want result done", resp)
	}
}

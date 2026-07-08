package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"foci/internal/app/fap"
)

type fakeInvoker struct {
	result fap.ToolResult
	err    error
	calls  int
	last   struct {
		tool, action string
		args         json.RawMessage
	}
	block chan struct{} // optional: hold the call until closed
}

func (f *fakeInvoker) InvokeTool(ctx context.Context, tool, action string, args json.RawMessage) (fap.ToolResult, error) {
	f.calls++
	f.last.tool, f.last.action, f.last.args = tool, action, args
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return fap.ToolResult{}, ctx.Err()
		}
	}
	return f.result, f.err
}

func TestAppAndroidTool_NoInvoker(t *testing.T) {
	tool := NewAppAndroidTool(func() (AppInvoker, bool) { return nil, false })
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"list"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(res.Text, "no Android device connected") {
		t.Errorf("expected 'no Android device' message, got %q", res.Text)
	}
}

func TestAppAndroidTool_ListAction(t *testing.T) {
	fake := &fakeInvoker{result: fap.ToolResult{
		Status: "completed",
		Output: json.RawMessage(`{"tasks":[{"task":"foci_battery","description":"battery level"}]}`),
	}}
	tool := NewAppAndroidTool(func() (AppInvoker, bool) { return fake, true })
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"list"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if fake.calls != 1 {
		t.Errorf("expected 1 invoker call, got %d", fake.calls)
	}
	if fake.last.tool != "android" || fake.last.action != "list" {
		t.Errorf("invoked with tool=%q action=%q, want android/list", fake.last.tool, fake.last.action)
	}
	if !strings.Contains(res.Text, "foci_battery") {
		t.Errorf("expected output to mention foci_battery, got %q", res.Text)
	}
}

func TestAppAndroidTool_PerformAction(t *testing.T) {
	fake := &fakeInvoker{result: fap.ToolResult{
		Status: "completed",
		Output: json.RawMessage(`{"level":82,"charging":true}`),
	}}
	tool := NewAppAndroidTool(func() (AppInvoker, bool) { return fake, true })
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"perform","task":"foci_battery","par1":"mode=fine"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Confirm par1 made it through as JSON-embedded args.
	var args map[string]any
	if err := json.Unmarshal(fake.last.args, &args); err != nil {
		t.Fatalf("invoker args not JSON: %v", err)
	}
	if args["task"] != "foci_battery" {
		t.Errorf("args.task = %v, want foci_battery", args["task"])
	}
	if args["par1"] != "mode=fine" {
		t.Errorf("args.par1 = %v, want mode=fine", args["par1"])
	}
	if !strings.Contains(res.Text, "82") {
		t.Errorf("expected output to mention level 82, got %q", res.Text)
	}
}

func TestAppAndroidTool_PendingResult(t *testing.T) {
	fake := &fakeInvoker{result: fap.ToolResult{Status: "pending", Error: "still running"}}
	tool := NewAppAndroidTool(func() (AppInvoker, bool) { return fake, true })
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"perform","task":"slow"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(res.Text, "still running") || !strings.Contains(res.Text, "won't be delivered") {
		t.Errorf("expected pending + not-delivered message, got %q", res.Text)
	}
}

func TestAppAndroidTool_ErrorResult(t *testing.T) {
	fake := &fakeInvoker{result: fap.ToolResult{Status: "error", Error: "task not found"}}
	tool := NewAppAndroidTool(func() (AppInvoker, bool) { return fake, true })
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"perform","task":"ghost"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(res.Text, "task not found") {
		t.Errorf("expected error text, got %q", res.Text)
	}
}

func TestAppAndroidTool_InvokeError(t *testing.T) {
	fake := &fakeInvoker{err: errors.New("no live device")}
	tool := NewAppAndroidTool(func() (AppInvoker, bool) { return fake, true })
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"list"}`))
	if err != nil {
		t.Fatalf("err: %v", err) // InvokeTool errors are surfaced as tool result text, not Go error
	}
	if !strings.Contains(res.Text, "no live device") {
		t.Errorf("expected error in result text, got %q", res.Text)
	}
}

func TestAppAndroidTool_CtxCancelPropagates(t *testing.T) {
	fake := &fakeInvoker{block: make(chan struct{})}
	tool := NewAppAndroidTool(func() (AppInvoker, bool) { return fake, true })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := tool.Execute(ctx, json.RawMessage(`{"action":"list"}`))
	// Ctx cancel surfaces as an error result (invoker returns ctx.Err) which
	// the tool wraps into the result text.
	if err != nil {
		t.Logf("Execute returned Go err (acceptable): %v", err)
	}
}

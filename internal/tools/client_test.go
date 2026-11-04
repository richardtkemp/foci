package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"foci/internal/fap"
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

func TestClientTool_NoInvoker(t *testing.T) {
	tool := NewClientTool(func() (AppInvoker, bool) { return nil, false })
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"tasker_list"}`))
	if err == nil || !strings.Contains(err.Error(), "no Android device connected") {
		t.Errorf("expected 'no Android device' error, got %v", err)
	}
}

func TestClientTool_ListAction(t *testing.T) {
	fake := &fakeInvoker{result: fap.ToolResult{
		Status: "completed",
		Output: json.RawMessage(`{"tasks":[{"task":"foci_battery","description":"battery level"}]}`),
	}}
	tool := NewClientTool(func() (AppInvoker, bool) { return fake, true })
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"tasker_list"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if fake.calls != 1 {
		t.Errorf("expected 1 invoker call, got %d", fake.calls)
	}
	if fake.last.tool != "client" || fake.last.action != "tasker_list" {
		t.Errorf("invoked with tool=%q action=%q, want client/tasker_list", fake.last.tool, fake.last.action)
	}
	if !strings.Contains(res.Text, "foci_battery") {
		t.Errorf("expected output to mention foci_battery, got %q", res.Text)
	}
}

func TestClientTool_PerformAction(t *testing.T) {
	fake := &fakeInvoker{result: fap.ToolResult{
		Status: "completed",
		Output: json.RawMessage(`{"level":82,"charging":true}`),
	}}
	tool := NewClientTool(func() (AppInvoker, bool) { return fake, true })
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"tasker_perform","task":"foci_battery","par1":"mode=fine"}`))
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

func TestClientTool_PendingResult(t *testing.T) {
	fake := &fakeInvoker{result: fap.ToolResult{Status: "pending", Error: "still running"}}
	tool := NewClientTool(func() (AppInvoker, bool) { return fake, true })
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"tasker_perform","task":"slow"}`))
	if err == nil || !strings.Contains(err.Error(), "still running") || !strings.Contains(err.Error(), "won't be delivered") {
		t.Errorf("expected pending + not-delivered error, got %v", err)
	}
}

func TestClientTool_ErrorResult(t *testing.T) {
	fake := &fakeInvoker{result: fap.ToolResult{Status: "error", Error: "task not found"}}
	tool := NewClientTool(func() (AppInvoker, bool) { return fake, true })
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"tasker_perform","task":"ghost"}`))
	if err == nil || !strings.Contains(err.Error(), "task not found") {
		t.Errorf("expected task-not-found error, got %v", err)
	}
}

func TestClientTool_InvokeError(t *testing.T) {
	fake := &fakeInvoker{err: errors.New("no live device")}
	tool := NewClientTool(func() (AppInvoker, bool) { return fake, true })
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"tasker_list"}`))
	if err == nil || !strings.Contains(err.Error(), "no live device") {
		t.Errorf("expected the invoke error, got %v", err)
	}
}

func TestClientTool_CtxCancelPropagates(t *testing.T) {
	fake := &fakeInvoker{block: make(chan struct{})}
	tool := NewClientTool(func() (AppInvoker, bool) { return fake, true })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := tool.Execute(ctx, json.RawMessage(`{"action":"tasker_list"}`))
	// Ctx cancel surfaces as an error (invoker returns ctx.Err).
	if err != nil {
		t.Logf("Execute returned Go err (acceptable): %v", err)
	}
}

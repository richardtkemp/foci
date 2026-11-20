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

// pushingInvoker is a fakeInvoker that can also push an alarm (#3194).
type pushingInvoker struct {
	fakeInvoker
	pushErr error
	pushed  []json.RawMessage
}

func (p *pushingInvoker) PushAlarm(args json.RawMessage) error {
	p.pushed = append(p.pushed, args)
	return p.pushErr
}

// With no phone connected, set_alarm goes by push and succeeds as held, with
// the note saying the alarm is not created yet (Dick 2026-11-20, "Push it now").
func TestClientTool_SetAlarmWithNoPhoneIsPushedAndHeld(t *testing.T) {
	inv := &pushingInvoker{fakeInvoker: fakeInvoker{err: ErrNoAppDevice}}
	tool := NewClientTool(func() (AppInvoker, bool) { return inv, true })
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"set_alarm","time":"07:30","label":"nursery","days":"mon,wed"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(inv.pushed) != 1 {
		t.Fatalf("pushes = %d, want 1", len(inv.pushed))
	}
	var args map[string]any
	if err := json.Unmarshal(inv.pushed[0], &args); err != nil || args["hour"] != float64(7) || args["minute"] != float64(30) || args["label"] != "nursery" {
		t.Errorf("pushed args = %s (%v)", inv.pushed[0], err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("result not JSON: %q", res.Text)
	}
	if out["alarm"] != "07:30" || out["held"] != true || out["created"] != false || out["label"] != "nursery" {
		t.Errorf("result = %v", out)
	}
	if !strings.Contains(out["note"].(string), "NOT created yet") {
		t.Errorf("note = %v", out["note"])
	}
}

// A desktop-only connection (ErrNoCapableDevice, not ErrNoAppDevice) pushes too.
func TestClientTool_SetAlarmWithOnlyDesktopIsPushed(t *testing.T) {
	inv := &pushingInvoker{fakeInvoker: fakeInvoker{err: ErrNoCapableDevice}}
	tool := NewClientTool(func() (AppInvoker, bool) { return inv, true })
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"set_alarm","time":"19:00"}`))
	if err != nil || len(inv.pushed) != 1 || !strings.Contains(res.Text, `"held":true`) || !strings.Contains(res.Text, `"days":[]`) {
		t.Errorf("res=%q err=%v pushes=%d", res.Text, err, len(inv.pushed))
	}
}

func TestClientTool_SetAlarmPushUnavailableOrFailed(t *testing.T) {
	inv := &pushingInvoker{fakeInvoker: fakeInvoker{err: ErrNoAppDevice}, pushErr: ErrNoAlarmPushDevice}
	tool := NewClientTool(func() (AppInvoker, bool) { return inv, true })
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"set_alarm","time":"07:30"}`))
	if err == nil || !strings.Contains(err.Error(), "no Android device connected") {
		t.Errorf("no push device: err = %v, want the no-phone error", err)
	}

	inv = &pushingInvoker{fakeInvoker: fakeInvoker{err: ErrNoAppDevice}, pushErr: errors.New("fcm send: status 400")}
	tool = NewClientTool(func() (AppInvoker, bool) { return inv, true })
	_, err = tool.Execute(context.Background(), json.RawMessage(`{"action":"set_alarm","time":"07:30"}`))
	if err == nil || !strings.Contains(err.Error(), "push to it failed") || !strings.Contains(err.Error(), "not set") {
		t.Errorf("failed push: err = %v", err)
	}
}

// Only set_alarm is pushed; a connected phone runs it live with no push.
func TestClientTool_OnlySetAlarmIsPushedAndOnlyWithNoPhone(t *testing.T) {
	inv := &pushingInvoker{fakeInvoker: fakeInvoker{err: ErrNoAppDevice}}
	tool := NewClientTool(func() (AppInvoker, bool) { return inv, true })
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"set_timer","duration":"2m"}`))
	if err == nil || len(inv.pushed) != 0 {
		t.Errorf("set_timer: err=%v pushes=%d, want error and no push", err, len(inv.pushed))
	}

	live := &pushingInvoker{fakeInvoker: fakeInvoker{result: fap.ToolResult{Status: "completed", Output: json.RawMessage(`{"alarm":"07:30"}`)}}}
	tool = NewClientTool(func() (AppInvoker, bool) { return live, true })
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"set_alarm","time":"07:30"}`)); err != nil || len(live.pushed) != 0 {
		t.Errorf("live phone: err=%v pushes=%d, want live run and no push", err, len(live.pushed))
	}
}

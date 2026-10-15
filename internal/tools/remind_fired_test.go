package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/memory"
)

// firedWakeFixture builds a remind tool over one store with a wake scheduled
// and marked fired, plus the cancel fn shape a test chooses.
func firedWakeFixture(t *testing.T, cancelReturns bool, held SessionHeldFn) (*Tool, *memory.ReminderStore, int64) {
	t.Helper()
	dbPath := t.TempDir() + "/test.db"
	rs, err := memory.NewReminderStore(dbPath)
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })
	noopSchedule := func(id int64, d time.Duration, msg, sessionKey string) error { return nil }
	cancelFn := func(id int64) bool { return cancelReturns }
	var opts []RemindOption
	if held != nil {
		opts = append(opts, WithSessionHeld(held))
	}
	tool := NewRemindTool(rs, "test", noopSchedule, cancelFn, opts...)

	id, err := rs.AddWake("test", "test/c1", "fire me", "1h")
	if err != nil {
		t.Fatalf("AddWake: %v", err)
	}
	rs.MarkWakeFired(id, "test/c1")
	return tool, rs, id
}

func execRemind(t *testing.T, tool *Tool, params string) (ToolResult, error) {
	t.Helper()
	return tool.Execute(context.Background(), json.RawMessage(params))
}

// execRemindJSON is execRemind with the exec bridge's --json hint set, so the
// tool renders its JSON output shape.
func execRemindJSON(t *testing.T, tool *Tool, params string) (ToolResult, error) {
	t.Helper()
	return tool.Execute(WithOutputHints(context.Background(), OutputHints{Format: OutputFormatJSON}), json.RawMessage(params))
}

// TestRemindList_FiredHeldText pins requirement 5's text form: a wake whose
// timer has fired and whose session is held behind background work lists as
// "fired, held behind background work" — never "overdue".
func TestRemindList_FiredHeldText(t *testing.T) {
	tool, _, id := firedWakeFixture(t, true, func(sk string) bool { return true })

	res, err := execRemind(t, tool, `{"list":true}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(res.Text, "fired, held behind background work") {
		t.Fatalf("list = %q, want the fired-held wording for id=%d (#2233)", res.Text, id)
	}
	if strings.Contains(res.Text, "overdue") {
		t.Fatalf("list = %q, a fired wake must not read as overdue", res.Text)
	}
}

// TestRemindList_FiredWaitingText: a fired wake on a session that is merely
// busy (not held) lists as "fired, waiting to run".
func TestRemindList_FiredWaitingText(t *testing.T) {
	tool, _, _ := firedWakeFixture(t, true, func(sk string) bool { return false })

	res, err := execRemind(t, tool, `{"list":true}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(res.Text, "fired, waiting to run") {
		t.Fatalf("list = %q, want the fired-waiting wording (#2233)", res.Text)
	}
}

// TestRemindList_JSONState pins the JSON list's per-wake state field:
// pending before the timer fires, held while background work delays the wake,
// fired once it is only waiting to run.
func TestRemindList_JSONState(t *testing.T) {
	tool, rs, firedID := firedWakeFixture(t, true, func(sk string) bool { return true })
	pendingID, err := rs.AddWake("test", "test/c1", "not yet", "1h")
	if err != nil {
		t.Fatalf("AddWake: %v", err)
	}

	res, err := execRemindJSON(t, tool, `{"list":true}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var rows []struct {
		ID    int64  `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(res.Text), &rows); err != nil {
		t.Fatalf("parse %q: %v", res.Text, err)
	}
	states := map[int64]string{}
	for _, r := range rows {
		states[r.ID] = r.State
	}
	if states[pendingID] != "pending" {
		t.Errorf("unfired wake state = %q, want pending", states[pendingID])
	}
	if states[firedID] != "held" {
		t.Errorf("fired+held wake state = %q, want held", states[firedID])
	}

	// Not held: the same fired wake reads "fired".
	tool2, _, id2 := firedWakeFixture(t, true, func(sk string) bool { return false })
	res2, err := execRemindJSON(t, tool2, `{"list":true}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var rows2 []struct {
		ID    int64  `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(res2.Text), &rows2); err != nil {
		t.Fatalf("parse %q: %v", res2.Text, err)
	}
	if len(rows2) != 1 || rows2[0].ID != id2 || rows2[0].State != "fired" {
		t.Fatalf("rows = %+v, want one fired row for id=%d", rows2, id2)
	}
}

// TestRemindList_PendingOverdueKeepsOverdueArm pins the switch's default arm:
// a wake past its due time whose timer has NOT fired (not in the fired overlay
// — e.g. restored rows in the moment before the restore re-fires them) still
// lists as "overdue" in text and "pending" in JSON. The fired wording belongs
// only to rows in the fired overlay, and a held session must not leak the
// held state into a wake that has not fired yet.
func TestRemindList_PendingOverdueKeepsOverdueArm(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	rs, err := memory.NewReminderStore(dbPath)
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })
	cancelFn := func(int64) bool { return true }
	tool := NewRemindTool(rs, "test", func(id int64, d time.Duration, msg, sessionKey string) error { return nil },
		cancelFn, WithSessionHeld(func(sk string) bool { return true }))

	pastID, err := rs.AddWake("test", "test/c1", "late timer", "2020-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("AddWake: %v", err)
	}

	res, err := execRemind(t, tool, `{"list":true}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(res.Text, "overdue") {
		t.Fatalf("list = %q, want the overdue wording for the past-due unfired wake", res.Text)
	}
	if strings.Contains(res.Text, "fired") {
		t.Fatalf("list = %q, an unfired wake must not read with fired wording", res.Text)
	}

	resJ, err := execRemindJSON(t, tool, `{"list":true}`)
	if err != nil {
		t.Fatalf("json list: %v", err)
	}
	var rows []struct {
		ID    int64  `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(resJ.Text), &rows); err != nil {
		t.Fatalf("parse %q: %v", resJ.Text, err)
	}
	if len(rows) != 1 || rows[0].ID != pastID || rows[0].State != "pending" {
		t.Fatalf("rows = %+v, want one pending row for id=%d", rows, pastID)
	}
}

// TestRemindCancel_FiredHeldError pins the cancel wording for a fired wake
// whose session is held: it says the wake has fired, is held behind background
// work, and will run when that work ends — never the "may have already fired"
// wording.
func TestRemindCancel_FiredHeldError(t *testing.T) {
	tool, rs, id := firedWakeFixture(t, false, func(sk string) bool { return true })

	_, err := execRemind(t, tool, `{"cancel":`+itoa(id)+`}`)
	if err == nil {
		t.Fatal("cancel of a fired wake succeeded — it is not cancellable")
	}
	for _, want := range []string{"has fired", "held behind background work", "will run when that work ends"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("cancel error = %q, want it to mention %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "may have already fired") {
		t.Errorf("cancel error = %q used the fired-and-lost wording for a held wake", err.Error())
	}
	if wakes, _ := rs.PendingWakes("test"); len(wakes) != 1 {
		t.Errorf("cancelled-in-error wake row = %d left, want 1 (nothing dismissed)", len(wakes))
	}
}

// TestRemindCancel_FiredWaitingError: the not-held variant of the same error.
func TestRemindCancel_FiredWaitingError(t *testing.T) {
	tool, _, id := firedWakeFixture(t, false, func(sk string) bool { return false })

	_, err := execRemind(t, tool, `{"cancel":`+itoa(id)+`}`)
	if err == nil {
		t.Fatal("cancel of a fired wake succeeded — it is not cancellable")
	}
	if !strings.Contains(err.Error(), "has fired and is waiting to run") {
		t.Fatalf("cancel error = %q, want the fired-waiting wording", err.Error())
	}
}

// TestRemindCancel_NeitherKeepsTodaysWording is the characterisation pin: a
// row that is neither live nor fired keeps today's "no live timer — it may
// have already fired" error.
func TestRemindCancel_NeitherKeepsTodaysWording(t *testing.T) {
	// Not fired, no live timer: today's shape.
	tool2, rs, id2 := firedWakeFixture(t, false, nil)
	rs.ClearWakeFired(id2)

	_, err := execRemind(t, tool2, `{"cancel":`+itoa(id2)+`}`)
	if err == nil || !strings.Contains(err.Error(), "may have already fired") {
		t.Fatalf("cancel error = %v, want today's no-live-timer wording", err)
	}
}

// TestRemindCancel_FiredIsNotCancellable pins the #2036 claim rule end to
// end: a fired wake's cancel returns an error and dismisses nothing.
func TestRemindCancel_FiredIsNotCancellable(t *testing.T) {
	tool, rs, id := firedWakeFixture(t, false, func(sk string) bool { return false })

	if _, err := execRemind(t, tool, `{"cancel":`+itoa(id)+`}`); err == nil {
		t.Fatal("a fired wake was cancelled")
	}
	wakes, _ := rs.PendingWakes("test")
	if len(wakes) != 1 || wakes[0].ID != id {
		t.Fatalf("rows after fired-cancel = %+v, want the wake intact", wakes)
	}
}

// TestRemindTool_JSONOutputDocumentsState pins the tool's declared JSON
// output: the list's state field is documented for the model.
func TestRemindTool_JSONOutputDocumentsState(t *testing.T) {
	tool, _, _ := firedWakeFixture(t, true, nil)
	if !strings.Contains(tool.JSONOutput, `"state"`) {
		t.Fatalf("JSONOutput does not document the state field: %q", tool.JSONOutput)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"foci/internal/fap"
)

// routingInvoker fakes the hub for the client tool's routing: a per-device-tool
// error (absent = the call succeeds) and a capability probe.
type routingInvoker struct {
	errFor   map[string]error
	canRun   map[string]bool
	calls    []string // "tool/action"
	lastArgs json.RawMessage
}

func (r *routingInvoker) InvokeTool(ctx context.Context, tool, action string, args json.RawMessage) (fap.ToolResult, error) {
	r.calls = append(r.calls, tool+"/"+action)
	r.lastArgs = args
	if err := r.errFor[tool]; err != nil {
		return fap.ToolResult{}, err
	}
	return fap.ToolResult{Status: fap.ToolStatusCompleted, Output: json.RawMessage(`{"ok":true}`)}, nil
}

func (r *routingInvoker) CanRunTool(tool string) bool { return r.canRun[tool] }

func execClient(t *testing.T, inv AppInvoker, params string) string {
	t.Helper()
	tool := NewClientTool(func() (AppInvoker, bool) { return inv, inv != nil })
	res, err := tool.Execute(context.Background(), json.RawMessage(params))
	if err != nil {
		t.Fatalf("Execute(%s): Go error %v (want an error string)", params, err)
	}
	return res.Text
}

// TestClientTool_RefusesBadArguments: each bad argument is refused with an
// error naming it, and nothing is sent to the phone (requirement 3).
func TestClientTool_RefusesBadArguments(t *testing.T) {
	cases := []struct {
		name, params, wantArg string
	}{
		{"hour 24", `{"action":"set_alarm","args":["24:10","x"]}`, "time"},
		{"minute 60", `{"action":"set_alarm","time":"07:60"}`, "time"},
		{"not a time", `{"action":"set_alarm","time":"7.30"}`, "time"},
		{"no time", `{"action":"set_alarm","label":"x"}`, "time"},
		{"unknown weekday", `{"action":"set_alarm","args":["07:30","x"],"days":"mon,funday"}`, "funday"},
		{"zero duration", `{"action":"set_timer","duration":"0"}`, "duration"},
		{"negative duration", `{"action":"set_timer","duration":"-2m"}`, "duration"},
		{"bad duration", `{"action":"set_timer","duration":"soon"}`, "duration"},
		{"no duration", `{"action":"set_timer"}`, "duration"},
		{"empty number", `{"action":"call","number":"  "}`, "number"},
		{"letters number", `{"action":"call","number":"eleni"}`, "number"},
		{"empty name", `{"action":"contact_lookup","name":""}`, "name"},
		{"empty text", `{"action":"clipboard","text":""}`, "text"},
		{"no text", `{"action":"clipboard"}`, "text"},
		{"intent url", `{"action":"open_url","url":"intent://x"}`, "url"},
		{"file url", `{"action":"open_url","url":"file:///sdcard/x"}`, "url"},
		{"javascript url", `{"action":"open_url","url":"javascript:alert(1)"}`, "url"},
		{"empty url", `{"action":"open_url"}`, "url"},
		{"zero ring", `{"action":"ring_phone","seconds":0}`, "seconds"},
		{"negative ring", `{"action":"ring_phone","args":["-5"]}`, "seconds"},
		{"long ring", `{"action":"ring_phone","seconds":3600}`, "seconds"},
		{"empty task", `{"action":"tasker_perform"}`, "task"},
		{"extra word", `{"action":"open_url","args":["https://a.example","https://b.example"]}`, "extra argument"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inv := &routingInvoker{}
			got := execClient(t, inv, c.params)
			if !strings.HasPrefix(got, "Error:") || !strings.Contains(got, c.wantArg) {
				t.Errorf("got %q, want an Error naming %q", got, c.wantArg)
			}
			if len(inv.calls) != 0 {
				t.Errorf("invoked the phone %v for a refused call", inv.calls)
			}
		})
	}
}

// TestClientTool_WireArgs: valid calls reach the phone's client handler with
// the action name and canonical args; shell words fill the named params.
func TestClientTool_WireArgs(t *testing.T) {
	cases := []struct {
		name, params, action string
		want                 map[string]any
	}{
		{"alarm from words", `{"action":"set_alarm","args":["07:30","Take","T"],"days":"wed, Monday"}`, "set_alarm",
			map[string]any{"hour": 7.0, "minute": 30.0, "label": "Take T", "days": []any{"mon", "wed"}}},
		{"alarm named", `{"action":"set_alarm","time":"0:05"}`, "set_alarm",
			map[string]any{"hour": 0.0, "minute": 5.0, "label": "", "days": []any{}}},
		{"alarm 23:59", `{"action":"set_alarm","time":"23:59","label":"late"}`, "set_alarm",
			map[string]any{"hour": 23.0, "minute": 59.0, "label": "late", "days": []any{}}},
		{"timer 2m", `{"action":"set_timer","args":["2m","tea"]}`, "set_timer",
			map[string]any{"seconds": 120.0, "label": "tea"}},
		{"timer bare seconds", `{"action":"set_timer","duration":"90"}`, "set_timer",
			map[string]any{"seconds": 90.0, "label": ""}},
		{"call", `{"action":"call","args":["+44","1234","567890"]}`, "call",
			map[string]any{"number": "+44 1234 567890"}},
		{"contact", `{"action":"contact_lookup","args":["Eleni"]}`, "contact_lookup",
			map[string]any{"name": "Eleni"}},
		{"clipboard joins words", `{"action":"clipboard","args":["hello","2802"]}`, "clipboard",
			map[string]any{"text": "hello 2802"}},
		{"open https", `{"action":"open_url","args":["https://example.com"]}`, "open_url",
			map[string]any{"url": "https://example.com"}},
		{"open geo", `{"action":"open_url","url":"geo:51.46,-0.11"}`, "open_url",
			map[string]any{"url": "geo:51.46,-0.11"}},
		{"location", `{"action":"location"}`, "location", map[string]any{}},
		{"ring default", `{"action":"ring_phone"}`, "ring_phone", map[string]any{"seconds": 30.0}},
		{"ring word", `{"action":"ring_phone","args":["5"]}`, "ring_phone", map[string]any{"seconds": 5.0}},
		{"ring number", `{"action":"ring_phone","seconds":12}`, "ring_phone", map[string]any{"seconds": 12.0}},
		{"tasker words", `{"action":"tasker_perform","args":["foci_battery","a b"]}`, "tasker_perform",
			map[string]any{"task": "foci_battery", "par1": "a b", "par2": ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inv := &routingInvoker{}
			got := execClient(t, inv, c.params)
			if got != `{"ok":true}` {
				t.Fatalf("result %q, want the phone's output", got)
			}
			if want := []string{"client/" + c.action}; !reflect.DeepEqual(inv.calls, want) {
				t.Fatalf("calls %v, want %v", inv.calls, want)
			}
			var args map[string]any
			if err := json.Unmarshal(inv.lastArgs, &args); err != nil {
				t.Fatalf("args not JSON: %v", err)
			}
			if !reflect.DeepEqual(args, c.want) {
				t.Errorf("args %v, want %v", args, c.want)
			}
		})
	}
}

// TestClientTool_DescriptionListsEveryAction (requirement 2).
func TestClientTool_DescriptionListsEveryAction(t *testing.T) {
	tool := NewClientTool(func() (AppInvoker, bool) { return nil, false })
	want := []string{"tasker_list", "tasker_perform", "set_alarm", "set_timer", "call",
		"contact_lookup", "clipboard", "open_url", "location", "ring_phone"}
	if got := clientActionNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("actions %v, want %v", got, want)
	}
	for _, a := range want {
		if !strings.Contains(tool.Description, "\n  "+a) {
			t.Errorf("description does not list action %s", a)
		}
	}
	for _, s := range []string{"switches each action on or off in the app", "grants there the Android permission"} {
		if !strings.Contains(tool.Description, s) {
			t.Errorf("description lacks %q", s)
		}
	}
	if tool.Name != "client" || !tool.ExecExport {
		t.Errorf("Name=%q ExecExport=%v, want client/true (shell foci_client)", tool.Name, tool.ExecExport)
	}
}

// TestClientTool_NoDeviceErrorsAreDistinct (requirement 8): no phone, no
// capable phone, a phone app too old, and no answer in time each read
// differently, and none is a Go error.
func TestClientTool_NoDeviceErrorsAreDistinct(t *testing.T) {
	noInvoker := execClient(t, nil, `{"action":"set_alarm","time":"07:00"}`)
	noApp := execClient(t, &routingInvoker{errFor: map[string]error{"client": ErrNoAppDevice}}, `{"action":"set_alarm","time":"07:00"}`)
	noCapable := execClient(t, &routingInvoker{errFor: map[string]error{"client": ErrNoCapableDevice, "android": ErrNoCapableDevice}}, `{"action":"set_alarm","time":"07:00"}`)
	tooOld := execClient(t, &routingInvoker{errFor: map[string]error{"client": ErrNoCapableDevice}, canRun: map[string]bool{"android": true}}, `{"action":"set_alarm","time":"07:00"}`)
	timeout := execClient(t, &routingInvoker{errFor: map[string]error{"client": context.DeadlineExceeded}}, `{"action":"set_alarm","time":"07:00"}`)

	if noInvoker != noApp {
		t.Errorf("no invoker %q and no app socket %q should read the same", noInvoker, noApp)
	}
	if !strings.Contains(noApp, "no Android device connected") {
		t.Errorf("no app: %q", noApp)
	}
	if !strings.Contains(noCapable, "no connected device can run foci_client") {
		t.Errorf("no capable: %q", noCapable)
	}
	if !strings.Contains(tooOld, "update the foci app") || !strings.Contains(tooOld, "set_alarm") {
		t.Errorf("too old: %q", tooOld)
	}
	if !strings.Contains(timeout, "did not answer within") {
		t.Errorf("timeout: %q", timeout)
	}
	seen := map[string]bool{}
	for _, s := range []string{noApp, noCapable, tooOld, timeout} {
		if !strings.HasPrefix(s, "Error:") {
			t.Errorf("%q is not an error string", s)
		}
		if seen[s] {
			t.Errorf("duplicate error string %q", s)
		}
		seen[s] = true
	}
}

// TestClientTool_LegacyPhoneServesTasker (requirement 12): a phone whose app
// predates the client handler still runs the Tasker actions through its
// "android" handler, with today's action names and args.
func TestClientTool_LegacyPhoneServesTasker(t *testing.T) {
	inv := &routingInvoker{errFor: map[string]error{"client": ErrNoCapableDevice}, canRun: map[string]bool{"android": true}}
	got := execClient(t, inv, `{"action":"tasker_perform","task":"foci_battery","par1":"x"}`)
	if got != `{"ok":true}` {
		t.Fatalf("result %q", got)
	}
	if want := []string{"client/tasker_perform", "android/perform"}; !reflect.DeepEqual(inv.calls, want) {
		t.Fatalf("calls %v, want %v", inv.calls, want)
	}
	var args map[string]any
	_ = json.Unmarshal(inv.lastArgs, &args)
	if want := map[string]any{"task": "foci_battery", "par1": "x", "par2": ""}; !reflect.DeepEqual(args, want) {
		t.Errorf("legacy args %v, want %v", args, want)
	}

	inv = &routingInvoker{errFor: map[string]error{"client": ErrNoCapableDevice}, canRun: map[string]bool{"android": true}}
	execClient(t, inv, `{"action":"tasker_list"}`)
	if want := []string{"client/tasker_list", "android/list"}; !reflect.DeepEqual(inv.calls, want) {
		t.Fatalf("list calls %v, want %v", inv.calls, want)
	}

	// A non-Tasker action is never sent to the legacy handler.
	inv = &routingInvoker{errFor: map[string]error{"client": ErrNoCapableDevice}, canRun: map[string]bool{"android": true}}
	execClient(t, inv, `{"action":"clipboard","text":"x"}`)
	if want := []string{"client/clipboard"}; !reflect.DeepEqual(inv.calls, want) {
		t.Fatalf("clipboard calls %v, want %v", inv.calls, want)
	}
}

// TestClientShellFunc_RestWords: the exported shell function keeps each bare
// word after the action as its own element, so a quoted label survives intact
// and flags mix in anywhere.
func TestClientShellFunc_RestWords(t *testing.T) {
	t.Parallel()
	tool := NewClientTool(func() (AppInvoker, bool) { return nil, false })
	if err := validateShellFuncSchemaParity(tool); err != nil {
		t.Fatalf("parity: %v", err)
	}
	body := generateShellFunc(tool)
	cases := []struct {
		invocation string
		want       map[string]any
	}{
		{`foci_client set_alarm 07:30 "Take T to nursery" --days mon,wed`,
			map[string]any{"action": "set_alarm", "args": []any{"07:30", "Take T to nursery"}, "days": "mon,wed"}},
		{`foci_client --days fri set_alarm 06:00`,
			map[string]any{"action": "set_alarm", "args": []any{"06:00"}, "days": "fri"}},
		{`foci_client ring_phone -5`,
			map[string]any{"action": "ring_phone", "args": []any{"-5"}}},
		{`foci_client location`,
			map[string]any{"action": "location"}},
		{`foci_client ring_phone --seconds 5`,
			map[string]any{"action": "ring_phone", "seconds": 5.0}},
		{`foci_client --action clipboard "a b" c`,
			map[string]any{"action": "clipboard", "args": []any{"a b", "c"}}},
	}
	for _, c := range cases {
		out, code := runShellFunc(t, body, c.invocation)
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s", c.invocation, code, out)
		}
		_, payload, ok := strings.Cut(strings.TrimSpace(out), "FOCI_CALL_REACHED:")
		if !ok {
			t.Fatalf("%s: foci-call not reached:\n%s", c.invocation, out)
		}
		var call struct {
			Tool   string         `json:"tool"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(payload), &call); err != nil {
			t.Fatalf("%s: payload %q: %v", c.invocation, payload, err)
		}
		if call.Tool != "client" || !reflect.DeepEqual(call.Params, c.want) {
			t.Errorf("%s: tool=%s params=%v, want client %v", c.invocation, call.Tool, call.Params, c.want)
		}
	}

	out, code := runShellFunc(t, body, `foci_client clipboard x --args '["y"]'`)
	if code == 0 || !strings.Contains(out, "use one form") {
		t.Errorf("words plus --args: exit %d, out %q; want a refusal", code, out)
	}
	help, _ := runShellFunc(t, body, `foci_client --help`)
	if !strings.Contains(help, "Usage: foci_client <action> [args...]") || !strings.Contains(help, "set_alarm <HH:MM>") {
		t.Errorf("help lacks the usage or the action list:\n%s", help)
	}
}

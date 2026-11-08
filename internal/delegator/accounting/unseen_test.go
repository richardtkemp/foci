package accounting

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var testUsageKeys = UsageKeys{
	"input": nil,
	"cache": {"read": nil, "write": nil},
	"iter":  AnyUsage,
}

// TestUnseenUsageKeys: every key the schema does not list is reported with its
// dotted path and compact value, nested ones included; a key marked AnyUsage
// is not looked into, and a nested schema is not applied to a non-object.
func TestUnseenUsageKeys(t *testing.T) {
	raw := `{"input":1,"cache":{"read":2,"write":3,"ttl24h":{"n": 4}},"iter":[{"anything":1}],"speed":"fast","new":null}`
	got := UnseenUsageKeys([]byte(raw), testUsageKeys)
	want := []UnseenKey{
		{Path: "cache.ttl24h", Value: `{"n":4}`},
		{Path: "new", Value: "null"},
		{Path: "speed", Value: `"fast"`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	for _, raw := range []string{"", "null", `{"input":1,"cache":null}`, `[1]`} {
		if got := UnseenUsageKeys([]byte(raw), testUsageKeys); len(got) != 0 {
			t.Errorf("%q: got %+v, want none", raw, got)
		}
	}
}

// TestStandardTierOnly: speed and service_tier other than "standard" are
// reported; absent or null is standard.
func TestStandardTierOnly(t *testing.T) {
	for raw, want := range map[string][]string{
		`{"speed":"standard","service_tier":"standard"}`: nil,
		`{"speed":null}`:   nil,
		`{}`:               nil,
		`{"speed":"fast"}`: {`speed="fast"`},
		`{"service_tier":"priority","speed":"fast"}`: {`service_tier="priority"`, `speed="fast"`},
	} {
		var got []string
		for _, k := range StandardTierOnly([]byte(raw)) {
			got = append(got, k.Path+"="+k.Value)
			if k.Reason == "" {
				t.Errorf("%s: a tier key must carry its reason", raw)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", raw, got, want)
		}
	}
}

// TestAlarmUnseenOncePerKey: a key alarms once per ledger (process), per
// adapter; an unknown key is the same key whatever its value, a tier value is
// new for each value; the alarm is invUnseenField with the backend and version
// given. A nil ledger is a no-op.
func TestAlarmUnseenOncePerKey(t *testing.T) {
	var alarms []Alarm
	l, _, err := Open(filepath.Join(t.TempDir(), "api.db"), Options{OnAlarm: func(a Alarm) { alarms = append(alarms, a) }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	k1 := []UnseenKey{{Path: "new", Value: "1"}}
	l.AlarmUnseen(BackendAPI, "v1", "anthropic", "message m1", k1)
	l.AlarmUnseen(BackendAPI, "v1", "anthropic", "message m2", []UnseenKey{{Path: "new", Value: "2"}})
	if len(alarms) != 1 {
		t.Fatalf("got %d alarms, want 1: %+v", len(alarms), alarms)
	}
	a := alarms[0]
	if a.Invariant != InvUnseenField || a.Backend != BackendAPI || a.Version != "v1" ||
		!strings.Contains(a.Detail, "anthropic") || !strings.Contains(a.Detail, "message m1") || !strings.Contains(a.Detail, "new=1") {
		t.Errorf("alarm = %+v", a)
	}
	l.AlarmUnseen(BackendAPI, "", "openai", "response r1", k1) // another adapter: its own key
	tier := func(v string) []UnseenKey { return []UnseenKey{{Path: "speed", Value: v, Reason: "r"}} }
	l.AlarmUnseen(BackendAPI, "", "anthropic", "m3", tier(`"fast"`))
	l.AlarmUnseen(BackendAPI, "", "anthropic", "m4", tier(`"fast"`))
	l.AlarmUnseen(BackendAPI, "", "anthropic", "m5", tier(`"priority"`))
	if len(alarms) != 4 {
		t.Errorf("got %d alarms, want 4 (openai's own, speed fast once, speed priority): %+v", len(alarms), alarms)
	}
	var nilLedger *Ledger
	nilLedger.AlarmUnseen(BackendAPI, "", "x", "y", k1)
}

// TestUnseenUsageKeysZeroCountsNothing (#2982): an unknown key whose value
// counts nothing — the number 0, or an object of such values — leaves nothing
// unpriced, so it is not reported; the same key with a count is, and so is a
// non-numeric value (a tier string, null).
func TestUnseenUsageKeysZeroCountsNothing(t *testing.T) {
	raw := `{"input":1,"cache":{"read":2,"image":0,"video":0.0},"zero":0,"zobj":{"a":0,"b":{"c":0}},"empty":{},` +
		`"some":{"a":0,"b":3},"n":5,"tier":"fast","nul":null,"f":false}`
	got := UnseenUsageKeys([]byte(raw), testUsageKeys)
	want := []UnseenKey{
		{Path: "f", Value: "false"},
		{Path: "n", Value: "5"},
		{Path: "nul", Value: "null"},
		{Path: "some", Value: `{"a":0,"b":3}`},
		{Path: "tier", Value: `"fast"`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestAlarmUnseenZeroThenCount (#2982): a key first seen at 0 is not marked
// seen, so the first call that carries a count for it still alarms.
func TestAlarmUnseenZeroThenCount(t *testing.T) {
	var alarms []Alarm
	l, _, err := Open(filepath.Join(t.TempDir(), "api.db"), Options{OnAlarm: func(a Alarm) { alarms = append(alarms, a) }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	l.AlarmUnseen(BackendAPI, "", "openai", "r1", UnseenUsageKeys([]byte(`{"input":1,"image_tokens":0}`), testUsageKeys))
	if len(alarms) != 0 {
		t.Fatalf("zero-valued key alarmed: %+v", alarms)
	}
	l.AlarmUnseen(BackendAPI, "", "openai", "r2", UnseenUsageKeys([]byte(`{"input":1,"image_tokens":12}`), testUsageKeys))
	if len(alarms) != 1 || !strings.Contains(alarms[0].Detail, "image_tokens=12") {
		t.Errorf("alarms = %+v, want one naming image_tokens=12", alarms)
	}
}

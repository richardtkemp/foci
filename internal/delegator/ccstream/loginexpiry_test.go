package ccstream

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"foci/internal/delegator"
)

// Frames captured live on 2026-11-06 with a scratch config dir and a fake
// credential, against a local mock API (trimmed to the fields foci reads).
const (
	result318Frame = `{"type":"result","subtype":"success","is_error":false,"result":"ok","credential_expires_at":"2026-11-11T00:11:19.571Z","session_id":"s","uuid":"u"}`
	result314Frame = `{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s","uuid":"u"}`
)

// TestCredentialExpiry_ParseNeverFails: a date foci cannot read means no date.
// It must never fail the parse of the frame that carries it.
func TestCredentialExpiry_ParseNeverFails(t *testing.T) {
	t.Parallel()
	want := time.Date(2026, 11, 11, 0, 11, 19, 571000000, time.UTC)
	for _, tc := range []struct {
		name, field string
		want        time.Time
	}{
		{"iso string", `"2026-11-11T00:11:19.571Z"`, want},
		{"null", `null`, time.Time{}},
		{"number", `1793000000000`, time.Time{}},
		{"object", `{"a":1}`, time.Time{}},
		{"bad string", `"next tuesday"`, time.Time{}},
		{"empty string", `""`, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var msg ResultMessage
			if err := json.Unmarshal([]byte(`{"type":"result","result":"ok","credential_expires_at":`+tc.field+`}`), &msg); err != nil {
				t.Fatalf("frame failed to parse: %v", err)
			}
			if msg.Result != "ok" {
				t.Errorf("Result = %q, the rest of the frame was lost", msg.Result)
			}
			if !msg.CredentialExpiresAt.at.Equal(tc.want) {
				t.Errorf("date = %v, want %v", msg.CredentialExpiresAt.at, tc.want)
			}
		})
	}
}

// TestOnResult_ReportsLoginExpiry: a result with the date reports it, and an
// older CC's result, with no such field, reports nothing.
func TestOnResult_ReportsLoginExpiry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, frame string
		want        time.Time
	}{
		{"2.1.318", result318Frame, time.Date(2026, 11, 11, 0, 11, 19, 571000000, time.UTC)},
		{"2.1.314", result314Frame, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got time.Time
			b := &Backend{}
			b.SetHostHooks(delegator.HostHooks{OnLoginExpiry: func(at time.Time) { got = at }})
			applyHandler(b, &testHandler{})
			var msg ResultMessage
			if err := json.Unmarshal([]byte(tc.frame), &msg); err != nil {
				t.Fatal(err)
			}
			b.OnResult(&msg)
			if !got.Equal(tc.want) {
				t.Errorf("reported %v, want %v", got, tc.want)
			}
		})
	}
}

// writeFakeClaude writes a script that answers `auth status` with body and the
// given exit code.
func writeFakeClaude(t *testing.T, body string, exit int) string {
	t.Helper()
	dir := t.TempDir()
	bodyPath := filepath.Join(dir, "body.json")
	if err := os.WriteFile(bodyPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "claude")
	script := "#!/bin/sh\n/bin/cat '" + bodyPath + "'\nexit " + strconv.Itoa(exit) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestCheckReady_LoginExpiry: the startup probe reports the date, and a missing
// or unreadable date neither warns nor fails readiness. The bodies are the live
// `claude auth status` shapes of 2.1.318 and 2.1.314.
func TestCheckReady_LoginExpiry(t *testing.T) {
	want := time.Date(2026, 11, 11, 0, 10, 35, 282000000, time.UTC)
	for _, tc := range []struct {
		name, body string
		want       time.Time
	}{
		{"2.1.318 date", `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","loginExpiresAt":"2026-11-11T00:10:35.282Z","email":null}`, want},
		{"2.1.318 null", `{"loggedIn":true,"authMethod":"claude.ai","loginExpiresAt":null}`, time.Time{}},
		{"2.1.314 absent", `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}`, time.Time{}},
		{"number", `{"loggedIn":true,"authMethod":"claude.ai","loginExpiresAt":1793000000000}`, time.Time{}},
		{"bad string", `{"loggedIn":true,"authMethod":"claude.ai","loginExpiresAt":"soon"}`, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got time.Time
			calls := 0
			b := &Backend{cfg: map[string]any{"binary": writeFakeClaude(t, tc.body, 0)}}
			b.SetHostHooks(delegator.HostHooks{
				OnAuthFailure: func(d string) { t.Errorf("re-login fired: %s", d) },
				OnLoginExpiry: func(at time.Time) { got = at; calls++ },
			})
			ready, err := b.CheckReady(context.Background())
			if err != nil || !ready {
				t.Fatalf("CheckReady = (%v, %v), want ready", ready, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("reported %v, want %v", got, tc.want)
			}
			if tc.want.IsZero() && calls != 0 {
				t.Errorf("hook called %d times for no date", calls)
			}
		})
	}
}

// TestCheckReady_LoggedOutReportsNoExpiry: a logged-out probe goes to re-login
// and does not also warn about a date.
func TestCheckReady_LoggedOutReportsNoExpiry(t *testing.T) {
	relogin, warned := false, false
	b := &Backend{cfg: map[string]any{"binary": writeFakeClaude(t, `{"loggedIn":false,"authMethod":"none","loginExpiresAt":"2026-11-11T00:10:35.282Z"}`, 1)}}
	b.SetHostHooks(delegator.HostHooks{
		OnAuthFailure: func(string) { relogin = true },
		OnLoginExpiry: func(time.Time) { warned = true },
	})
	ready, err := b.CheckReady(context.Background())
	if err != nil || ready {
		t.Fatalf("CheckReady = (%v, %v), want not ready, no error", ready, err)
	}
	if !relogin || warned {
		t.Errorf("relogin=%v warned=%v, want true, false", relogin, warned)
	}
}

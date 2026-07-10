package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeCheckers builds a userActivityChecker and sessionActivityChecker that
// each return a fixed bool — useful for table-driven gate tests where we
// don't want to exercise SQLite/timestamp logic, just the gate's branching
// on the four conditions plus in-flight.
func fakeCheckers(userActive, sessionActive bool) (userActivityChecker, sessionActivityChecker) {
	uc := func(string, time.Duration) bool { return userActive }
	sc := func(string, time.Duration) bool { return sessionActive }
	return uc, sc
}

// TestCheckActivityGate_FourFlagMatrix exercises every combination of the
// four conditions × {user-active, user-inactive} × {session-active,
// session-inactive} × {in-flight, idle}. The matrix is the contract: each
// row pins down exactly one (input → outcome) mapping the gate must honour.
//
// In-flight short-circuit: when InFlight=true, both `userActiveWithin` and
// `sessionActiveWithin` evaluate to true regardless of the underlying
// checker results. So:
//
//   - --if-user-inactive skips (gate fires) if InFlight=true
//   - --if-user-active passes (gate doesn't fire) if InFlight=true
//   - --if-inactive skips if InFlight=true
//   - --if-active passes if InFlight=true
func TestCheckActivityGate_FourFlagMatrix(t *testing.T) {
	tests := []struct {
		name           string
		in             activityGateInputs
		userActive     bool
		sessionActive  bool
		wantPass       bool   // true = gate returned true (request proceeds)
		wantBodyMarker string // substring expected in skip-response body (when !wantPass)
	}{
		// --- No conditions: always passes ---
		{
			name:     "no conditions, idle",
			in:       activityGateInputs{},
			wantPass: true,
		},

		// --- IfUserActive ---
		{
			name:           "if_user_active=1h, user-inactive, idle → skip",
			in:             activityGateInputs{IfUserActive: "1h"},
			wantPass:       false,
			wantBodyMarker: "no recent user activity",
		},
		{
			name:       "if_user_active=1h, user-active, idle → pass",
			in:         activityGateInputs{IfUserActive: "1h"},
			userActive: true,
			wantPass:   true,
		},
		{
			name:     "if_user_active=1h, user-inactive, in-flight → pass (in-flight counts as user attention)",
			in:       activityGateInputs{IfUserActive: "1h", InFlight: true},
			wantPass: true,
		},

		// --- IfUserInactive ---
		{
			name:     "if_user_inactive=1h, user-inactive, idle → pass",
			in:       activityGateInputs{IfUserInactive: "1h"},
			wantPass: true,
		},
		{
			name:           "if_user_inactive=1h, user-active, idle → skip",
			in:             activityGateInputs{IfUserInactive: "1h"},
			userActive:     true,
			wantPass:       false,
			wantBodyMarker: "user recently active",
		},
		{
			name:           "if_user_inactive=1h, user-inactive, in-flight → skip (in-flight counts as recent attention)",
			in:             activityGateInputs{IfUserInactive: "1h", InFlight: true},
			wantPass:       false,
			wantBodyMarker: "user recently active",
		},

		// --- IfActive ---
		{
			name:           "if_active=1h, session-inactive, idle → skip",
			in:             activityGateInputs{IfActive: "1h"},
			wantPass:       false,
			wantBodyMarker: "no recent activity",
		},
		{
			name:          "if_active=1h, session-active, idle → pass",
			in:            activityGateInputs{IfActive: "1h"},
			sessionActive: true,
			wantPass:      true,
		},
		{
			name:     "if_active=1h, session-inactive, in-flight → pass",
			in:       activityGateInputs{IfActive: "1h", InFlight: true},
			wantPass: true,
		},

		// --- IfInactive (the keepalive scenario) ---
		{
			name:     "if_inactive=45m, session-inactive, idle → pass",
			in:       activityGateInputs{IfInactive: "45m"},
			wantPass: true,
		},
		{
			name:           "if_inactive=45m, session-active, idle → skip",
			in:             activityGateInputs{IfInactive: "45m"},
			sessionActive:  true,
			wantPass:       false,
			wantBodyMarker: "session recently active",
		},
		{
			name:           "if_inactive=45m, session-inactive, in-flight → skip (THE bug fix — TODO #753)",
			in:             activityGateInputs{IfInactive: "45m", InFlight: true},
			wantPass:       false,
			wantBodyMarker: "session recently active",
		},

		// --- Combined: in-flight short-circuits both gates simultaneously ---
		{
			name: "all four flags, idle, no activity → if_user_active fires first",
			in: activityGateInputs{
				IfUserActive: "1h", IfUserInactive: "1h",
				IfActive: "1h", IfInactive: "1h",
			},
			wantPass:       false,
			wantBodyMarker: "no recent user activity",
		},
		{
			name: "all four flags, in-flight, no activity → if_user_inactive fires first",
			in: activityGateInputs{
				IfUserActive: "1h", IfUserInactive: "1h",
				IfActive: "1h", IfInactive: "1h",
				InFlight: true,
			},
			wantPass:       false,
			wantBodyMarker: "user recently active",
		},

		// --- Bad-duration validation ---
		{
			name:           "bad if_user_active duration → 400",
			in:             activityGateInputs{IfUserActive: "not-a-duration"},
			wantPass:       false,
			wantBodyMarker: "bad if_user_active duration",
		},
		{
			name:           "bad if_user_inactive duration → 400",
			in:             activityGateInputs{IfUserInactive: "not-a-duration"},
			wantPass:       false,
			wantBodyMarker: "bad if_user_inactive duration",
		},
		{
			name:           "bad if_active duration → 400",
			in:             activityGateInputs{IfActive: "not-a-duration"},
			wantPass:       false,
			wantBodyMarker: "bad if_active duration",
		},
		{
			name:           "bad if_inactive duration → 400",
			in:             activityGateInputs{IfInactive: "not-a-duration"},
			wantPass:       false,
			wantBodyMarker: "bad if_inactive duration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uc, sc := fakeCheckers(tt.userActive, tt.sessionActive)
			w := httptest.NewRecorder()
			got := checkActivityGate(w, tt.in, uc, sc)
			if got != tt.wantPass {
				t.Fatalf("checkActivityGate returned %v, want %v (body: %s)", got, tt.wantPass, w.Body.String())
			}
			if !tt.wantPass && tt.wantBodyMarker != "" {
				if !strings.Contains(w.Body.String(), tt.wantBodyMarker) {
					t.Fatalf("response body %q missing marker %q", w.Body.String(), tt.wantBodyMarker)
				}
			}
		})
	}
}

// TestCheckActivityGate_KeepaliveBugRegression reproduces the exact scenario
// that motivated TODO #753: a long-running turn (in-flight, no recent
// last_activity yet because it's the same turn) triggers a keepalive cron
// with --if-inactive 45m. Without the in-flight short-circuit, the cron
// would queue behind the running turn. With the short-circuit, it skips.
func TestCheckActivityGate_KeepaliveBugRegression(t *testing.T) {
	// State: the agent is mid-turn (e.g. 2h health-check still running, or
	// blocked waiting on a CC permission decision). No recent user activity.
	// Session metadata's last_activity hasn't fired yet for THIS turn — it
	// was only set at turn entry, but cleaned out long ago by previous
	// turns. The signal that matters is InFlight=true.
	uc, sc := fakeCheckers(false, false)
	in := activityGateInputs{
		AgentID:     "clutch",
		SessionBase: "clutch/c5970082313",
		InFlight:    true,
		IfInactive:  "45m",
		LogTag:      "branch",
		Endpoint:    "/branch",
	}

	w := httptest.NewRecorder()
	got := checkActivityGate(w, in, uc, sc)
	if got {
		t.Fatalf("gate returned true (proceed) — keepalive should have been skipped because a turn is in flight")
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, w.Body.String())
	}
	if resp["response"] != "skipped: session recently active" {
		t.Fatalf("response = %q, want skipped marker", resp["response"])
	}
}

// TestCheckActivityGate_FlagOrdering pins down the evaluation order:
// IfUserActive → IfUserInactive → IfActive → IfInactive. When multiple
// flags would skip, the first one in this order wins. This matters because
// the response body identifies *which* gate fired, and tooling may parse it.
func TestCheckActivityGate_FlagOrdering(t *testing.T) {
	uc, sc := fakeCheckers(false, false)

	// All four set; user is inactive AND session is inactive. With no
	// activity at all, the first applicable skip is IfUserActive (which
	// requires user activity to pass).
	in := activityGateInputs{
		IfUserActive:   "1h",
		IfUserInactive: "1h",
		IfActive:       "1h",
		IfInactive:     "1h",
	}
	w := httptest.NewRecorder()
	if checkActivityGate(w, in, uc, sc) {
		t.Fatalf("gate returned true; want false (IfUserActive should fire)")
	}
	if !strings.Contains(w.Body.String(), "no recent user activity") {
		t.Fatalf("first-firing gate should be IfUserActive; got body=%s", w.Body.String())
	}
}

// TestCheckActivityGate_HTTPStatusOnSkip pins down that a skipped request
// returns HTTP 200 with a JSON body — not a 4xx. This matters because the
// caller (foci CLI / cron) treats the skip as "successful no-op", not
// "request failed."
func TestCheckActivityGate_HTTPStatusOnSkip(t *testing.T) {
	uc, sc := fakeCheckers(false, true)
	in := activityGateInputs{IfInactive: "1h"}
	w := httptest.NewRecorder()
	if checkActivityGate(w, in, uc, sc) {
		t.Fatalf("expected gate to skip")
	}
	// httptest.NewRecorder defaults Code to 200; the gate should not have
	// written a different status code on skip (only on bad duration parse).
	if w.Code != http.StatusOK {
		t.Fatalf("skip status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("skip Content-Type = %q, want application/json", ct)
	}
}

// TestCheckActivityGate_HTTPStatusOnBadDuration verifies that an unparseable
// duration string fails fast with 400.
func TestCheckActivityGate_HTTPStatusOnBadDuration(t *testing.T) {
	uc, sc := fakeCheckers(false, false)
	in := activityGateInputs{IfActive: "garbage"}
	w := httptest.NewRecorder()
	if checkActivityGate(w, in, uc, sc) {
		t.Fatalf("expected gate to reject")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad-duration status = %d, want 400", w.Code)
	}
}

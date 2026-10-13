package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

// newControlTestBackend returns a Backend (session "sess-ctrl") wired to
// a fresh recorded test server.
func newControlTestBackend(t *testing.T) (*Backend, *controlRecorder) {
	t.Helper()
	hs, rec := newControlTestServer(t)
	return newControlTestBackendForSession(t, hs, "sess-ctrl"), rec
}

// newControlTestServer starts ONE httptest server recording every
// request into a shared controlRecorder. One opencode server serves
// every session of an agent, so tests exercising sibling Backends must
// share a server + recorder (#2330). For PATCH /session/:id the
// recorder mirrors opencode's session.update semantics — the body's
// permission rules are concatenated onto the session's accumulated
// ruleset — so evaluate resolves the accumulated state the way opencode
// does (findLast).
func newControlTestServer(t *testing.T) (*httptest.Server, *controlRecorder) {
	t.Helper()
	rec := &controlRecorder{rules: map[string][]rule{}}
	hs := httptest.NewServer(http.HandlerFunc(rec.serveHTTP))
	t.Cleanup(hs.Close)
	return hs, rec
}

// newControlTestBackendForSession returns a Backend bound to sessionID
// on the shared test server — the state a SendControl caller is in once
// Start has created/resumed that session.
func newControlTestBackendForSession(t *testing.T, hs *httptest.Server, sessionID string) *Backend {
	t.Helper()
	return &Backend{
		server: &Server{
			baseURL:  hs.URL,
			http:     hs.Client(),
			agentID:  "ctrl-test",
			sessions: map[string]*Backend{},
		},
		agentID:     "ctrl-test",
		sessionID:   sessionID,
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
	}
}

type controlRequest struct {
	Method string
	Path   string
	Body   []byte
}

// rule is the test-side shape of one session-ruleset entry — the wire
// form a permission apply sends.
type rule struct {
	Permission string `json:"permission"`
	Pattern    string `json:"pattern"`
	Action     string `json:"action"`
}

// controlRecorder records every request hitting its test server and
// accumulates per-session permission rules the way opencode does
// (concatenation — there is no rule-removal API).
type controlRecorder struct {
	mu       sync.Mutex
	requests []controlRequest
	rules    map[string][]rule

	// sessionPatchStatus/sessionPatchBody, when status is non-zero, are
	// the response for PATCH /session/:id (set via failSessionPatches).
	// Guarded by mu.
	sessionPatchStatus int
	sessionPatchBody   string
}

// serveHTTP records the request and answers it like the opencode
// endpoints the backend talks to.
func (r *controlRecorder) serveHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	path := req.URL.Path
	isSessionPatch := req.Method == http.MethodPatch && sessionPatchTarget(path) != ""

	status := http.StatusOK
	var statusBody string
	r.mu.Lock()
	r.requests = append(r.requests, controlRequest{Method: req.Method, Path: path, Body: body})
	if isSessionPatch {
		var patch struct {
			Permission []rule `json:"permission"`
		}
		if err := json.Unmarshal(body, &patch); err == nil {
			sid := sessionPatchTarget(path)
			r.rules[sid] = append(r.rules[sid], patch.Permission...)
		}
		if r.sessionPatchStatus != 0 {
			status = r.sessionPatchStatus
			statusBody = r.sessionPatchBody
		}
	}
	n := len(r.requests)
	r.mu.Unlock()

	if req.Method == http.MethodPost && path == "/session" {
		// session.create → a distinct id per call.
		_, _ = fmt.Fprintf(w, `{"id":"sess-%d"}`, n)
		return
	}
	w.WriteHeader(status)
	if statusBody != "" {
		_, _ = w.Write([]byte(statusBody))
	}
}

// sessionPatchTarget extracts the session id from a /session/:id path
// without further segments, or "" when the path is not that shape.
func sessionPatchTarget(path string) string {
	rest, ok := strings.CutPrefix(path, "/session/")
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return ""
	}
	return rest
}

// allRequests returns a copy of every recorded request.
func (r *controlRecorder) allRequests() []controlRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]controlRequest, len(r.requests))
	copy(out, r.requests)
	return out
}

// lastSessionPatch returns the last PATCH recorded against the given
// opencode session id.
func (r *controlRecorder) lastSessionPatch(sessionID string) (controlRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.requests) - 1; i >= 0; i-- {
		if r.requests[i].Method == http.MethodPatch && r.requests[i].Path == "/session/"+sessionID {
			return r.requests[i], true
		}
	}
	return controlRequest{}, false
}

// assertNoConfigRequests fails the test if any request of any method
// hit /config — the server-wide permission lever that was never
// enforced, aborted every session's turns, and wrote config.json into
// the agent's workDir (#2330).
func (r *controlRecorder) assertNoConfigRequests(t testing.TB) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, req := range r.requests {
		if req.Path == "/config" {
			t.Errorf("recorded %s /config — permissions must be session-scoped, never server config", req.Method)
		}
	}
}

// seedRules pre-seeds a session's accumulated ruleset — the state a
// resumed session can carry from applies foci made earlier.
func (r *controlRecorder) seedRules(sessionID string, rules ...rule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules[sessionID] = append(r.rules[sessionID], rules...)
}

// failSessionPatches makes every PATCH /session/:id respond 500 with a
// diagnostic body — opencode answers a rejected apply (e.g. an invalid
// default_permission value → 400) with a validation message, and the
// apply error must carry that body to the operator.
func (r *controlRecorder) failSessionPatches() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionPatchStatus = http.StatusInternalServerError
	r.sessionPatchBody = "permission action must be allow, deny, or ask"
}

// evaluate resolves a permission ask the way opencode does over the
// accumulated rules: findLast — the LAST rule whose permission glob
// matches the tool AND whose pattern glob matches the target wins;
// "ask" when none matches.
func (r *controlRecorder) evaluate(sessionID, permission, target string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	action := "ask"
	for _, rl := range r.rules[sessionID] {
		if globMatch(rl.Permission, permission) && globMatch(rl.Pattern, target) {
			action = rl.Action
		}
	}
	return action
}

// globMatch reports whether s matches pattern under opencode's wildcard
// glob: "*" matches any run of characters, everything else is literal,
// anchored at both ends.
func globMatch(pattern, s string) bool {
	var re strings.Builder
	re.WriteString("^")
	for _, c := range pattern {
		if c == '*' {
			re.WriteString(".*")
		} else {
			re.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	re.WriteString("$")
	return regexp.MustCompile(re.String()).MatchString(s)
}

func (r *controlRecorder) lastAbort() (controlRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.requests) - 1; i >= 0; i-- {
		if r.requests[i].Path == "/session/sess-ctrl/abort" {
			return r.requests[i], true
		}
	}
	return controlRequest{}, false
}

// ---------------------------------------------------------------------------
// SendControl — SetModel
// ---------------------------------------------------------------------------

func TestSendControl_SetModel(t *testing.T) {
	// Verifies SetModelRequest validates and stores the model on the
	// Backend for inclusion in subsequent prompt bodies (per-message
	// "model" field). PATCH /config is documented but doesn't take
	// effect on opencode 1.17.x.
	b, _ := newControlTestBackend(t)
	b.resolveModelFn = func(_ context.Context, _, _, model string) (string, error) {
		return "zai-coding-plan/" + model, nil
	}

	if err := b.SendControl(context.Background(), &delegator.SetModelRequest{
		Model: "glm-5.2",
	}); err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	b.mu.Lock()
	model := b.model
	b.mu.Unlock()
	if model != "zai-coding-plan/glm-5.2" {
		t.Errorf("b.model = %q, want zai-coding-plan/glm-5.2", model)
	}
}

// ---------------------------------------------------------------------------
// SendControl — SetPermissionMode
// ---------------------------------------------------------------------------

func TestSendControl_SetPermissionMode(t *testing.T) {
	// Verifies SetPermissionModeRequest translates mapPermissionMode into
	// the SESSION-scoped ruleset opencode actually enforces: one PATCH
	// /session/:id carrying the mode's rules, with the "*" catch-all
	// FIRST and the specific categories after it, sorted — opencode
	// resolves findLast, so a specific rule must follow its own
	// catch-all. Nothing may touch /config any more (#2330).
	b, rec := newControlTestBackend(t)

	if err := b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{
		Mode: "acceptEdits",
	}); err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	patch, ok := rec.lastSessionPatch("sess-ctrl")
	if !ok {
		t.Fatal("PATCH /session/sess-ctrl was not sent")
	}
	var body struct {
		Permission []rule `json:"permission"`
	}
	if err := json.Unmarshal(patch.Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	want := []rule{
		{Permission: "*", Pattern: "*", Action: "ask"},
		{Permission: "bash", Pattern: "*", Action: "ask"},
		{Permission: "edit", Pattern: "*", Action: "allow"},
	}
	if !reflect.DeepEqual(body.Permission, want) {
		t.Errorf("permission rules = %+v, want %+v", body.Permission, want)
	}
	rec.assertNoConfigRequests(t)
}

func TestSendControl_SetPermissionMode_ScopedToOwnSession(t *testing.T) {
	// One opencode server serves every session of an agent, so /mode on
	// one Backend must send exactly one request, PATCH'd to ITS session —
	// nothing may touch the sibling session or the server-wide /config.
	hs, rec := newControlTestServer(t)
	a := newControlTestBackendForSession(t, hs, "sess-a")
	_ = newControlTestBackendForSession(t, hs, "sess-b") // the sibling sharing the server

	if err := a.SendControl(context.Background(), &delegator.SetPermissionModeRequest{
		Mode: "plan",
	}); err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	reqs := rec.allRequests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want exactly 1: %+v", len(reqs), reqs)
	}
	if reqs[0].Method != http.MethodPatch || reqs[0].Path != "/session/sess-a" {
		t.Errorf("request = %s %s, want PATCH /session/sess-a", reqs[0].Method, reqs[0].Path)
	}
	if _, ok := rec.lastSessionPatch("sess-b"); ok {
		t.Error("a request touched sibling session sess-b")
	}
	rec.assertNoConfigRequests(t)
}

func TestSendControl_SetPermissionMode_LatestModeWins(t *testing.T) {
	// opencode's session ruleset is append-only (findLast resolution),
	// so "the latest mode wins" has two halves: the second apply's body
	// is EXACTLY the new mode's rules (nothing carried over in the
	// request), and the ACCUMULATED ruleset still evaluates to the new
	// mode's outcome — the new batch's "*" rule shadows every rule of
	// the earlier batch.
	b, rec := newControlTestBackend(t)

	apply := func(mode string) {
		t.Helper()
		if err := b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{
			Mode: mode,
		}); err != nil {
			t.Fatalf("SendControl(%s): %v", mode, err)
		}
	}
	apply("acceptEdits")
	apply("plan")

	patch, ok := rec.lastSessionPatch("sess-ctrl")
	if !ok {
		t.Fatal("PATCH /session/sess-ctrl was not sent")
	}
	var body struct {
		Permission []rule `json:"permission"`
	}
	if err := json.Unmarshal(patch.Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	want := []rule{
		{Permission: "*", Pattern: "*", Action: "ask"},
		{Permission: "edit", Pattern: "*", Action: "deny"},
		{Permission: "read", Pattern: "*", Action: "allow"},
		{Permission: "read", Pattern: "*.env", Action: "ask"},
		{Permission: "read", Pattern: "*.env.*", Action: "ask"},
		{Permission: "read", Pattern: "*.env.example", Action: "allow"},
	}
	if !reflect.DeepEqual(body.Permission, want) {
		t.Errorf("last PATCH rules = %+v, want exactly plan's %+v", body.Permission, want)
	}

	for perm, wantAction := range map[string]string{
		"edit":  "deny",
		"read":  "allow",
		"bash":  "ask",
		"write": "ask",
	} {
		if got := rec.evaluate("sess-ctrl", perm, "src/main.go"); got != wantAction {
			t.Errorf("accumulated rules: %s → %s, want %s (no acceptEdits leftovers)", perm, got, wantAction)
		}
	}
	// plan keeps opencode's .env read guards (see
	// TestSendControl_SetPermissionMode_PlanKeepsEnvReadGuards).
	for _, tc := range []struct{ target, want string }{
		{".env", "ask"},
		{".env.local", "ask"},
		{".env.example", "allow"},
	} {
		if got := rec.evaluate("sess-ctrl", "read", tc.target); got != tc.want {
			t.Errorf("accumulated rules: read %s → %s, want %s", tc.target, got, tc.want)
		}
	}
}

func TestSendControl_SetPermissionMode_PlanKeepsEnvReadGuards(t *testing.T) {
	// opencode resolves a permission ask with findLast over
	// agent.permission ++ session.permission, and its agent defaults
	// guard .env reads ("*.env"/"*.env.*" ask, "*.env.example" allow —
	// opencode 1.17.15). plan allows the read category, so the session
	// batch must re-state those guards AFTER its {read,*,allow} rule:
	// without them the session rule is the last match and reading .env
	// passes with no prompt, putting secrets in the transcript (#2330
	// threat scan). The seed mirrors the agent defaults the live server
	// resolves before the session's own rules.
	b, rec := newControlTestBackend(t)
	rec.seedRules("sess-ctrl",
		rule{Permission: "read", Pattern: "*.env", Action: "ask"},
		rule{Permission: "read", Pattern: "*.env.*", Action: "ask"},
		rule{Permission: "read", Pattern: "*.env.example", Action: "allow"},
	)

	if err := b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{
		Mode: "plan",
	}); err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	if _, ok := rec.lastSessionPatch("sess-ctrl"); !ok {
		t.Fatal("PATCH /session/sess-ctrl was not sent")
	}
	for _, tc := range []struct{ perm, target, want string }{
		{"read", ".env", "ask"},           // guard re-asserted after read:allow
		{"read", ".env.local", "ask"},     // *.env.* guard
		{"read", ".env.example", "allow"}, // examples stay allowed, as in opencode's defaults
		{"read", "src/main.go", "allow"},  // plan's reads-are-free kept
		{"edit", "src/main.go", "deny"},   // plan's edit deny kept
	} {
		if got := rec.evaluate("sess-ctrl", tc.perm, tc.target); got != tc.want {
			t.Errorf("evaluate(%s, %q) = %s, want %s", tc.perm, tc.target, got, tc.want)
		}
	}
}

func TestSendControl_SetPermissionMode_AllowAllCarriesNoEnvGuards(t *testing.T) {
	// Pins the documented decision (#2330 threat scan): modes whose
	// whole meaning is "allow everything" (bypassPermissions, auto)
	// deliberately drop opencode's .env read guards — the user asked
	// for no prompts. Their ruleset stays one {*,*,allow} rule so a
	// later mode can still supersede it under findLast.
	for _, mode := range []string{"bypassPermissions", "auto"} {
		t.Run(mode, func(t *testing.T) {
			b, rec := newControlTestBackend(t)

			if err := b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{
				Mode: mode,
			}); err != nil {
				t.Fatalf("SendControl: %v", err)
			}

			patch, ok := rec.lastSessionPatch("sess-ctrl")
			if !ok {
				t.Fatal("PATCH /session/sess-ctrl was not sent")
			}
			var body struct {
				Permission []rule `json:"permission"`
			}
			if err := json.Unmarshal(patch.Body, &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			want := []rule{{Permission: "*", Pattern: "*", Action: "allow"}}
			if !reflect.DeepEqual(body.Permission, want) {
				t.Errorf("%s rules = %+v, want exactly %+v (no guards)", mode, body.Permission, want)
			}
			if got := rec.evaluate("sess-ctrl", "read", ".env"); got != "allow" {
				t.Errorf("%s: read .env → %s, want allow (explicit allow-everything)", mode, got)
			}
		})
	}
}

func TestBackend_SessionRequestsEscapeTheSessionID(t *testing.T) {
	// A session id containing "/" (e.g. from a corrupted
	// ResumeSessionID) must not extend the request path to another
	// server resource: /session/:id URLs are built through sessionURL,
	// which path-escapes the id (#2330 threat scan — a PATCH meant for
	// one session must never be routable to /config). Observable at the
	// server as the ESCAPED request path.
	var mu sync.Mutex
	var paths []string
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.EscapedPath())
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hs.Close)
	b := newControlTestBackendForSession(t, hs, "ses/sion")

	if err := b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{
		Mode: "default",
	}); err != nil {
		t.Fatalf("SendControl: %v", err)
	}
	if err := b.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"PATCH /session/ses%2Fsion",
		"POST /session/ses%2Fsion/abort",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("requests = %v, want %v (session id must arrive escaped)", paths, want)
	}
}

func TestSendControl_SetPermissionMode_ErrorBodyIsBounded(t *testing.T) {
	// The apply error carries opencode's response body so a rejection
	// is self-explanatory, but the read is bounded (maxErrorBodyBytes):
	// a misbehaving local server returning a huge error body must not
	// balloon the returned error, the WARN at Start, or foci's memory
	// (#2330 threat scan).
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("x", 1<<20)) // 1 MiB, far above the cap
	}))
	t.Cleanup(hs.Close)
	b := newControlTestBackendForSession(t, hs, "sess-ctrl")

	err := b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{
		Mode: "plan",
	})
	if err == nil {
		t.Fatal("SendControl should error when the session PATCH fails")
	}
	if !strings.Contains(err.Error(), "PATCH /session/sess-ctrl: HTTP 500") {
		t.Errorf("error = %q, want it to name PATCH /session/sess-ctrl: HTTP 500", err)
	}
	// 16 KiB is generously above the 8 KiB cap + the message prefix;
	// an unbounded read would carry the full 1 MiB body.
	if n := len(err.Error()); n > 16*1024 {
		t.Errorf("error message is %d bytes — the error body read is not bounded", n)
	}
}

func TestSendControl_SetPermissionMode_WinsOverExistingRules(t *testing.T) {
	// A resumed session may still carry rules foci set on it before
	// (a previous /mode, a previous Start). The apply must outrank ALL
	// of them, including specific-pattern rules: the new batch's
	// {*,*} rule sorts after every earlier rule under findLast.
	b, rec := newControlTestBackend(t)
	rec.seedRules("sess-ctrl",
		rule{Permission: "bash", Pattern: "git status", Action: "allow"},
		rule{Permission: "edit", Pattern: "*", Action: "deny"},
	)

	if err := b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{
		Mode: "plan",
	}); err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	for _, tc := range []struct{ perm, target, want string }{
		{"bash", "git status", "ask"}, // new {*,*} outranks the seeded specific rule
		{"edit", "src/main.go", "deny"},
		{"read", "src/main.go", "allow"},
	} {
		if got := rec.evaluate("sess-ctrl", tc.perm, tc.target); got != tc.want {
			t.Errorf("evaluate(%s, %q) = %s, want %s", tc.perm, tc.target, got, tc.want)
		}
	}
}

func TestSendControl_SetPermissionMode_Guards(t *testing.T) {
	// Pins the guard shape: no server or no sessionID → error to the
	// caller (a failed /mode apply must surface, not silently pass).
	t.Run("no server", func(t *testing.T) {
		b := &Backend{sessionID: "sess-x", outstanding: delegator.NewOutstandingRegistry()}
		if err := b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{
			Mode: "plan",
		}); err == nil {
			t.Error("expected error with no server")
		}
	})
	t.Run("empty sessionID", func(t *testing.T) {
		hs, _ := newControlTestServer(t)
		b := newControlTestBackendForSession(t, hs, "")
		if err := b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{
			Mode: "plan",
		}); err == nil {
			t.Error("expected error with empty sessionID")
		}
	})
}

func TestSendControl_SetPermissionMode_FailedPatchReturnsError(t *testing.T) {
	// A failed /mode apply must return an error to the caller — not
	// silently succeed by falling back to some other endpoint. Both
	// failure shapes must NAME the session-scoped apply (PATCH
	// /session/<the real id>), so the message can never read as the old
	// server-wide PATCH /config wording (#2330), and the HTTP arm must
	// also carry opencode's response body so a rejected apply (e.g. an
	// invalid mode value) is self-explanatory.
	t.Run("HTTP error status", func(t *testing.T) {
		hs, rec := newControlTestServer(t)
		b := newControlTestBackendForSession(t, hs, "sess-ctrl")
		rec.failSessionPatches()

		err := b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{
			Mode: "plan",
		})
		if err == nil {
			t.Fatal("SendControl should error when the session PATCH fails")
		}
		for _, want := range []string{
			"PATCH /session/sess-ctrl: HTTP 500",
			"permission action must be allow, deny, or ask",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
		}
	})
	t.Run("transport failure", func(t *testing.T) {
		// Nothing listens behind baseURL any more: the request itself
		// fails, and the wrap must still name the endpoint and session
		// it failed on.
		hs, _ := newControlTestServer(t)
		b := newControlTestBackendForSession(t, hs, "sess-ctrl")
		hs.Close()

		err := b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{
			Mode: "plan",
		})
		if err == nil {
			t.Fatal("SendControl should error when the session PATCH cannot be sent")
		}
		if !strings.HasPrefix(err.Error(), "PATCH /session/sess-ctrl: ") {
			t.Errorf("error = %q, want it to name PATCH /session/sess-ctrl before the cause", err)
		}
	})
}

// ---------------------------------------------------------------------------
// SendControl — ApplyFlagSettings (effort) is a no-op
// ---------------------------------------------------------------------------

func TestSendControl_SetEffort_NoOpReturns(t *testing.T) {
	// Verifies ApplyFlagSettingsRequest (used for /effort) is a no-op
	// for opencode — returns nil without sending any HTTP request.
	b, rec := newControlTestBackend(t)

	if err := b.SendControl(context.Background(), &delegator.ApplyFlagSettingsRequest{
		Settings: map[string]any{"effortLevel": "high"},
	}); err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	if len(rec.requests) != 0 {
		t.Errorf("effort no-op should not send any requests; got %d", len(rec.requests))
	}
}

// ---------------------------------------------------------------------------
// Interrupt — POST /session/:id/abort
// ---------------------------------------------------------------------------

func TestInterrupt_PostsAbort(t *testing.T) {
	// Verifies Interrupt POSTs to /session/:id/abort.
	b, rec := newControlTestBackend(t)

	if err := b.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	abort, ok := rec.lastAbort()
	if !ok {
		t.Fatal("POST /session/:id/abort was not sent")
	}
	if abort.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", abort.Method)
	}
}

func TestInterrupt_BeforeStart(t *testing.T) {
	// Verifies Interrupt on a never-Started Backend returns an error
	// rather than panicking.
	b := &Backend{}
	err := b.Interrupt(context.Background())
	if err == nil {
		t.Error("Interrupt before Start should error")
	}
}

// ---------------------------------------------------------------------------
// CompactionWait — already tested in Step 6's TestArmCompactionWait_ContextCancellation.
// Add the "fires on session.compacted" test here.
// ---------------------------------------------------------------------------

func TestCompactionWait_FiresOnSessionCompacted(t *testing.T) {
	// Verifies WaitForCompaction unblocks when onSessionCompacted fires.
	b := &Backend{
		sessionID:     "sess-test",
		compactDoneCh: make(chan struct{}, 1),
		outstanding:   delegator.NewOutstandingRegistry(),
	}
	// Arm + fire.
	b.ArmCompactionWait()
	b.onSessionCompacted("sess-test")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.WaitForCompaction(ctx); err != nil {
		t.Errorf("WaitForCompaction: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CompactionStartWait — real start signal (compaction part)
// ---------------------------------------------------------------------------

func TestCompactionStartWait_BlocksUntilCompactionPart(t *testing.T) {
	// Verifies ArmCompactionStartWait arms (but does NOT fire) the start
	// waiter: WaitForCompactionStart blocks until the compaction part
	// arrives via handleCompactionPart — opencode's real start signal,
	// emitted at summarize initiation.
	b := &Backend{
		sessionID:   "sess-test",
		outstanding: delegator.NewOutstandingRegistry(),
	}

	b.ArmCompactionStartWait()

	// Armed but no compaction part yet → must block, not return immediately.
	blockedCtx, blockedCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer blockedCancel()
	if err := b.WaitForCompactionStart(blockedCtx); err == nil {
		t.Fatal("WaitForCompactionStart returned before compaction part arrived (expected to block until the start signal)")
	}

	// Compaction part arrives → start signal fires.
	b.handleCompactionPart()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.WaitForCompactionStart(ctx); err != nil {
		t.Errorf("WaitForCompactionStart after compaction part: %v", err)
	}
}

func TestCompactionStartWait_NotArmedReturnsNil(t *testing.T) {
	// Verifies WaitForCompactionStart without arming returns nil
	// (matching WaitForCompaction's no-arm contract).
	b := &Backend{
		sessionID:     "sess-test",
		compactDoneCh: make(chan struct{}, 1),
		outstanding:   delegator.NewOutstandingRegistry(),
	}

	if err := b.WaitForCompactionStart(context.Background()); err != nil {
		t.Errorf("WaitForCompactionStart (unarmed): %v", err)
	}
}

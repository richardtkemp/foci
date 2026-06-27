package app

// Security / attack-surface tests for the internet-exposed app-provider (FAP)
// endpoints — /app/ws, /app/blob[/<id>], /app/pair, /app/pair/revoke,
// /app/devices, /app/push/register, /app/history, /app/avatar/<id>.
//
// These are the only foci endpoints intended to be reachable from outside the
// LAN (the app connects from anywhere). They self-authenticate (Bearer
// app.api_key master key OR a per-device token), bypassing the outer
// http.api_key middleware. This file exercises the defenses an attacker would
// probe: unauthenticated access, credential brute force, master-vs-device
// privilege separation, token revocation, path traversal on the file-serving
// routes, HTTP method abuse, malformed/oversized request bodies, the
// allowed_devices allowlist, and hostile WebSocket frames.
//
// Each test asserts the SERVER DEFENDS CORRECTLY. Where current behaviour is a
// known gap rather than a defense, the test name ends in _FINDING and a comment
// points at docs (the audit writeup) — it characterises today's behaviour so a
// future hardening flips it red→green deliberately.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"foci/internal/config"
	"foci/internal/platform"
)

// secHub returns a hub with a known master key and one paired device, plus that
// device's token, for the auth-matrix tests.
func secHub(t *testing.T) (*Hub, string) {
	t.Helper()
	h := newTestHub()
	h.apiKey = "master-secret"
	d := h.devices.pair("phone-1", "Phone")
	return h, d.Token
}

// secReq builds an HTTP request to an /app/* path with an optional raw
// Authorization header value (use "" to omit the header entirely).
func secReq(method, path, body, authHeader string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	return r
}

// endpoint describes one REST handler and a representative request shape.
type endpoint struct {
	name    string
	method  string
	path    string
	body    string
	handler func(http.ResponseWriter, *http.Request)
}

func restEndpoints(h *Hub) []endpoint {
	return []endpoint{
		{"pair", http.MethodPost, "/app/pair", `{"deviceId":"d"}`, h.ServePair},
		{"revoke", http.MethodPost, "/app/pair/revoke", `{"deviceId":"d"}`, h.ServeRevoke},
		{"devices", http.MethodGet, "/app/devices", "", h.ServeDevices},
		{"push-register", http.MethodPost, "/app/push/register", `{"deviceId":"d","pushToken":"t"}`, h.ServePushRegister},
		{"history", http.MethodGet, "/app/history?conversationId=c", "", h.ServeHistory},
		{"replay", http.MethodGet, "/app/replay?conversationId=c", "", h.ServeReplay},
		{"blob-post", http.MethodPost, "/app/blob", "x", h.ServeBlobPost},
		{"blob-get", http.MethodGet, "/app/blob/abc", "", h.ServeBlobGet},
		{"avatar", http.MethodGet, "/app/avatar/clutch", "", h.ServeAvatar},
		{"ws", http.MethodGet, "/app/ws", "", h.ServeWS},
	}
}

// 1. No credential and a garbage credential are rejected on EVERY endpoint, and
//    a non-Bearer scheme is treated as no credential (not parsed as a token).
func TestSecurity_AuthMatrix_RejectsUnauthenticated(t *testing.T) {
	h, _ := secHub(t)
	for _, e := range restEndpoints(h) {
		// Per-endpoint auth semantics only — reset the brute-force limiter so the
		// shared per-IP bucket doesn't trip (429) partway through enumeration as
		// the endpoint list grows. Cumulative lockout is covered by test 5.
		h.authLim = newAuthLimiter(authFailMax, authFailWindow)

		// No Authorization header → 401.
		w := httptest.NewRecorder()
		e.handler(w, secReq(e.method, e.path, e.body, ""))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: no-cred code = %d, want 401", e.name, w.Code)
		}

		// Garbage bearer → 403.
		w = httptest.NewRecorder()
		e.handler(w, secReq(e.method, e.path, e.body, "Bearer not-the-key"))
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: garbage-bearer code = %d, want 403", e.name, w.Code)
		}

		// Non-Bearer scheme (Basic) is not a token → treated as missing → 401.
		w = httptest.NewRecorder()
		e.handler(w, secReq(e.method, e.path, e.body, "Basic bWFzdGVyOng="))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: basic-auth code = %d, want 401 (Bearer-only)", e.name, w.Code)
		}
	}
}

// 2. Management endpoints (pair / revoke / devices) require the MASTER key. A
//    valid device token authenticates for use endpoints but must NOT be able to
//    mint, revoke, or enumerate devices — privilege escalation guard.
func TestSecurity_MasterOnlyEndpoints_RejectValidDeviceToken(t *testing.T) {
	h, devTok := secHub(t)
	masterOnly := []endpoint{
		{"pair", http.MethodPost, "/app/pair", `{"deviceId":"d2"}`, h.ServePair},
		{"revoke", http.MethodPost, "/app/pair/revoke", `{"deviceId":"phone-1"}`, h.ServeRevoke},
		{"devices", http.MethodGet, "/app/devices", "", h.ServeDevices},
	}
	for _, e := range masterOnly {
		w := httptest.NewRecorder()
		e.handler(w, secReq(e.method, e.path, e.body, "Bearer "+devTok))
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: device-token code = %d, want 403 (master-only)", e.name, w.Code)
		}
	}
}

// 3. The use endpoints (history / push-register / blob) DO accept a valid device
//    token — confirming the auth split lets a paired device work without the
//    master key.
func TestSecurity_UseEndpoints_AcceptDeviceToken(t *testing.T) {
	h, devTok := secHub(t)
	use := []endpoint{
		{"history", http.MethodGet, "/app/history?conversationId=c", "", h.ServeHistory},
		{"replay", http.MethodGet, "/app/replay?conversationId=c", "", h.ServeReplay},
		{"push-register", http.MethodPost, "/app/push/register", `{"pushToken":"t"}`, h.ServePushRegister},
		{"blob-post", http.MethodPost, "/app/blob", "x", h.ServeBlobPost},
	}
	for _, e := range use {
		w := httptest.NewRecorder()
		e.handler(w, secReq(e.method, e.path, e.body, "Bearer "+devTok))
		if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
			t.Errorf("%s: valid device token rejected with %d", e.name, w.Code)
		}
	}
}

// 4. A revoked device token is rejected immediately. The same token that worked
//    before revocation must 403 after.
func TestSecurity_RevokedDeviceToken_Rejected(t *testing.T) {
	h, devTok := secHub(t)

	// Works before revoke.
	w := httptest.NewRecorder()
	h.ServeHistory(w, secReq(http.MethodGet, "/app/history?conversationId=c", "", "Bearer "+devTok))
	if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
		t.Fatalf("pre-revoke history code = %d, expected accepted", w.Code)
	}

	if _, ok := h.devices.revoke("phone-1"); !ok {
		t.Fatal("revoke failed")
	}

	// Rejected after revoke.
	w = httptest.NewRecorder()
	h.ServeHistory(w, secReq(http.MethodGet, "/app/history?conversationId=c", "", "Bearer "+devTok))
	if w.Code != http.StatusForbidden {
		t.Errorf("post-revoke history code = %d, want 403", w.Code)
	}
}

// 5. Per-IP brute-force lockout: after authFailMax wrong-credential attempts from
//    one IP, further attempts are 429 — and even a CORRECT master key is locked
//    out for the window (the block check precedes the token check).
func TestSecurity_AuthLimiter_LocksOutBruteForce(t *testing.T) {
	h, _ := secHub(t)
	const ip = "203.0.113.7:5000"

	for i := 0; i < authFailMax; i++ {
		w := httptest.NewRecorder()
		r := secReq(http.MethodGet, "/app/history?conversationId=c", "", "Bearer wrong")
		r.RemoteAddr = ip
		h.ServeHistory(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("attempt %d code = %d, want 403", i, w.Code)
		}
	}

	// Next wrong attempt is now blocked with 429.
	w := httptest.NewRecorder()
	r := secReq(http.MethodGet, "/app/history?conversationId=c", "", "Bearer wrong")
	r.RemoteAddr = ip
	h.ServeHistory(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("blocked attempt code = %d, want 429", w.Code)
	}

	// Even the correct master key is refused while the IP is locked out.
	w = httptest.NewRecorder()
	r = secReq(http.MethodGet, "/app/history?conversationId=c", "", "Bearer master-secret")
	r.RemoteAddr = ip
	h.ServeHistory(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("master key while locked out code = %d, want 429", w.Code)
	}
}

// 6. Lockout is per-IP: a different source IP is unaffected by another IP's
//    failures.
func TestSecurity_AuthLimiter_PerIPIsolation(t *testing.T) {
	h, _ := secHub(t)
	const bad = "198.51.100.1:5000"
	for i := 0; i < authFailMax+2; i++ {
		w := httptest.NewRecorder()
		r := secReq(http.MethodGet, "/app/history?conversationId=c", "", "Bearer wrong")
		r.RemoteAddr = bad
		h.ServeHistory(w, r)
	}
	// A fresh IP with the correct key is still served (not 429).
	w := httptest.NewRecorder()
	r := secReq(http.MethodGet, "/app/history?conversationId=c", "", "Bearer master-secret")
	r.RemoteAddr = "198.51.100.250:5000"
	h.ServeHistory(w, r)
	if w.Code == http.StatusTooManyRequests {
		t.Errorf("unrelated IP was locked out (code 429) — lockout not per-IP")
	}
}

// 7. EXPLOIT: an attacker on one real socket rotates X-Forwarded-For every
//    request, trying to land in a fresh per-IP lockout bucket each time and so
//    brute-force the master key without ever tripping the throttle. Traefik
//    appends the real downstream socket to the RIGHT of XFF, so foci keys the
//    limiter on that rightmost hop — the attacker's spoofed leftmost values are
//    ignored and the lockout still fires. The test runs the exploit and FAILS if
//    it succeeds (i.e. if the limiter never blocks). See F1 in the audit doc.
func TestSecurity_AuthLimiter_XFFRotationDoesNotBypass(t *testing.T) {
	h, _ := secHub(t)
	const realHop = "203.0.113.9" // what Traefik appends for this attacker's socket
	blocked := false
	for i := 0; i < authFailMax*3; i++ {
		w := httptest.NewRecorder()
		r := secReq(http.MethodGet, "/app/history?conversationId=c", "", "Bearer wrong")
		r.RemoteAddr = realHop + ":5000"
		// Attacker spoofs a different leftmost IP each time; Traefik appends the
		// real hop on the right. A correct limiter keys on the rightmost value.
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("10.0.0.%d, %s", i, realHop))
		h.ServeHistory(w, r)
		if w.Code == http.StatusTooManyRequests {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Fatalf("XFF rotation evaded the rate limiter over %d attempts — exploit succeeded; remoteIP must key on the trusted rightmost hop", authFailMax*3)
	}
}

// 8. Path-traversal on the blob download route is rejected: any id containing a
//    slash (the only path separator after TrimPrefix) is a 400, and the served
//    path is looked up from in-memory metadata keyed by a server-minted ULID —
//    never built from the URL — so "../" cannot escape the blob dir. An unknown
//    but well-formed id is a clean 404, not a file probe.
func TestSecurity_BlobGet_PathTraversalRejected(t *testing.T) {
	h, _ := secHub(t)
	bad := []string{
		"/app/blob/../etc/passwd",
		"/app/blob/..%2f..%2fetc%2fpasswd", // httptest does not decode; literal still contains %2f, not "/", but the path has no slash → 404 not 400; see note
		"/app/blob/a/b",
		"/app/blob/",
	}
	for _, p := range bad {
		w := httptest.NewRecorder()
		h.ServeBlobGet(w, secReq(http.MethodGet, p, "", "Bearer master-secret"))
		// Either a 400 (slash/empty) or 404 (unknown id) — never 200, never a
		// file outside the store.
		if w.Code == http.StatusOK {
			t.Errorf("blob get %q returned 200 — must be rejected", p)
		}
	}
	// A well-formed unknown id → 404 (no traversal, no info leak beyond "absent").
	w := httptest.NewRecorder()
	h.ServeBlobGet(w, secReq(http.MethodGet, "/app/blob/01ABCDEF01ABCDEF01ABCDEF01", "", "Bearer master-secret"))
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown blob id code = %d, want 404", w.Code)
	}
}

// 9. The avatar route cannot be coerced into reading an arbitrary file: a path
//    with a slash is a 400, and any id is resolved ONLY against configured agent
//    IDs (agentAvatarPath returns a config-declared path or ""), so an unknown
//    or traversal-shaped id yields 404 — the URL never reaches the filesystem.
func TestSecurity_Avatar_NoArbitraryFileRead(t *testing.T) {
	h := hubWithAvatar(t, "clutch", "/etc/hostname") // real agent, but...
	for _, p := range []string{
		"/app/avatar/../../etc/passwd", // contains slash → 400
		"/app/avatar/a/b",              // contains slash → 400
		"/app/avatar/..",               // no slash, but not an agent id → 404
		"/app/avatar/nonexistent-agent",
	} {
		w := httptest.NewRecorder()
		h.ServeAvatar(w, secReq(http.MethodGet, p, "", "Bearer secret-key"))
		if w.Code == http.StatusOK {
			t.Errorf("avatar %q returned 200 — arbitrary path served", p)
		}
	}
}

// 10. HTTP method abuse: each handler enforces its verb AFTER auth. Wrong verbs
//     get 405, not silent action.
func TestSecurity_MethodEnforcement(t *testing.T) {
	h, _ := secHub(t)
	cases := []struct {
		name    string
		method  string
		path    string
		body    string
		handler func(http.ResponseWriter, *http.Request)
		want    int
	}{
		{"GET-pair", http.MethodGet, "/app/pair", "", h.ServePair, http.StatusMethodNotAllowed},
		{"GET-revoke", http.MethodGet, "/app/pair/revoke", "", h.ServeRevoke, http.StatusMethodNotAllowed},
		{"POST-history", http.MethodPost, "/app/history?conversationId=c", "", h.ServeHistory, http.StatusMethodNotAllowed},
		{"POST-replay", http.MethodPost, "/app/replay?conversationId=c", "", h.ServeReplay, http.StatusMethodNotAllowed},
		{"POST-pushreg", http.MethodGet, "/app/push/register", "", h.ServePushRegister, http.StatusMethodNotAllowed},
		{"DELETE-blob", http.MethodDelete, "/app/blob", "x", h.ServeBlobPost, http.StatusMethodNotAllowed},
		{"POST-avatar", http.MethodPost, "/app/avatar/clutch", "", h.ServeAvatar, http.StatusMethodNotAllowed},
		{"POST-devices", http.MethodPost, "/app/devices", "", h.ServeDevices, http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		c.handler(w, secReq(c.method, c.path, c.body, "Bearer master-secret"))
		if w.Code != c.want {
			t.Errorf("%s: code = %d, want %d", c.name, w.Code, c.want)
		}
	}
}

// 11. Malformed and oversized request bodies are rejected with 400 (not a panic,
//     not a partial action). Covers invalid JSON, a missing required field, and a
//     body past the 64KB MaxBytesReader cap on the pair/revoke/push handlers.
func TestSecurity_MalformedAndOversizedBodies(t *testing.T) {
	h, _ := secHub(t)
	const master = "Bearer master-secret"

	// Invalid JSON → 400.
	w := httptest.NewRecorder()
	h.ServePair(w, secReq(http.MethodPost, "/app/pair", `{not json`, master))
	if w.Code != http.StatusBadRequest {
		t.Errorf("pair invalid-json code = %d, want 400", w.Code)
	}

	// Missing required deviceId → 400.
	w = httptest.NewRecorder()
	h.ServePair(w, secReq(http.MethodPost, "/app/pair", `{"label":"x"}`, master))
	if w.Code != http.StatusBadRequest {
		t.Errorf("pair empty-deviceId code = %d, want 400", w.Code)
	}

	// Oversized body (> 64KB) → MaxBytesReader trips → 400.
	huge := `{"deviceId":"` + strings.Repeat("a", 70000) + `"}`
	w = httptest.NewRecorder()
	h.ServePair(w, secReq(http.MethodPost, "/app/pair", huge, master))
	if w.Code != http.StatusBadRequest {
		t.Errorf("pair oversized-body code = %d, want 400", w.Code)
	}

	// Revoke unknown device → 404.
	w = httptest.NewRecorder()
	h.ServeRevoke(w, secReq(http.MethodPost, "/app/pair/revoke", `{"deviceId":"ghost"}`, master))
	if w.Code != http.StatusNotFound {
		t.Errorf("revoke unknown code = %d, want 404", w.Code)
	}

	// push/register with no pushToken → 400.
	w = httptest.NewRecorder()
	h.ServePushRegister(w, secReq(http.MethodPost, "/app/push/register", `{"deviceId":"d"}`, master))
	if w.Code != http.StatusBadRequest {
		t.Errorf("push-register no-token code = %d, want 400", w.Code)
	}
}

// 12. The allowed_devices allowlist (when configured) is enforced at pairing: a
//     device id not on the list cannot be paired even with the master key.
func TestSecurity_AllowedDevicesAllowlist(t *testing.T) {
	h, _ := secHub(t)
	h.allowedDevices = map[string]bool{"trusted-phone": true}

	// Disallowed id → 403.
	w := httptest.NewRecorder()
	h.ServePair(w, secReq(http.MethodPost, "/app/pair", `{"deviceId":"attacker-phone"}`, "Bearer master-secret"))
	if w.Code != http.StatusForbidden {
		t.Errorf("disallowed device pair code = %d, want 403", w.Code)
	}

	// Allowed id → 200.
	w = httptest.NewRecorder()
	h.ServePair(w, secReq(http.MethodPost, "/app/pair", `{"deviceId":"trusted-phone"}`, "Bearer master-secret"))
	if w.Code != http.StatusOK {
		t.Errorf("allowed device pair code = %d, want 200", w.Code)
	}
}

// 13. Blob upload enforces the size cap: a body past the limit is rejected 413,
//     not written to disk unbounded.
func TestSecurity_BlobUpload_SizeCapEnforced(t *testing.T) {
	h, _ := secHub(t)
	h.blobs.maxBytes = 16 // tiny cap for the test
	w := httptest.NewRecorder()
	h.ServeBlobPost(w, secReq(http.MethodPost, "/app/blob", strings.Repeat("A", 1024), "Bearer master-secret"))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized blob code = %d, want 413", w.Code)
	}
}

// 14. Hostile WebSocket frames after a connection is established must not panic
//     the read loop or produce spurious output: malformed JSON, an unknown frame
//     type, and an empty frame are all dropped silently.
func TestSecurity_WSDispatch_SurvivesHostileFrames(t *testing.T) {
	h := newTestHub()
	c := fakeClient()
	c.hub = h
	hostile := []string{
		`{not json at all`,
		`{"t":"totally-unknown-type","id":"x","d":{}}`,
		``,
		`[]`,
		`{"t":"command"}`, // command with no conversationId/name → no-op, no panic
		`{"t":"interactive.response","id":"x","d":{"promptId":"nope","data":"nope:0"}}`,
	}
	for _, frame := range hostile {
		// Must not panic.
		h.dispatchInbound(c, []byte(frame))
	}
	if got := drain(t, c); len(got) != 0 {
		t.Errorf("hostile frames produced %d output frames, want 0: %v", len(got), types(got))
	}
}

// 15. End-to-end WebSocket Bearer gate through the real HTTP handler + gorilla
//     upgrade: a valid master key upgrades to 101; no credential is 401 and a
//     wrong credential is 403 — both BEFORE the upgrade, so an unauthenticated
//     client never reaches the frame loop.
func TestSecurity_WSBearerGate_EndToEnd(t *testing.T) {
	h := newTestHub()
	h.apiKey = "master-secret"
	h.deps = platform.ProviderDeps{Config: &config.Config{}}
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })

	mux := http.NewServeMux()
	mux.HandleFunc("/app/ws", WSHandler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/app/ws"

	// No credential → handshake fails with 401.
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Error("dial with no credential succeeded — auth gate bypassed")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-credential handshake status = %v, want 401", statusOf(resp))
	}

	// Wrong credential → 403.
	_, resp, err = websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer wrong"}})
	if err == nil {
		t.Error("dial with wrong credential succeeded — auth gate bypassed")
	} else if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong-credential handshake status = %v, want 403", statusOf(resp))
	}

	// Correct master key → 101 Switching Protocols, connection established.
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer master-secret"}})
	if err != nil {
		t.Fatalf("dial with master key failed: %v (status %v)", err, statusOf(resp))
	}
	defer func() { _ = conn.Close() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("master-key handshake status = %d, want 101", resp.StatusCode)
	}
}

func statusOf(r *http.Response) any {
	if r == nil {
		return "no response"
	}
	return r.StatusCode
}

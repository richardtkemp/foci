package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	flog "foci/internal/log"
	"foci/internal/memory"
	"foci/internal/tools"
)

// newTodoRouteDeps builds the HTTP test harness with ONE todo store shared
// by both agents (the default test-agent plus a second agent "other" — bare
// instance, since /todo/done touches only id and cc.TodoStore). Ids are per
// agent even in a shared store (PRIMARY KEY (agent_id, id)), so both agents
// holding todo #1 is the normal shape here — and the strongest one for the
// per-agent predicate: a route that scoped by id alone would be caught by
// every closing test.
func newTodoRouteDeps(t *testing.T) (httpHandlerDeps, *memory.TodoStore) {
	t.Helper()
	d, _ := httpTestSetup(t, httpTestOpts{})
	other := &agentInstance{id: "other"}
	d.agents["other"] = other
	d.agentOrder = append(d.agentOrder, "other")

	store, err := memory.NewTodoStore(filepath.Join(t.TempDir(), "todo.db"))
	if err != nil {
		t.Fatalf("NewTodoStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, id := range d.agentOrder {
		d.agents[id].cc.TodoStore = store
	}
	return d, store
}

// snapshotTodos renders every agent's items of the shared store, to prove a
// refused request changed nothing — statuses, reasons and timestamps
// included. Read errors render as nothing; a closed store reads as empty
// before and after alike.
func snapshotTodos(store *memory.TodoStore) string {
	var b strings.Builder
	for _, id := range []string{testAgentID, "other"} {
		items, err := store.List(id, "", nil, "", "", false, 0)
		fmt.Fprintf(&b, "%s: err=%v items=%+v\n", id, err, items)
	}
	return b.String()
}

// postTodoDone posts a /todo/done request body to the mux, with the reason
// carried as bytes exactly as the CLI sends it.
func postTodoDone(mux *http.ServeMux, agent string, id int64, reason string) *httptest.ResponseRecorder {
	body, err := json.Marshal(todoDoneRequest{Agent: agent, ID: id, Reason: []byte(reason)})
	if err != nil {
		panic(err)
	}
	return postJSON(mux, "/todo/done", string(body))
}

// TestTodoDone_ClosesLikeTheTool proves requirement 1: the route closes a
// todo exactly as the agent todo tool's complete action does with the same
// reason — status done, completed_at set, close_reason stored — through the
// same store writer, and the tool's get then displays the reason.
func TestTodoDone_ClosesLikeTheTool(t *testing.T) {
	t.Parallel()
	d, store := newTodoRouteDeps(t)
	mux := newTestMux(d)
	reason := "landed 3fc608953 (factory run 01M4SWKGRFGSMXBHKPKH3E6VEK)"

	idA, _ := store.Add(testAgentID, "closed by the tool", "medium", "")
	idB, _ := store.Add(testAgentID, "closed by the route", "medium", "")

	// A: the agent todo tool's complete action with the reason.
	tool := tools.NewTodoTool(store, testAgentID)
	toolParams, _ := json.Marshal(map[string]any{"action": "complete", "id": idA, "reason": reason})
	if _, err := tool.Execute(context.Background(), toolParams); err != nil {
		t.Fatalf("todo tool complete: %v", err)
	}

	// B: the route, with the same reason.
	w := postTodoDone(mux, testAgentID, idB, reason)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if want := fmt.Sprintf("#%d: done", idB); resp["response"] != want {
		t.Errorf("response = %q, want %q", resp["response"], want)
	}

	a, errA := store.Get(testAgentID, idA)
	b, errB := store.Get(testAgentID, idB)
	if errA != nil || errB != nil {
		t.Fatalf("Get: %v / %v", errA, errB)
	}
	if b.Status != "done" || b.CompletedAt == nil || b.CloseReason != reason {
		t.Errorf("route-closed item = {%s, completed=%v, reason=%q}, want done with the reason and completed_at set", b.Status, b.CompletedAt, b.CloseReason)
	}
	if b.Status != a.Status || b.CloseReason != a.CloseReason || (b.CompletedAt == nil) != (a.CompletedAt == nil) {
		t.Errorf("route result {%s, %v, %q} differs from the tool's {%s, %v, %q}", b.Status, b.CompletedAt, b.CloseReason, a.Status, a.CompletedAt, a.CloseReason)
	}

	// The reason is what the tool's get displays for the route-closed item.
	getParams, _ := json.Marshal(map[string]any{"action": "get", "id": idB})
	res, err := tool.Execute(context.Background(), getParams)
	if err != nil {
		t.Fatalf("todo tool get: %v", err)
	}
	if !strings.Contains(res.Text, reason) {
		t.Errorf("todo tool get output = %q, want it to show the reason %q", res.Text, reason)
	}
}

// TestTodoDone_ReasonVerbatim proves requirement 2: the reason is stored byte
// for byte — the spec's two examples plus a leading dash, flag look-alikes,
// double spaces, a trailing space, an embedded newline, unicode, a NUL and
// bytes that are not valid UTF-8 (carried as base64 on the wire).
func TestTodoDone_ReasonVerbatim(t *testing.T) {
	t.Parallel()
	reasons := []string{
		"landed 3fc608953 (factory run 01M4SWKGRFGSMXBHKPKH3E6VEK)",
		"landed abc (run \"x\" $(id) `y`)",
		"-x started with a dash",
		"--agent evil",
		"-a other",
		"double  spaced",
		"trailing space ",
		"line one\nline two",
		"a  b\tc\nd",
		"ünïcödé ✓",
		"NUL\x00inside",
		"bad\xffbyte",
		"caf\xe9 latin-1 tail",
	}
	for _, reason := range reasons {
		reason := reason
		t.Run(fmt.Sprintf("%q", reason), func(t *testing.T) {
			t.Parallel()
			d, store := newTodoRouteDeps(t)
			mux := newTestMux(d)
			id, _ := store.Add(testAgentID, "verbatim target", "medium", "")

			w := postTodoDone(mux, testAgentID, id, reason)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			item, err := store.Get(testAgentID, id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if item.CloseReason != reason {
				t.Errorf("close_reason = %q, want it verbatim (%q)", item.CloseReason, reason)
			}
		})
	}
}

// TestTodoDone_Refusals proves requirement 3 (and the explicit-agent half of
// 4): every refusal names the todo and the cause with a distinctive status
// and body, and changes nothing — the snapshot of every store, the first
// agent's todo #1 included, is identical before and after.
func TestTodoDone_Refusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		agent    string
		id       int64
		reason   string
		body     string // raw body, when not built from the fields
		method   string
		wantCode int
		wantBody string
		closeSto bool // close the target agent's store before the POST
		nilStore bool // give the target agent no store at all
	}{
		{name: "unknown todo id", agent: testAgentID, id: 99, reason: "r", wantCode: http.StatusNotFound, wantBody: "todo #99 not found"},
		{name: "unknown agent", agent: "zz", id: 1, reason: "r", wantCode: http.StatusBadRequest, wantBody: `unknown agent: "zz"`},
		{name: "missing agent", agent: "", id: 1, reason: "r", wantCode: http.StatusBadRequest, wantBody: "agent is required"},
		{name: "empty reason", agent: testAgentID, id: 1, reason: "", wantCode: http.StatusBadRequest, wantBody: "reason is required"},
		// The reason travels ONLY as reason_base64: a plain JSON "reason"
		// string is refused naming the field, never base64-decoded — a
		// silently decoded string would store garbage bytes (a valid-looking
		// base64 string like "abcd") or fail mid-decode ("landed abc").
		{name: "plain string reason", body: `{"agent":"` + testAgentID + `","id":1,"reason":"landed abc"}`, wantCode: http.StatusBadRequest, wantBody: `in the "reason_base64" field`},
		{name: "plain string reason that is valid base64", body: `{"agent":"` + testAgentID + `","id":1,"reason":"abcd"}`, wantCode: http.StatusBadRequest, wantBody: `in the "reason_base64" field`},
		{name: "zero id", agent: testAgentID, id: 0, reason: "r", wantCode: http.StatusBadRequest, wantBody: "id is required"},
		{name: "negative id", agent: testAgentID, id: -3, reason: "r", wantCode: http.StatusBadRequest, wantBody: "id is required"},
		{name: "non-JSON body", body: "not json", wantCode: http.StatusBadRequest, wantBody: "bad request"},
		// reason_base64 must BE base64: a value that is not decodes to
		// nothing — the JSON decoder's own 400, never a close with
		// half-read bytes (the reviewer's invalid-base64 row).
		{name: "invalid base64 reason_base64", body: `{"agent":"` + testAgentID + `","id":1,"reason_base64":"!!not base64!!"}`, wantCode: http.StatusBadRequest, wantBody: "bad request"},
		{name: "GET method", method: http.MethodGet, wantCode: http.StatusMethodNotAllowed, wantBody: "method not allowed"},
		{name: "closed store", agent: testAgentID, id: 1, reason: "r", wantCode: http.StatusInternalServerError, wantBody: "close todo #1", closeSto: true},
		{name: "agent without store", agent: "other", id: 1, reason: "r", wantCode: http.StatusServiceUnavailable, wantBody: "todo store not configured", nilStore: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, store := newTodoRouteDeps(t)
			mux := newTestMux(d)
			// Both agents hold an open todo #1 in the one shared store: a
			// refused request must leave both untouched.
			store.Add(testAgentID, "must survive", "medium", "")
			store.Add("other", "must survive too", "medium", "")
			if tt.nilStore {
				d.agents["other"].cc.TodoStore = nil
			}
			if tt.closeSto {
				if err := store.Close(); err != nil {
					t.Fatalf("close store: %v", err)
				}
			}
			before := snapshotTodos(store)

			var w *httptest.ResponseRecorder
			switch {
			case tt.method != "":
				w = postJSONMethod(mux, tt.method, "/todo/done", `{}`)
			case tt.body != "":
				w = postJSON(mux, "/todo/done", tt.body)
			default:
				w = postTodoDone(mux, tt.agent, tt.id, tt.reason)
			}

			if w.Code != tt.wantCode {
				t.Errorf("status = %d, want %d; body: %s", w.Code, tt.wantCode, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", w.Body.String(), tt.wantBody)
			}
			if after := snapshotTodos(store); after != before {
				t.Errorf("a refused request changed the stores:\nbefore: %s\nafter:  %s", before, after)
			}
		})
	}
}

// TestTodoDone_LargeReasonBody pins the route's body cap against the
// limit a caller can actually hit: a reason assembled from several
// maximum-sized command-line arguments (the kernel allows 128 KiB per
// argument, ~2 MiB across the line) reaches well past the 1 MiB the
// other JSON endpoints cap at, so /todo/done's cap must sit above the
// base64-expanded argv ceiling — anything smaller refuses a reason the
// command line delivered fine (the round-19 review's F1: 7x120000-byte
// arguments failed with HTTP 413). The cap still exists: a body beyond
// todoDoneMaxBodyBytes is refused 413 and changes nothing.
func TestTodoDone_LargeReasonBody(t *testing.T) {
	t.Parallel()
	d, store := newTodoRouteDeps(t)
	mux := newTestMux(d)
	id, _ := store.Add(testAgentID, "big reason target", "medium", "")

	// Eight argv-sized words joined: 8x150000 bytes = 1.2 MB of reason,
	// ~1.6 MB on the wire after base64 — over the generic 1 MiB JSON cap,
	// under todoDoneMaxBodyBytes.
	words := make([]string, 8)
	for i := range words {
		words[i] = strings.Repeat("a", 150000-1) + strconv.Itoa(i)
	}
	reason := strings.Join(words, " ")
	w := postTodoDone(mux, testAgentID, id, reason)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	item, err := store.Get(testAgentID, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(item.CloseReason) != len(reason) || !strings.HasSuffix(item.CloseReason, "7") {
		t.Errorf("close_reason len = %d (want %d), tail = %q — the large reason must be stored verbatim", len(item.CloseReason), len(reason), item.CloseReason[len(item.CloseReason)-10:])
	}

	// Beyond the route's own cap the body is still refused, 413, and
	// nothing changes: the cap is a bound, just a higher one.
	before := snapshotTodos(store)
	huge := todoDoneRequest{Agent: testAgentID, ID: id, Reason: []byte(strings.Repeat("x", int(todoDoneMaxBodyBytes)))}
	body, err := json.Marshal(huge)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Base64 expands the reason 4/3, so this body is ~10.7 MB — over the cap.
	w = postJSON(mux, "/todo/done", string(body))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413; body: %s", w.Code, w.Body.String())
	}
	if after := snapshotTodos(store); after != before {
		t.Errorf("an over-cap request changed the store:\nbefore: %s\nafter:  %s", before, after)
	}
}

// postJSONMethod posts body with an explicit method (the GET/405 arm).
func postJSONMethod(mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// TestTodoDone_ClosesOnlyTheNamedAgentsTodo proves requirement 4: closing
// todo #1 of one agent never touches todo #1 of another — ids are per agent
// and the agent is the caller's explicit choice. Both agents hold todo #1 in
// the ONE shared store, so the agent_id scope of the write is what is being
// proved, not the separation of two store files.
func TestTodoDone_ClosesOnlyTheNamedAgentsTodo(t *testing.T) {
	t.Parallel()
	d, store := newTodoRouteDeps(t)
	mux := newTestMux(d)
	idMine, _ := store.Add(testAgentID, "mine", "medium", "")
	idTheirs, _ := store.Add("other", "theirs", "medium", "")
	if idMine != idTheirs {
		t.Fatalf("shared store gave different ids (%d, %d); the per-agent id row needs them equal", idMine, idTheirs)
	}

	if w := postTodoDone(mux, "other", idTheirs, "landed abc"); w.Code != http.StatusOK {
		t.Fatalf("close other #%d: status = %d, body: %s", idTheirs, w.Code, w.Body.String())
	}
	mine, _ := store.Get(testAgentID, idMine)
	if mine.Status != "open" || mine.CloseReason != "" {
		t.Errorf("test-agent #%d = {%s, %q} after closing other #%d, want untouched and open", idMine, mine.Status, mine.CloseReason, idTheirs)
	}
	theirs, _ := store.Get("other", idTheirs)
	if theirs.Status != "done" || theirs.CloseReason != "landed abc" {
		t.Errorf("other #%d = {%s, %q}, want done with the reason", idTheirs, theirs.Status, theirs.CloseReason)
	}

	// And the mirror direction.
	if w := postTodoDone(mux, testAgentID, idMine, "mine now"); w.Code != http.StatusOK {
		t.Fatalf("close test-agent #%d: status = %d, body: %s", idMine, w.Code, w.Body.String())
	}
	mine, _ = store.Get(testAgentID, idMine)
	if mine.Status != "done" || mine.CloseReason != "mine now" {
		t.Errorf("test-agent #%d = {%s, %q}, want done with the reason", idMine, mine.Status, mine.CloseReason)
	}
	// The mirror close must not have re-touched other's row either: both
	// directions are asserted, so a route that (also) writes the other
	// agent's same id fails here and above.
	theirs, _ = store.Get("other", idTheirs)
	if theirs.Status != "done" || theirs.CloseReason != "landed abc" {
		t.Errorf("other #%d = {%s, %q} after the mirror close, want still done with its own reason", idTheirs, theirs.Status, theirs.CloseReason)
	}
}

// TestTodoDoneEndpoint_NamedInStartupLog proves the endpoint inventory the
// operator greps at startup ("registered endpoints: ...") names /todo/done —
// an endpoint missing from the inventory reads as missing from the server.
//
// Serial, like the /app/stt twin: the inventory line carries nothing unique
// to this run, so in the parallel phase another test's handler registration
// could satisfy the assertion.
func TestTodoDoneEndpoint_NamedInStartupLog(t *testing.T) {

	logs := captureLog(t, flog.INFO)

	d, _ := httpTestSetup(t, httpTestOpts{})
	_ = newTestMux(d) // registering the handlers is what logs

	if len(logs.Lines("/todo/done")) == 0 {
		t.Errorf("registered-endpoints log does not name /todo/done:\n%s", logs)
	}
}

// TestTodoDone_AccessRules proves requirement 5: the route is reachable on
// the gateway's unix socket by its owner with no credential, and over TCP
// only with the HTTP API key — the exact wiring /command has (the same mux
// raw on the socket, behind newTCPGatewayHandler — the one constructor
// main's TCP listener serves — on TCP), so it adds no unauthenticated
// way to change todos. The 401, 403 and success arms are asserted
// separately: a plain 404 (route not registered) satisfies none of them.
func TestTodoDone_AccessRules(t *testing.T) {
	t.Parallel()
	d, store := newTodoRouteDeps(t)
	mux := newTestMux(d)
	id, _ := store.Add(testAgentID, "access target", "medium", "")
	body := fmt.Sprintf(`{"agent":%q,"id":%d,"reason_base64":"cg=="}`, testAgentID, id)

	// (a) Unix socket: same-user peer credentials, no secret involved.
	sockPath := filepath.Join(t.TempDir(), "todo-done.sock")
	srv, err := startUnixSocket(sockPath, mux)
	if err != nil {
		t.Fatalf("startUnixSocket: %v", err)
	}
	defer cleanupSocket(srv, sockPath)

	unixClient := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		},
	}}
	resp, err := unixClient.Post("http://foci-gw/todo/done", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST over unix socket: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("unix socket status = %d, want 200", resp.StatusCode)
	}
	if item, _ := store.Get(testAgentID, id); item.Status != "done" {
		t.Errorf("unix socket left #%d %q, want done", id, item.Status)
	}

	// (b) TCP: the same mux behind the one TCP handler constructor main
	// serves through (newTCPGatewayHandler) — the wrap is what a test
	// can observe of main's wiring.
	tcpSrv := httptest.NewServer(newTCPGatewayHandler("k", mux))
	defer tcpSrv.Close()

	postTCP := func(key string, body string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, tcpSrv.URL+"/todo/done", strings.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST over TCP: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// No credential → 401, todo still open.
	id2, _ := store.Add(testAgentID, "auth target", "medium", "")
	body2 := fmt.Sprintf(`{"agent":%q,"id":%d,"reason_base64":"cg=="}`, testAgentID, id2)
	respNo, err := http.Post(tcpSrv.URL+"/todo/done", "application/json", strings.NewReader(body2))
	if err != nil {
		t.Fatalf("POST without key: %v", err)
	}
	respNo.Body.Close()
	if respNo.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-credential status = %d, want 401", respNo.StatusCode)
	}
	if item, _ := store.Get(testAgentID, id2); item.Status != "open" {
		t.Errorf("a 401 request closed #%d", id2)
	}

	// Wrong key → 403, todo still open.
	if code := postTCP("wrong", body2); code != http.StatusForbidden {
		t.Errorf("wrong-key status = %d, want 403", code)
	}
	if item, _ := store.Get(testAgentID, id2); item.Status != "open" {
		t.Errorf("a 403 request closed #%d", id2)
	}

	// Right key → 200 and closed.
	if code := postTCP("k", body2); code != http.StatusOK {
		t.Errorf("right-key status = %d, want 200", code)
	}
	if item, _ := store.Get(testAgentID, id2); item.Status != "done" {
		t.Errorf("right key left #%d %q, want done", id2, item.Status)
	}
}

// TestTodoDone_CLIOverRealSocket is the end-to-end proof of requirements 1
// to 5: the REAL CLI binary (built the way internal/tools/execbridge_test.go
// builds foci-call), run with an environment of HOME and PATH only — the
// factory landing's environment, no FOCI_SOCK, no BASH_ENV, no FOCI_* at
// all — against the REAL mux served on a real unix socket at
// $HOME/data/foci-gw.sock, backed by a REAL TodoStore. The earlier tests
// in this file pin the handler arm by arm; this one pins the join: socket
// discovery, the wire contract and the store row, through the public
// entry point a service actually calls.
//
// disconnected-test-ok: black-box half — the client half of this test is a
// subprocess; the handler and store it drives are the in-process ones above.
func TestTodoDone_CLIOverRealSocket(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "foci")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "foci/cmd/foci").CombinedOutput(); err != nil {
		t.Fatalf("build foci CLI: %v\n%s", err, out)
	}

	d, store := newTodoRouteDeps(t)
	mux := newTestMux(d)

	// The gateway the bare-environment CLI must discover on its own.
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "data"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	sockPath := filepath.Join(home, "data", "foci-gw.sock")
	srv, err := startUnixSocket(sockPath, mux)
	if err != nil {
		t.Fatalf("startUnixSocket: %v", err)
	}
	defer cleanupSocket(srv, sockPath)

	// Bare environment: HOME (for socket discovery) and PATH, nothing else.
	bareEnv := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	for _, kv := range bareEnv {
		if strings.HasPrefix(kv, "FOCI_") || strings.HasPrefix(kv, "BASH_ENV") {
			t.Fatalf("bare env must not carry %q", kv)
		}
	}

	run := func(env []string, args ...string) (stdout, stderr string, exitErr error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		var out, errb bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errb
		err := cmd.Run()
		return out.String(), errb.String(), err
	}
	oneStderrLine := func(stderr string) string {
		line := strings.TrimSpace(stderr)
		if line == "" || strings.Contains(line, "\n") {
			t.Fatalf("stderr must be exactly one non-empty line, got %q", stderr)
		}
		return line
	}

	// Requirement 1 + 2: close with the spec's reasons; every byte of the
	// argv element is the stored close_reason, and the answer is "#N: done".
	// "-n" is the trailing-dash form: parseTodoDoneArgs stops at the id, so
	// a lone "-n" after it is a reason word, not a flag.
	for _, reason := range []string{
		"landed 3fc608953 (factory run 01M4SWKGRFGSMXBHKPKH3E6VEK)",
		`landed abc (run "x" $(id) ` + "`y`" + `)`,
		"-n a leading dash",
		"-n",
	} {
		id, _ := store.Add(testAgentID, "e2e target", "medium", "")
		stdout, stderr, err := run(bareEnv, "todo", "done", "-a", testAgentID, strconv.FormatInt(id, 10), reason)
		if err != nil {
			t.Fatalf("todo done %q: %v\nstderr: %s", reason, err, stderr)
		}
		if !strings.Contains(stdout, fmt.Sprintf("#%d: done", id)) {
			t.Errorf("stdout = %q, want it to contain #%d: done", stdout, id)
		}
		item, gerr := store.Get(testAgentID, id)
		if gerr != nil {
			t.Fatalf("Get: %v", gerr)
		}
		if item.CloseReason != reason {
			t.Errorf("close_reason = %q, want it verbatim %q", item.CloseReason, reason)
		}
		if item.Status != "done" || item.CompletedAt == nil {
			t.Errorf("item = {%s, completed=%v}, want done with completed_at set", item.Status, item.CompletedAt)
		}
	}

	// Requirement 4: the same id of another agent — same store, ids per
	// agent — is never touched by test-agent's close, in either direction.
	id, _ := store.Add(testAgentID, "e2e mine", "medium", "")
	idOther, _ := store.Add("other", "e2e other", "medium", "")
	if _, stderr, err := run(bareEnv, "todo", "done", "-a", testAgentID, strconv.FormatInt(id, 10), "mine"); err != nil {
		t.Fatalf("close test-agent #%d: %v\nstderr: %s", id, err, stderr)
	}
	other, _ := store.Get("other", idOther)
	if other.Status != "open" || other.CloseReason != "" {
		t.Errorf("other #%d = {%s, %q} after closing test-agent, want untouched", idOther, other.Status, other.CloseReason)
	}
	if _, stderr, err := run(bareEnv, "todo", "done", "-a", "other", strconv.FormatInt(idOther, 10), "theirs"); err != nil {
		t.Fatalf("close other #%d: %v\nstderr: %s", idOther, err, stderr)
	}
	mine, _ := store.Get(testAgentID, id)
	if mine.Status != "done" || mine.CloseReason != "mine" {
		t.Errorf("test-agent #%d = {%s, %q} after closing other, want still done with its own reason", id, mine.Status, mine.CloseReason)
	}

	// Requirement 3: refusals — non-zero exit, exactly one stderr line
	// naming the todo or the cause, and no row changes at all.
	before := snapshotTodos(store)
	for _, tt := range []struct {
		name   string
		args   []string
		wantIn string
	}{
		{name: "unknown todo id", args: []string{"todo", "done", "-a", testAgentID, "999", "r"}, wantIn: "#999"},
		{name: "unknown agent", args: []string{"todo", "done", "-a", "zz", strconv.FormatInt(id, 10), "r"}, wantIn: `"zz"`},
		{name: "empty reason", args: []string{"todo", "done", "-a", testAgentID, strconv.FormatInt(id, 10), ""}, wantIn: "reason is required"},
	} {
		stdout, stderr, err := run(bareEnv, tt.args...)
		if err == nil {
			t.Fatalf("%v must exit non-zero; stdout: %q stderr: %q", tt.args, stdout, stderr)
		}
		if line := oneStderrLine(stderr); !strings.Contains(line, tt.wantIn) {
			t.Errorf("%v: stderr line = %q, want it to contain %q", tt.args, line, tt.wantIn)
		}
	}
	if after := snapshotTodos(store); after != before {
		t.Errorf("a refused request changed the stores:\nbefore: %s\nafter:  %s", before, after)
	}

	// Requirement 3, unreachable gateway: HOME without a socket and a dead
	// FOCI_ADDR — one line, no dial-retry banner.
	deadHome := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for dead addr: %v", err)
	}
	deadAddr := l.Addr().String()
	_ = l.Close()
	unreachable := func(name string, env []string) {
		t.Helper()
		_, stderr, err := run(env, "todo", "done", "-a", testAgentID, strconv.FormatInt(id, 10), "r")
		if err == nil {
			t.Fatalf("%s: must exit non-zero", name)
		}
		line := oneStderrLine(stderr)
		if !strings.Contains(line, "#") || !strings.Contains(line, "unreachable") {
			t.Errorf("%s: stderr line = %q, want it to name the todo and the unreachable gateway", name, line)
		}
		if strings.Contains(line, "gave up after") {
			t.Errorf("%s: stderr line = %q, contains the retry-loop banner", name, line)
		}
	}
	unreachable("dead TCP addr", []string{"HOME=" + deadHome, "PATH=" + os.Getenv("PATH"), "FOCI_ADDR=" + deadAddr})

	// The literal socket-miss forms: a FOCI_GW_SOCK whose path does not
	// exist (discovery falls through to TCP), and a socket file that exists
	// but whose listener is gone (dial refused) — each is one line too.
	unreachable("socket path does not exist", []string{
		"HOME=" + deadHome, "PATH=" + os.Getenv("PATH"),
		"FOCI_GW_SOCK=" + filepath.Join(deadHome, "data", "foci-gw.sock"), "FOCI_ADDR=" + deadAddr,
	})
	staleHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(staleHome, "data"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	staleSock := filepath.Join(staleHome, "data", "foci-gw.sock")
	sl, err := net.Listen("unix", staleSock)
	if err != nil {
		t.Fatalf("listen for stale socket: %v", err)
	}
	sl.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = sl.Close() // the socket file stays; nothing answers dials now
	unreachable("stale socket file", []string{"HOME=" + staleHome, "PATH=" + os.Getenv("PATH")})

	// Requirement 5: over TCP the same mux sits behind the one handler
	// constructor main serves through (newTCPGatewayHandler) — no
	// credential is refused, the API key works. The CLI reaches it only
	// when no socket is discoverable (empty HOME + FOCI_ADDR).
	tcpSrv := httptest.NewServer(newTCPGatewayHandler("k", mux))
	defer tcpSrv.Close()
	idTCP, _ := store.Add(testAgentID, "tcp target", "medium", "")
	noSockHome := t.TempDir()
	_, stderr, err := run(
		[]string{"HOME=" + noSockHome, "PATH=" + os.Getenv("PATH"), "FOCI_ADDR=" + strings.TrimPrefix(tcpSrv.URL, "http://")},
		"todo", "done", "-a", testAgentID, strconv.FormatInt(idTCP, 10), "r")
	if err == nil {
		t.Fatal("TCP without the API key must exit non-zero")
	}
	if line := oneStderrLine(stderr); !strings.Contains(line, "401") {
		t.Errorf("stderr line = %q, want it to name the 401", line)
	}
	if item, _ := store.Get(testAgentID, idTCP); item.Status != "open" {
		t.Errorf("a 401 request closed #%d", idTCP)
	}
	if _, stderr, err := run(
		[]string{"HOME=" + noSockHome, "PATH=" + os.Getenv("PATH"), "FOCI_ADDR=" + strings.TrimPrefix(tcpSrv.URL, "http://"), "FOCI_API_KEY=k"},
		"todo", "done", "-a", testAgentID, strconv.FormatInt(idTCP, 10), "r"); err != nil {
		t.Fatalf("TCP with the API key: %v\nstderr: %s", err, stderr)
	}
	if item, _ := store.Get(testAgentID, idTCP); item.Status != "done" {
		t.Errorf("right key left #%d %q, want done", idTCP, item.Status)
	}
}

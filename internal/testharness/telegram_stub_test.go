package testharness

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// apiResponse mirrors the Bot API envelope the stub writes, for assertions.
type apiResponse struct {
	Ok          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// newTestStubBot starts a stub and registers one bot, returning both plus
// the token. Shared setup for the raw-HTTP handler tests below.
func newTestStubBot(t *testing.T) (*TelegramStub, string) {
	t.Helper()
	stub := NewTelegramStub()
	t.Cleanup(stub.Close)
	token := "111:TESTTOKEN"
	stub.RegisterBot(token, gotgbot.User{Id: 1, IsBot: true, FirstName: "Stub", Username: "stub_bot"})
	return stub, token
}

// postForm posts a URL-encoded body to /bot<token>/<method> and returns
// the raw HTTP response. Caller closes the body (or uses decodeAPI).
func postForm(t *testing.T, stub *TelegramStub, token, method string, form url.Values) *http.Response {
	t.Helper()
	resp, err := http.Post(
		stub.URL()+"/bot"+token+"/"+method,
		"application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		t.Fatalf("POST %s: %v", method, err)
	}
	return resp
}

// decodeAPI reads and closes the response body, unmarshalling the Bot API
// envelope.
func decodeAPI(t *testing.T, resp *http.Response) apiResponse {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	var out apiResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode API response %q: %v", b, err)
	}
	return out
}

// TestTelegramStub_InjectError_OneShot proves a queued fault fires on
// exactly one call: the first sendMessage gets the injected 502, the
// second succeeds normally.
func TestTelegramStub_InjectError_OneShot(t *testing.T) {
	stub, token := newTestStubBot(t)
	stub.InjectError("sendMessage", 502, "synthetic gateway error")

	form := url.Values{"chat_id": {"42"}, "text": {"hi"}}

	resp := postForm(t, stub, token, "sendMessage", form)
	if resp.StatusCode != 502 {
		t.Errorf("first call status = %d, want 502", resp.StatusCode)
	}
	api := decodeAPI(t, resp)
	if api.Ok || api.ErrorCode != 502 || api.Description != "synthetic gateway error" {
		t.Errorf("first call envelope = %+v, want ok=false code=502 desc=synthetic gateway error", api)
	}

	resp = postForm(t, stub, token, "sendMessage", form)
	if resp.StatusCode != 200 {
		t.Errorf("second call status = %d, want 200", resp.StatusCode)
	}
	if api := decodeAPI(t, resp); !api.Ok {
		t.Errorf("second call ok = false, want true (fault must be one-shot)")
	}
}

// TestTelegramStub_InjectErrorPersistent_UntilCleared proves a persistent
// fault repeats across calls and stops only after ClearInjections.
func TestTelegramStub_InjectErrorPersistent_UntilCleared(t *testing.T) {
	stub, token := newTestStubBot(t)
	stub.InjectErrorPersistent("sendMessage", 500, "always down")

	form := url.Values{"chat_id": {"42"}, "text": {"hi"}}
	for i := 0; i < 3; i++ {
		resp := postForm(t, stub, token, "sendMessage", form)
		if resp.StatusCode != 500 {
			t.Fatalf("call %d status = %d, want persistent 500", i, resp.StatusCode)
		}
		resp.Body.Close()
	}

	stub.ClearInjections("sendMessage")
	resp := postForm(t, stub, token, "sendMessage", form)
	if resp.StatusCode != 200 {
		t.Errorf("post-clear status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestTelegramStub_Inject429_RetryAfter proves the rate-limit fault emits
// HTTP 429 with parameters.retry_after embedded in the error envelope.
func TestTelegramStub_Inject429_RetryAfter(t *testing.T) {
	stub, token := newTestStubBot(t)
	stub.Inject429("sendMessage", 17)

	resp := postForm(t, stub, token, "sendMessage", url.Values{"chat_id": {"1"}, "text": {"x"}})
	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	api := decodeAPI(t, resp)
	if api.Ok || api.ErrorCode != 429 {
		t.Errorf("envelope = %+v, want ok=false code=429", api)
	}
	if api.Parameters.RetryAfter != 17 {
		t.Errorf("retry_after = %d, want 17", api.Parameters.RetryAfter)
	}
}

// TestTelegramStub_InjectBody_RawOverride proves a body fault returns the
// raw bytes with the requested content-type and status, and that empty
// content-type/status fall back to application/json and 200.
func TestTelegramStub_InjectBody_RawOverride(t *testing.T) {
	tests := []struct {
		name       string
		body       []byte
		ct         string
		status     int
		wantCT     string
		wantStatus int
	}{
		{
			name:       "explicit html error page",
			body:       []byte("<html>CDN says no</html>"),
			ct:         "text/html",
			status:     503,
			wantCT:     "text/html",
			wantStatus: 503,
		},
		{
			name:       "defaults applied",
			body:       []byte(`{"ok":false}`),
			ct:         "",
			status:     0,
			wantCT:     "application/json",
			wantStatus: 200,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub, token := newTestStubBot(t)
			stub.InjectBody("sendMessage", tt.body, tt.ct, tt.status)

			resp := postForm(t, stub, token, "sendMessage", url.Values{"chat_id": {"1"}, "text": {"x"}})
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if ct := resp.Header.Get("Content-Type"); ct != tt.wantCT {
				t.Errorf("content-type = %q, want %q", ct, tt.wantCT)
			}
			got, _ := io.ReadAll(resp.Body)
			if !bytes.Equal(got, tt.body) {
				t.Errorf("body = %q, want %q (verbatim)", got, tt.body)
			}
		})
	}
}

// TestTelegramStub_InjectConnDrop proves n queued drops kill exactly n
// connections (client sees a transport error, not an HTTP response) and
// the n+1th call succeeds; n<=0 is a no-op.
func TestTelegramStub_InjectConnDrop(t *testing.T) {
	stub, token := newTestStubBot(t)
	stub.InjectConnDrop("sendMessage", 0) // no-op: must not queue anything
	stub.InjectConnDrop("sendMessage", 2)

	u := stub.URL() + "/bot" + token + "/sendMessage"
	body := "chat_id=1&text=x"
	for i := 0; i < 2; i++ {
		resp, err := http.Post(u, "application/x-www-form-urlencoded", strings.NewReader(body))
		if err == nil {
			resp.Body.Close()
			t.Fatalf("call %d: expected transport error from conn drop, got HTTP %d", i, resp.StatusCode)
		}
	}

	resp, err := http.Post(u, "application/x-www-form-urlencoded", strings.NewReader(body))
	if err != nil {
		t.Fatalf("third call after 2 drops should succeed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("third call status = %d, want 200", resp.StatusCode)
	}
}

// TestTelegramStub_EmptyFault_Returns500 proves the applyFault misuse
// guard fires: a fault with no Code/Body/ConnDrop (InjectError with code
// 0) surfaces as a loud HTTP 500 rather than passing silently.
func TestTelegramStub_EmptyFault_Returns500(t *testing.T) {
	stub, token := newTestStubBot(t)
	stub.InjectError("sendMessage", 0, "")

	resp := postForm(t, stub, token, "sendMessage", url.Values{"chat_id": {"1"}, "text": {"x"}})
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500 for empty fault misuse", resp.StatusCode)
	}
}

// TestTelegramStub_EditMessageText proves the edit handler echoes the
// requested message_id and text, and auto-assigns an id when message_id
// is absent.
func TestTelegramStub_EditMessageText(t *testing.T) {
	stub, token := newTestStubBot(t)

	resp := postForm(t, stub, token, "editMessageText", url.Values{
		"chat_id": {"42"}, "message_id": {"7"}, "text": {"edited"},
	})
	api := decodeAPI(t, resp)
	if !api.Ok {
		t.Fatalf("ok = false, want true")
	}
	var msg gotgbot.Message
	if err := json.Unmarshal(api.Result, &msg); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if msg.MessageId != 7 || msg.Text != "edited" || msg.Chat.Id != 42 {
		t.Errorf("message = id=%d text=%q chat=%d, want id=7 text=edited chat=42",
			msg.MessageId, msg.Text, msg.Chat.Id)
	}

	// No message_id supplied: the stub assigns a fresh one.
	resp = postForm(t, stub, token, "editMessageText", url.Values{
		"chat_id": {"42"}, "text": {"edited2"},
	})
	api = decodeAPI(t, resp)
	if err := json.Unmarshal(api.Result, &msg); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if msg.MessageId == 0 {
		t.Errorf("message_id = 0, want auto-assigned non-zero")
	}
}

// TestTelegramStub_SendMedia proves sendDocument (and siblings) return a
// synthetic ok Message with an auto-assigned id.
func TestTelegramStub_SendMedia(t *testing.T) {
	stub, token := newTestStubBot(t)

	resp := postForm(t, stub, token, "sendDocument", url.Values{"chat_id": {"42"}})
	api := decodeAPI(t, resp)
	if !api.Ok {
		t.Fatalf("ok = false, want true")
	}
	var msg gotgbot.Message
	if err := json.Unmarshal(api.Result, &msg); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if msg.MessageId == 0 {
		t.Errorf("message_id = 0, want auto-assigned non-zero")
	}
}

// TestTelegramStub_UnknownMethod_ReturnsOK proves unmodelled Bot API verbs
// get a default ok=true so foci-gw doesn't crash on unhandled surfaces.
func TestTelegramStub_UnknownMethod_ReturnsOK(t *testing.T) {
	stub, token := newTestStubBot(t)

	resp := postForm(t, stub, token, "someFutureMethod", url.Values{})
	api := decodeAPI(t, resp)
	if !api.Ok {
		t.Errorf("ok = false, want true for unmodelled method")
	}
}

// TestTelegramStub_GetFile proves getFile returns the registered blob's
// path and size for a known file_id, and the stub fallback path for an
// unknown one.
func TestTelegramStub_GetFile(t *testing.T) {
	stub, token := newTestStubBot(t)
	stub.RegisterFile(token, "file-1", FileBlob{Path: "voice/msg.ogg", Data: []byte("opusdata")})

	// gotgbot sends getFile params as JSON — exercise the JSON branch of
	// extractField through the live handler.
	resp, err := http.Post(stub.URL()+"/bot"+token+"/getFile", "application/json",
		strings.NewReader(`{"file_id":"file-1"}`))
	if err != nil {
		t.Fatalf("POST getFile: %v", err)
	}
	api := decodeAPI(t, resp)
	var result struct {
		FilePath string `json:"file_path"`
		FileSize int    `json:"file_size"`
	}
	if err := json.Unmarshal(api.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.FilePath != "voice/msg.ogg" || result.FileSize != len("opusdata") {
		t.Errorf("getFile = %+v, want file_path=voice/msg.ogg size=8", result)
	}

	// Unknown file_id falls back to the stub path.
	resp, err = http.Post(stub.URL()+"/bot"+token+"/getFile", "application/json",
		strings.NewReader(`{"file_id":"nope"}`))
	if err != nil {
		t.Fatalf("POST getFile: %v", err)
	}
	api = decodeAPI(t, resp)
	if err := json.Unmarshal(api.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.FilePath != "stub.bin" {
		t.Errorf("unknown file_id file_path = %q, want stub.bin", result.FilePath)
	}
}

// TestTelegramStub_RegisterFile_UnknownTokenPanics proves RegisterFile
// fails fast (panics) when the bot token was never registered, so tests
// can't silently seed files on the wrong bot.
func TestTelegramStub_RegisterFile_UnknownTokenPanics(t *testing.T) {
	stub := NewTelegramStub()
	defer stub.Close()

	defer func() {
		if recover() == nil {
			t.Errorf("RegisterFile on unknown token did not panic")
		}
	}()
	stub.RegisterFile("never-registered", "f", FileBlob{Path: "x"})
}

// TestTelegramStub_PushUpdate_UnknownTokenPanics proves PushUpdate fails
// fast (panics) for an unregistered token rather than queueing into the void.
func TestTelegramStub_PushUpdate_UnknownTokenPanics(t *testing.T) {
	stub := NewTelegramStub()
	defer stub.Close()

	defer func() {
		if recover() == nil {
			t.Errorf("PushUpdate on unknown token did not panic")
		}
	}()
	stub.PushUpdate("never-registered", gotgbot.Update{})
}

// TestTelegramStub_FileDownload covers the /file/bot<token>/<path> routes:
// registered blob served verbatim with its MIME type, 404 for unknown
// path, 404 envelope for unknown token, 400 for a path missing the
// token/filePath separator, fault injection on the synthetic
// "fileDownload" method, and the recorded fileDownload paper trail.
func TestTelegramStub_FileDownload(t *testing.T) {
	stub, token := newTestStubBot(t)
	stub.RegisterFile(token, "file-1", FileBlob{Path: "voice/msg.ogg", Data: []byte("opusdata"), MIMEType: "audio/ogg"})

	get := func(path string) *http.Response {
		t.Helper()
		resp, err := http.Get(stub.URL() + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}

	// Happy path: registered blob served verbatim with MIME type.
	resp := get("/file/bot" + token + "/voice/msg.ogg")
	if resp.StatusCode != 200 {
		t.Fatalf("download status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/ogg" {
		t.Errorf("content-type = %q, want audio/ogg", ct)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(data) != "opusdata" {
		t.Errorf("body = %q, want opusdata", data)
	}

	// Download is recorded under the synthetic fileDownload method.
	var sawDownload bool
	for _, c := range stub.PeekSent(token) {
		if c.Method == "fileDownload" {
			sawDownload = true
		}
	}
	if !sawDownload {
		t.Errorf("no fileDownload SentCall recorded for the download")
	}

	// Unknown file path on a known token.
	resp = get("/file/bot" + token + "/no/such.bin")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unknown path status = %d, want 404", resp.StatusCode)
	}

	// Unknown token.
	resp = get("/file/botunknown:tok/x.bin")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unknown token status = %d, want 404", resp.StatusCode)
	}

	// Malformed path: no token/filePath separator.
	resp = get("/file/botnoslash")
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("malformed path status = %d, want 400", resp.StatusCode)
	}

	// Fault injection on the synthetic fileDownload method.
	stub.InjectError("fileDownload", 502, "download blew up")
	resp = get("/file/bot" + token + "/voice/msg.ogg")
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("faulted download status = %d, want 502", resp.StatusCode)
	}
}

// TestTelegramStub_HandleBadPaths proves the dispatcher rejects URLs that
// don't match /bot<token>/<method>: missing "bot" prefix is 404, missing
// method separator is 400.
func TestTelegramStub_HandleBadPaths(t *testing.T) {
	stub, _ := newTestStubBot(t)

	resp, err := http.Get(stub.URL() + "/nonsense")
	if err != nil {
		t.Fatalf("GET /nonsense: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("non-bot path status = %d, want 404", resp.StatusCode)
	}

	resp, err = http.Get(stub.URL() + "/bottokenwithoutmethod")
	if err != nil {
		t.Fatalf("GET /bottokenwithoutmethod: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("missing-method path status = %d, want 400", resp.StatusCode)
	}
}

// TestTelegramStub_PushCallbackQuery proves a pushed callback query is
// delivered on the next getUpdates with the data, chat, user, and an
// auto-assigned message id.
func TestTelegramStub_PushCallbackQuery(t *testing.T) {
	stub, token := newTestStubBot(t)
	stub.PushCallbackQuery(token, "im:req-1:0", 42, 7, 0)

	resp := postForm(t, stub, token, "getUpdates", url.Values{})
	api := decodeAPI(t, resp)
	var updates []gotgbot.Update
	if err := json.Unmarshal(api.Result, &updates); err != nil {
		t.Fatalf("decode updates: %v", err)
	}
	if len(updates) != 1 || updates[0].CallbackQuery == nil {
		t.Fatalf("updates = %+v, want exactly 1 callback query", updates)
	}
	cb := updates[0].CallbackQuery
	if cb.Data != "im:req-1:0" || cb.From.Id != 7 {
		t.Errorf("callback = data=%q from=%d, want data=im:req-1:0 from=7", cb.Data, cb.From.Id)
	}
	if cb.Message == nil {
		t.Fatalf("callback message is nil, want embedded chat/message info")
	}
	if cb.Message.GetChat().Id != 42 || cb.Message.GetMessageId() == 0 {
		t.Errorf("callback message = chat=%d id=%d, want chat=42 and auto-assigned id",
			cb.Message.GetChat().Id, cb.Message.GetMessageId())
	}
}

// TestTelegramStub_GetUpdates_OffsetTrims proves getUpdates honours the
// offset param: updates with id < offset are acknowledged and dropped,
// later ones still delivered.
func TestTelegramStub_GetUpdates_OffsetTrims(t *testing.T) {
	stub, token := newTestStubBot(t)
	stub.PushUpdate(token, gotgbot.Update{UpdateId: 5, Message: &gotgbot.Message{
		Chat: gotgbot.Chat{Id: 1, Type: "private"}, Text: "old"}})
	stub.PushUpdate(token, gotgbot.Update{UpdateId: 10, Message: &gotgbot.Message{
		Chat: gotgbot.Chat{Id: 1, Type: "private"}, Text: "new"}})

	resp := postForm(t, stub, token, "getUpdates", url.Values{"offset": {"6"}})
	api := decodeAPI(t, resp)
	var updates []gotgbot.Update
	if err := json.Unmarshal(api.Result, &updates); err != nil {
		t.Fatalf("decode updates: %v", err)
	}
	if len(updates) != 1 || updates[0].UpdateId != 10 {
		t.Fatalf("updates = %+v, want exactly the UpdateId=10 entry", updates)
	}
}

// TestTelegramStub_PeekSent proves PeekSent returns recorded calls without
// clearing them (two peeks agree, a later drain still sees them) and that
// unknown tokens yield nil for both Peek and Drain.
func TestTelegramStub_PeekSent(t *testing.T) {
	stub, token := newTestStubBot(t)

	resp := postForm(t, stub, token, "sendMessage", url.Values{"chat_id": {"1"}, "text": {"x"}})
	resp.Body.Close()

	first := stub.PeekSent(token)
	second := stub.PeekSent(token)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("peek lengths = %d, %d; want 1, 1 (peek must not clear)", len(first), len(second))
	}
	if drained := stub.DrainSent(token); len(drained) != 1 {
		t.Errorf("drain after peeks = %d calls, want 1", len(drained))
	}

	if got := stub.PeekSent("unknown"); got != nil {
		t.Errorf("PeekSent(unknown) = %v, want nil", got)
	}
	if got := stub.DrainSent("unknown"); got != nil {
		t.Errorf("DrainSent(unknown) = %v, want nil", got)
	}
}

// TestParseFormToJSON proves the body-normalisation heuristic: empty maps
// to {}, JSON passes through verbatim, multipart is wrapped raw, URL forms
// flatten (single value = string, repeated key = array), and unparseable
// bodies are preserved under _raw.
func TestParseFormToJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
		want map[string]any
	}{
		{name: "empty", body: "", want: map[string]any{}},
		{name: "json passthrough", body: `{"chat_id":42,"text":"hi"}`,
			want: map[string]any{"chat_id": float64(42), "text": "hi"}},
		{name: "multipart wrapped raw", body: "--boundary\r\ndata",
			want: map[string]any{"_raw_multipart": "--boundary\r\ndata"}},
		{name: "url form single values", body: "chat_id=42&text=hi",
			want: map[string]any{"chat_id": "42", "text": "hi"}},
		{name: "url form repeated key", body: "a=1&a=2",
			want: map[string]any{"a": []any{"1", "2"}}},
		{name: "unparseable form", body: "%zz",
			want: map[string]any{"_raw": "%zz"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := parseFormToJSON([]byte(tt.body))
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("output %q is not valid JSON: %v", raw, err)
			}
			wantJSON, _ := json.Marshal(tt.want)
			gotJSON, _ := json.Marshal(got)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Errorf("parseFormToJSON(%q) = %s, want %s", tt.body, gotJSON, wantJSON)
			}
		})
	}
}

// TestExtractField proves field extraction works for both encodings
// gotgbot uses (JSON string, JSON number via formatInt64, URL form) and
// returns "" for missing keys, non-scalar values, and malformed bodies.
func TestExtractField(t *testing.T) {
	tests := []struct {
		name string
		body string
		key  string
		want string
	}{
		{name: "empty body", body: "", key: "chat_id", want: ""},
		{name: "json string", body: `{"chat_id":"42"}`, key: "chat_id", want: "42"},
		{name: "json number", body: `{"chat_id":42}`, key: "chat_id", want: "42"},
		{name: "json missing key", body: `{"other":"x"}`, key: "chat_id", want: ""},
		{name: "json non-scalar value", body: `{"chat_id":[1,2]}`, key: "chat_id", want: ""},
		{name: "malformed json", body: `{not json`, key: "chat_id", want: ""},
		{name: "url form", body: "chat_id=42&text=hi", key: "text", want: "hi"},
		{name: "malformed form", body: "%zz=1", key: "chat_id", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractField([]byte(tt.body), tt.key); got != tt.want {
				t.Errorf("extractField(%q, %q) = %q, want %q", tt.body, tt.key, got, tt.want)
			}
		})
	}
}

// TestTelegramStub_ConcurrentPushAndPoll proves the stub's locking holds
// up under concurrent producers and consumers (run with -race to catch
// violations): all pushed updates are eventually delivered exactly once.
func TestTelegramStub_ConcurrentPushAndPoll(t *testing.T) {
	stub, token := newTestStubBot(t)

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stub.PushUpdate(token, gotgbot.Update{Message: &gotgbot.Message{
				Chat: gotgbot.Chat{Id: 1, Type: "private"}, Text: "m"}})
		}()
	}
	wg.Wait()

	seen := 0
	for seen < n {
		resp := postForm(t, stub, token, "getUpdates", url.Values{})
		api := decodeAPI(t, resp)
		var updates []gotgbot.Update
		if err := json.Unmarshal(api.Result, &updates); err != nil {
			t.Fatalf("decode updates: %v", err)
		}
		if len(updates) == 0 {
			t.Fatalf("getUpdates returned empty before all %d updates seen (got %d)", n, seen)
		}
		seen += len(updates)
	}
	if seen != n {
		t.Errorf("delivered %d updates, want exactly %d", seen, n)
	}
}

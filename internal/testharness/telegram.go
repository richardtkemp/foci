// Package testharness provides scaffolding for foci's L2 integration tests:
// a Telegram Bot API stub, a foci-gw subprocess manager, and helpers for
// driving end-to-end scenarios with real foci-gw against synthetic edges.
//
// This package is intentionally not imported by production code — only by
// tests under //go:build integration. It depends on gotgbot/v2 for Bot API
// types so test fixtures can construct Updates with type safety.
package testharness

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// TelegramStub is an in-process Bot API server that records inbound calls
// and serves outbound updates from a per-token queue. Tests construct one
// per test, point foci-gw at its URL via [platforms.telegram].api_base,
// and use PushUpdate / DrainSent to drive scenarios and assert outcomes.
//
// The stub is multiplexed by bot token: each bot registered with
// RegisterBot has its own update queue and sent-call log. Unknown tokens
// receive a 404 to surface misconfiguration loudly.
//
// Fault injection: tests can register synthetic per-method faults via the
// Inject* helpers (InjectError, InjectBody, InjectConnDrop, Inject429).
// Faults apply per HTTP method (e.g. "sendMessage", "getUpdates") and are
// scoped to the stub (not per-token) — sufficient for current failure tests
// which run one bot at a time. The fault queue is FIFO; persistent faults
// repeat until ClearInjections(method) is called.
type TelegramStub struct {
	server *httptest.Server

	mu      sync.Mutex
	bots    map[string]*botState // token → state
	nextMsg int64
	nextUpd int64

	// faults: per-method FIFO queue of one-shot faults. Drained on each
	// matching call. After drain, persistFault is consulted for the method.
	faults       map[string][]injectedFault // method → queue (one-shot)
	persistFault map[string]injectedFault   // method → persistent fault (until ClearInjections)
}

// injectedFault represents one scripted fault to apply to a single response.
// Mutually exclusive fields: exactly one of Body, ConnDrop, or Code is set.
//
// Code != 0          → emit a Bot API error JSON with HTTP status=Code
// Body != nil        → write raw bytes (status 200 unless BodyStatus set)
// ConnDrop=true      → hijack the underlying conn and close it without writing
//
// RetryAfter >0      → only valid when Code=429; embeds parameters.retry_after.
type injectedFault struct {
	Code        int    // HTTP + Bot API error_code (e.g. 429, 502)
	Desc        string // description in the error JSON
	RetryAfter  int    // for 429: parameters.retry_after seconds
	Body        []byte // raw body override; overrides Code/Desc when set
	BodyCT      string // content-type for Body ("" = application/json)
	BodyStatus  int    // HTTP status for Body ("" = 200)
	ConnDrop    bool   // hijack and close the underlying TCP conn
}

// botState holds per-token mutable state.
type botState struct {
	user    gotgbot.User
	updates []gotgbot.Update // pending getUpdates queue
	sent    []SentCall       // outbound API calls for assertion
	// files maps a file_id → registered FileBlob. Used by RegisterFile +
	// the /file/bot<token>/<filePath> download handler. Tests pre-register
	// blobs and reference the file_id when building synthetic Voice /
	// PhotoSize / Document Telegram payloads.
	files map[string]FileBlob
}

// FileBlob is a synthetic binary payload registered against a file_id.
// The Path field becomes the file_path returned by getFile; the Data is
// served verbatim from /file/bot<token>/<path>. MIMEType is informational
// (not echoed by the stub; foci's media handling sniffs separately).
type FileBlob struct {
	Path     string
	Data     []byte
	MIMEType string
}

// SentCall is one outbound API call captured from foci. Tests inspect
// these to verify side effects (e.g. did foci-gw send a sendMessage with
// the right body to the right chat).
type SentCall struct {
	Method  string          // e.g. "sendMessage"
	Body    json.RawMessage // JSON form of the parsed request payload
	Time    time.Time
}

// NewTelegramStub starts the stub on a local httptest port.
// Call Close in test cleanup.
func NewTelegramStub() *TelegramStub {
	s := &TelegramStub{
		bots:         map[string]*botState{},
		faults:       map[string][]injectedFault{},
		persistFault: map[string]injectedFault{},
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// InjectError queues a one-shot fault: the next call to method returns the
// given HTTP status code and Bot API error_code with desc as the
// description. Subsequent calls return normally unless more faults are
// queued. Method names match the Bot API verbs ("sendMessage",
// "editMessageText", "getUpdates", ...).
func (s *TelegramStub) InjectError(method string, code int, desc string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[method] = append(s.faults[method], injectedFault{Code: code, Desc: desc})
}

// InjectErrorPersistent installs a persistent fault for method. Every call
// returns code/desc until ClearInjections(method) is called. Useful for
// "5 consecutive failures escalates" assertions where transient injection
// would race the consumer.
func (s *TelegramStub) InjectErrorPersistent(method string, code int, desc string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persistFault[method] = injectedFault{Code: code, Desc: desc}
}

// Inject429 queues a one-shot Telegram-style rate-limit response: HTTP 429
// with parameters.retry_after=retryAfter in the body. foci's poll/send
// paths sanitize and log this without special-case handling, so the test
// surface is just "got an error response, didn't crash".
func (s *TelegramStub) Inject429(method string, retryAfter int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[method] = append(s.faults[method], injectedFault{
		Code:       429,
		Desc:       "Too Many Requests",
		RetryAfter: retryAfter,
	})
}

// InjectBody queues a one-shot raw body override: the next call to method
// returns the given bytes verbatim with the given Content-Type (empty =
// application/json) and the given HTTP status (0 = 200). Use this to
// simulate CDN HTML error pages or other malformed responses.
func (s *TelegramStub) InjectBody(method string, body []byte, contentType string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[method] = append(s.faults[method], injectedFault{
		Body:       append([]byte(nil), body...),
		BodyCT:     contentType,
		BodyStatus: status,
	})
}

// InjectConnDrop queues n one-shot connection drops on method: the stub
// hijacks the underlying TCP conn and closes it without writing a
// response. Simulates a mid-request network drop. Pass n=1 for a single
// drop, or higher for multiple consecutive drops.
func (s *TelegramStub) InjectConnDrop(method string, n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < n; i++ {
		s.faults[method] = append(s.faults[method], injectedFault{ConnDrop: true})
	}
}

// ClearInjections removes all queued and persistent faults for method.
func (s *TelegramStub) ClearInjections(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.faults, method)
	delete(s.persistFault, method)
}

// takeFault returns the next applicable fault for method (FIFO queue first,
// then persistent slot) or (zero, false) if no fault should fire.
func (s *TelegramStub) takeFault(method string) (injectedFault, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q := s.faults[method]; len(q) > 0 {
		f := q[0]
		s.faults[method] = q[1:]
		return f, true
	}
	if f, ok := s.persistFault[method]; ok {
		return f, true
	}
	return injectedFault{}, false
}

// URL returns the base URL to plug into [platforms.telegram].api_base.
// gotgbot constructs full method URLs as <base>/bot<token>/<method>.
func (s *TelegramStub) URL() string {
	return s.server.URL
}

// Close shuts down the HTTP server.
func (s *TelegramStub) Close() {
	s.server.Close()
}

// RegisterBot binds a bot token to a synthetic User profile. Tests must
// call this for every bot foci will use (one per agent + facets).
// Calling RegisterBot twice with the same token overwrites the User.
func (s *TelegramStub) RegisterBot(token string, user gotgbot.User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.bots[token]
	if !ok {
		st = &botState{}
		s.bots[token] = st
	}
	st.user = user
	if st.files == nil {
		st.files = map[string]FileBlob{}
	}
}

// RegisterFile makes fileID downloadable for the given token. The next
// getFile call for fileID returns blob.Path as file_path, and a request
// to /file/bot<token>/<blob.Path> returns blob.Data verbatim. Used by
// tests to drive end-to-end media-download paths against the stub.
func (s *TelegramStub) RegisterFile(token, fileID string, blob FileBlob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.bots[token]
	if !ok {
		panic(fmt.Sprintf("testharness: RegisterFile for unknown token %q — call RegisterBot first", token))
	}
	if st.files == nil {
		st.files = map[string]FileBlob{}
	}
	st.files[fileID] = blob
}

// PushUpdate enqueues a synthetic update for the bot identified by token.
// The next getUpdates call from that bot will drain it (subject to the
// offset semantics gotgbot uses). Update.UpdateId is auto-assigned if zero.
func (s *TelegramStub) PushUpdate(token string, upd gotgbot.Update) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.bots[token]
	if !ok {
		panic(fmt.Sprintf("testharness: PushUpdate for unknown token %q — call RegisterBot first", token))
	}
	if upd.UpdateId == 0 {
		upd.UpdateId = atomic.AddInt64(&s.nextUpd, 1)
	}
	if upd.Message != nil && upd.Message.MessageId == 0 {
		upd.Message.MessageId = atomic.AddInt64(&s.nextMsg, 1)
	}
	if upd.Message != nil && upd.Message.Date == 0 {
		upd.Message.Date = time.Now().Unix()
	}
	st.updates = append(st.updates, upd)
}

// PushCallbackQuery enqueues a synthetic callback_query Update simulating
// a user pressing an inline keyboard button.
//
// Foci's interactive-message machinery keys prompts by an ID it provided
// when it sent the original sendMessage; the per-button callback strings
// are encoded as "im:<promptID>:<buttonIndex>". For permission prompts,
// promptID is the CC requestID (UUID-like). Tests therefore typically
// call this as:
//
//	stub.PushCallbackQuery(token, "im:"+requestID+":0", chatID, userID, msgID)
//
// where index 0 = Allow, 1 = Deny, 2.. = "allow_always:<prefix>".
//
// fromUserID identifies the synthetic user pressing the button — must
// match the agent's allowed_users so the bot accepts it. chatID + msgID
// are stored on the embedded Message so the bot's callback handler can
// resolve them; msgID can be 0 (auto-assigned) when the test doesn't
// need to reference a specific message.
func (s *TelegramStub) PushCallbackQuery(token string, data string, chatID int64, fromUserID int64, msgID int64) {
	if msgID == 0 {
		msgID = atomic.AddInt64(&s.nextMsg, 1)
	}
	upd := gotgbot.Update{
		CallbackQuery: &gotgbot.CallbackQuery{
			Id:   fmt.Sprintf("cb-%d", atomic.AddInt64(&s.nextUpd, 1)),
			Data: data,
			From: gotgbot.User{Id: fromUserID, FirstName: "Tester"},
			// MaybeInaccessibleMessage interface satisfied by Message{}
			// (value receiver — see gotgbot/v2/gen_types.go).
			Message: gotgbot.Message{
				Chat:      gotgbot.Chat{Id: chatID, Type: "private"},
				MessageId: msgID,
			},
		},
	}
	s.PushUpdate(token, upd)
}

// DrainSent returns and clears the recorded outbound calls for a token.
// Tests use this to assert what foci tried to send.
func (s *TelegramStub) DrainSent(token string) []SentCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.bots[token]
	if !ok {
		return nil
	}
	out := st.sent
	st.sent = nil
	return out
}

// PeekSent is like DrainSent but doesn't clear the buffer. Useful for
// polling-style assertions ("wait until a sendMessage with X arrives").
func (s *TelegramStub) PeekSent(token string) []SentCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.bots[token]
	if !ok {
		return nil
	}
	out := make([]SentCall, len(st.sent))
	copy(out, st.sent)
	return out
}

// handle dispatches incoming HTTP requests. URL shape: /bot<token>/<method>.
// File downloads come in as /file/bot<token>/<filePath> and route to the
// per-token files map populated via RegisterFile.
func (s *TelegramStub) handle(w http.ResponseWriter, r *http.Request) {
	// Path is like /bot12345:ABCDEF/sendMessage
	path := strings.TrimPrefix(r.URL.Path, "/")
	if strings.HasPrefix(path, "file/bot") {
		s.serveFileDownload(w, strings.TrimPrefix(path, "file/bot"))
		return
	}
	if !strings.HasPrefix(path, "bot") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rest := strings.TrimPrefix(path, "bot")
	slash := strings.Index(rest, "/")
	if slash < 0 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	token := rest[:slash]
	method := rest[slash+1:]

	s.mu.Lock()
	st, ok := s.bots[token]
	s.mu.Unlock()
	if !ok {
		// Unknown token — surface loudly so misconfigured tests fail fast
		// rather than hanging on long-polls. Real Telegram returns 401
		// Unauthorized for a bad/unknown token (404 is reserved for unknown
		// *methods*); matching that lets foci's isPermanentTelegramErr
		// fast-fail instead of retrying the token check with backoff. The
		// "unknown bot token" phrase is preserved for test diagnostics.
		writeError(w, 401, "Unauthorized: unknown bot token (RegisterBot first)")
		return
	}

	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	s.recordCall(st, method, body)

	// Apply injected fault (if any) before normal dispatch. Faults are
	// per-method; getMe is intentionally not faulted in any current test
	// path because gateway readiness depends on it.
	if f, ok := s.takeFault(method); ok {
		s.applyFault(w, f)
		return
	}

	switch method {
	case "getMe":
		writeOK(w, st.user)
	case "getUpdates":
		s.serveGetUpdates(w, st, body)
	case "setMyCommands":
		writeOK(w, true)
	case "sendMessage":
		s.serveSendMessage(w, st, body)
	case "editMessageText":
		s.serveEditMessageText(w, st, body)
	case "sendChatAction":
		writeOK(w, true)
	case "sendDocument", "sendVoice", "sendVideo", "sendPhoto", "sendAudio", "sendAnimation":
		s.serveSendMedia(w, st)
	case "getFile":
		// Look up the registered file_path for this file_id. Tests use
		// RegisterFile to seed a blob; bots that ask for an unknown
		// file_id get a stub response (preserves backward compat).
		fileID := extractField(body, "file_id")
		s.mu.Lock()
		blob, ok := st.files[fileID]
		s.mu.Unlock()
		if ok {
			writeOK(w, map[string]any{
				"file_id":   fileID,
				"file_path": blob.Path,
				"file_size": len(blob.Data),
			})
		} else {
			writeOK(w, map[string]any{"file_id": fileID, "file_path": "stub.bin"})
		}
	case "answerCallbackQuery":
		writeOK(w, true)
	case "deleteMessage":
		writeOK(w, true)
	default:
		// Pre-shipping unknown methods: treat as ok=true with empty result
		// so foci-gw doesn't crash on a Telegram surface we haven't modelled.
		// Logged in SentCall so tests can detect drift.
		writeOK(w, true)
	}
}

// recordCall stores an outbound call. body is the raw form POST body
// (URL-encoded or multipart); we attempt JSON parsing for ergonomic
// assertions, falling back to the raw bytes when that fails.
func (s *TelegramStub) recordCall(st *botState, method string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// gotgbot sends application/x-www-form-urlencoded or multipart; the
	// most ergonomic form for assertion is a JSON object keyed by form
	// field. Parse the body as form data and re-emit as JSON.
	parsed := parseFormToJSON(body)
	st.sent = append(st.sent, SentCall{
		Method: method,
		Body:   parsed,
		Time:   time.Now(),
	})
}

// parseFormToJSON normalises a request body into a flat JSON map.
// gotgbot uses Content-Type: application/json for plain method calls
// and multipart/form-data when files are attached. The stub doesn't see
// the Content-Type here (callers pass just the body), so we heuristic on
// the first byte: '{' = JSON, '-' = multipart, anything else = URL form.
func parseFormToJSON(body []byte) json.RawMessage {
	if len(body) == 0 {
		return json.RawMessage("{}")
	}
	switch body[0] {
	case '{':
		// gotgbot's normal path — JSON-encoded params. Re-emit verbatim so
		// tests can json.Unmarshal into typed structs.
		return json.RawMessage(append([]byte(nil), body...))
	case '-':
		// Multipart form — keep raw for now; first 5 tests don't assert
		// on individual multipart fields.
		raw, _ := json.Marshal(map[string]any{"_raw_multipart": string(body)})
		return raw
	}
	values, err := parseURLForm(body)
	if err != nil {
		raw, _ := json.Marshal(map[string]any{"_raw": string(body)})
		return raw
	}
	out := make(map[string]any, len(values))
	for k, v := range values {
		if len(v) == 1 {
			out[k] = v[0]
		} else {
			out[k] = v
		}
	}
	raw, _ := json.Marshal(out)
	return raw
}

// extractField pulls a string value out of a request body regardless of
// whether gotgbot sent JSON or URL-encoded form. Used by serveSendMessage
// et al. to read chat_id and text without forcing a particular encoding.
func extractField(body []byte, key string) string {
	if len(body) == 0 {
		return ""
	}
	if body[0] == '{' {
		var m map[string]any
		if err := json.Unmarshal(body, &m); err == nil {
			switch v := m[key].(type) {
			case string:
				return v
			case float64:
				// Numeric JSON fields (chat_id, message_id) decode as float64.
				return formatInt64(int64(v))
			}
		}
		return ""
	}
	if values, err := parseURLForm(body); err == nil {
		return values.Get(key)
	}
	return ""
}

// serveGetUpdates drains pending updates up to a soft cap. gotgbot polls
// with a long timeout (~60s in foci); we return immediately if we have
// updates, otherwise block briefly so the test harness can synchronise.
func (s *TelegramStub) serveGetUpdates(w http.ResponseWriter, st *botState, body []byte) {
	// Optional: respect offset to drop acknowledged updates.
	if offsetStr := extractField(body, "offset"); offsetStr != "" {
		// Trim updates with id < offset
		s.mu.Lock()
		var keep []gotgbot.Update
		for _, u := range st.updates {
			if u.UpdateId >= parseInt64(offsetStr) {
				keep = append(keep, u)
			}
		}
		st.updates = keep
		s.mu.Unlock()
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		s.mu.Lock()
		if len(st.updates) > 0 {
			out := st.updates
			st.updates = nil
			s.mu.Unlock()
			writeOK(w, out)
			return
		}
		s.mu.Unlock()
		if time.Now().After(deadline) {
			writeOK(w, []gotgbot.Update{})
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// serveSendMessage returns a synthetic Message echoing the chat_id and text.
func (s *TelegramStub) serveSendMessage(w http.ResponseWriter, st *botState, body []byte) {
	chatID := parseInt64(extractField(body, "chat_id"))
	text := extractField(body, "text")
	msg := buildMessage(s, st.user, chatID, text)
	writeOK(w, msg)
}

// serveEditMessageText is a no-op edit returning a synthetic Message.
func (s *TelegramStub) serveEditMessageText(w http.ResponseWriter, st *botState, body []byte) {
	chatID := parseInt64(extractField(body, "chat_id"))
	msgID := parseInt64(extractField(body, "message_id"))
	text := extractField(body, "text")
	msg := buildMessage(s, st.user, chatID, text)
	if msgID != 0 {
		msg.MessageId = msgID
	}
	writeOK(w, msg)
}

// serveSendMedia returns a synthetic Message for any sendXxx method. We
// don't parse multipart payloads — tests assert on the SentCall body
// recorded above (which keeps the raw multipart bytes).
func (s *TelegramStub) serveSendMedia(w http.ResponseWriter, st *botState) {
	msg := buildMessage(s, st.user, 0, "")
	writeOK(w, msg)
}

// buildMessage assembles a minimum-viable Message Telegram would have
// produced. The chat field is mandatory; foci's reader relies on it.
func buildMessage(s *TelegramStub, from gotgbot.User, chatID int64, text string) gotgbot.Message {
	return gotgbot.Message{
		MessageId: atomic.AddInt64(&s.nextMsg, 1),
		Date:      time.Now().Unix(),
		Chat: gotgbot.Chat{
			Id:   chatID,
			Type: "private",
		},
		From: &from,
		Text: text,
	}
}

// writeOK writes a `{"ok":true,"result":<r>}` Bot API response.
func writeOK(w http.ResponseWriter, r any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": r})
}

// serveFileDownload handles /file/bot<token>/<filePath> requests. The
// `rest` argument is the path after the "file/bot" prefix — beginning
// with <token>/<filePath>. Tests register blobs via RegisterFile keyed
// by file_id; the file_path returned by getFile points the download
// here. Fault injection on the synthetic method "fileDownload" lets
// tests simulate transient download failures.
func (s *TelegramStub) serveFileDownload(w http.ResponseWriter, rest string) {
	slash := strings.Index(rest, "/")
	if slash < 0 {
		http.Error(w, "bad file path", http.StatusBadRequest)
		return
	}
	token := rest[:slash]
	filePath := rest[slash+1:]

	s.mu.Lock()
	st, ok := s.bots[token]
	s.mu.Unlock()
	if !ok {
		writeError(w, 404, "unknown bot token (RegisterBot first)")
		return
	}

	// Record the download call against the synthetic "fileDownload"
	// method so PeekSent gives tests a paper trail without colliding
	// with real Bot API verbs.
	s.recordCall(st, "fileDownload", []byte(filePath))

	// Honour any fault injection on the fileDownload method.
	if f, ok := s.takeFault("fileDownload"); ok {
		s.applyFault(w, f)
		return
	}

	// Find the blob whose Path matches.
	s.mu.Lock()
	var blob FileBlob
	var found bool
	for _, b := range st.files {
		if b.Path == filePath {
			blob = b
			found = true
			break
		}
	}
	s.mu.Unlock()
	if !found {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	if blob.MIMEType != "" {
		w.Header().Set("Content-Type", blob.MIMEType)
	}
	_, _ = w.Write(blob.Data)
}

// applyFault writes the configured fault to the response. For ConnDrop, it
// hijacks and closes the underlying TCP conn. For Body, it writes raw bytes
// with the requested content-type and status. For Code, it emits a Bot API
// error JSON. For 429s, it embeds parameters.retry_after.
func (s *TelegramStub) applyFault(w http.ResponseWriter, f injectedFault) {
	if f.ConnDrop {
		// Hijack and close the TCP conn so the client sees io.EOF /
		// "connection reset" / "broken pipe" depending on timing.
		hj, ok := w.(http.Hijacker)
		if !ok {
			// Fallback if the server's ResponseWriter doesn't implement
			// Hijacker for some reason. Surface as 502 so tests still see
			// a failure (just not the specific drop signature).
			writeError(w, 502, "stub: hijack unavailable, simulating with 502")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			writeError(w, 502, "stub: hijack failed")
			return
		}
		_ = conn.Close()
		return
	}
	if f.Body != nil {
		ct := f.BodyCT
		if ct == "" {
			ct = "application/json"
		}
		status := f.BodyStatus
		if status == 0 {
			status = 200
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(f.Body)
		return
	}
	if f.Code != 0 {
		desc := f.Desc
		if desc == "" {
			desc = http.StatusText(f.Code)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.Code)
		payload := map[string]any{
			"ok":          false,
			"error_code":  f.Code,
			"description": desc,
		}
		if f.RetryAfter > 0 {
			payload["parameters"] = map[string]any{"retry_after": f.RetryAfter}
		}
		_ = json.NewEncoder(w).Encode(payload)
		return
	}
	// No-op fault — shouldn't happen, fall through to a 500 so the test
	// notices the misuse.
	writeError(w, 500, "stub: applyFault with empty fault")
}

// writeError writes a `{"ok":false,"error_code":N,"description":S}` response.
func writeError(w http.ResponseWriter, code int, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":          false,
		"error_code":  code,
		"description": desc,
	})
}

package app

// GET /app/stt (#2315, server half of #1151): the endpoint that hands a paired
// device the agent's speech-to-text credential so the app can transcribe a
// preview-the-text voice note directly at the provider. These tests pin the
// wire contract (fields, cache header), the gate (auth, method, agent
// resolution, no-STT refusal) and the one discipline that matters most: the
// provider key reaches the response body and NOTHING else — no log line.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"foci/internal/config"
	flog "foci/internal/log"
	"foci/internal/platform"
	"foci/internal/voice"
)

// sttTestHub returns a hub with one paired device (its token) and one agent
// whose STT provider is a real *voice.OpenAISTT — the same type production
// wires through lazySTT — so the credential on the wire is the provider's own.
func sttTestHub(t *testing.T) (*Hub, string) {
	t.Helper()
	h := newTestHub()
	h.agents["clutch"] = &appConn{hub: h, agentID: "clutch", stt: &voice.OpenAISTT{
		Endpoint: "https://stt.example/v1/audio/transcriptions",
		APIKey:   "sk-stt-test-cred-7f3a",
		Model:    "whisper-large-v3",
	}}
	h.agentOrder = append(h.agentOrder, "clutch")
	d := h.devices.pair("phone-1", "Phone")
	return h, d.Token
}

// TestServeSTT_ReturnsCredential pins the happy-path wire contract: an
// authenticated GET returns 200 application/json with Cache-Control: no-store,
// and every field — endpoint, model, apiKey, format, fileField, modelField — is
// the value the agent's resolved STT provider holds, never a re-read of config.
func TestServeSTT_ReturnsCredential(t *testing.T) {
	h, tok := sttTestHub(t)

	r := httptest.NewRequest(http.MethodGet, "/app/stt?agentId=clutch", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeSTT(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("cache-control = %q, want no-store — a credential must never sit in a shared cache", cc)
	}

	var resp struct {
		Endpoint   string `json:"endpoint"`
		Model      string `json:"model"`
		APIKey     string `json:"apiKey"`
		Format     string `json:"format"`
		FileField  string `json:"fileField"`
		ModelField string `json:"modelField"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, w.Body.String())
	}
	if resp.Endpoint != "https://stt.example/v1/audio/transcriptions" {
		t.Errorf("endpoint = %q, want the provider's", resp.Endpoint)
	}
	if resp.Model != "whisper-large-v3" {
		t.Errorf("model = %q, want the provider's", resp.Model)
	}
	if resp.APIKey != "sk-stt-test-cred-7f3a" {
		t.Errorf("apiKey = %q, want the provider's resolved key", resp.APIKey)
	}
	if resp.Format != "openai-multipart" {
		t.Errorf("format = %q, want openai-multipart (the only shape today)", resp.Format)
	}
	if resp.FileField != "file" {
		t.Errorf("fileField = %q, want file", resp.FileField)
	}
	if resp.ModelField != "model" {
		t.Errorf("modelField = %q, want model", resp.ModelField)
	}
}

// TestServeSTT_RoutedThroughHandler proves the endpoint serves through the
// real registration path (STTHandler → withHub → active hub), not just the
// method value — a handler nobody can reach is indistinguishable from none.
func TestServeSTT_RoutedThroughHandler(t *testing.T) {
	h, tok := sttTestHub(t)
	h.deps = platform.ProviderDeps{Config: &config.Config{}}
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })

	mux := http.NewServeMux()
	mux.HandleFunc("/app/stt", STTHandler())

	r := httptest.NewRequest(http.MethodGet, "/app/stt?agentId=clutch", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("routed code = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "whisper-large-v3") {
		t.Errorf("routed body = %s, want the agent's credential", w.Body.String())
	}
}

// TestServeSTT_AuthRequired: the gate matches every other /app route — no
// credential is 401, a wrong credential is 403 (the device-token endpoints'
// missing/invalid split, same as ServeHistory).
func TestServeSTT_AuthRequired(t *testing.T) {
	h, _ := sttTestHub(t)

	w := httptest.NewRecorder()
	h.ServeSTT(w, httptest.NewRequest(http.MethodGet, "/app/stt?agentId=clutch", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no-credential code = %d, want 401", w.Code)
	}

	r := httptest.NewRequest(http.MethodGet, "/app/stt?agentId=clutch", nil)
	r.Header.Set("Authorization", "Bearer not-a-device-token")
	w = httptest.NewRecorder()
	h.ServeSTT(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("garbage-bearer code = %d, want 403", w.Code)
	}
}

// TestServeSTT_RevokedDeviceRejected proves revocation stops the credential
// fetch: the same token that worked before the revoke is refused after — the
// 403 every other device-token endpoint gives a revoked credential. (The key
// already handed out stays valid at the provider until it is rotated — that
// trade-off is documented in docs/WIRING.md, not testable server-side.)
func TestServeSTT_RevokedDeviceRejected(t *testing.T) {
	h, tok := sttTestHub(t)

	r := httptest.NewRequest(http.MethodGet, "/app/stt?agentId=clutch", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeSTT(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("pre-revoke code = %d, want 200", w.Code)
	}

	if _, ok := h.devices.revoke("phone-1"); !ok {
		t.Fatal("revoke failed")
	}

	r = httptest.NewRequest(http.MethodGet, "/app/stt?agentId=clutch", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	h.ServeSTT(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("post-revoke code = %d, want 403", w.Code)
	}
}

// TestServeSTT_MethodNotAllowed: only GET is served; anything else is 405
// after auth, the sibling endpoints' verb gate.
func TestServeSTT_MethodNotAllowed(t *testing.T) {
	h, tok := sttTestHub(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		r := httptest.NewRequest(method, "/app/stt?agentId=clutch", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeSTT(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s code = %d, want 405", method, w.Code)
		}
	}
}

// TestServeSTT_RequiresAgentID: a missing agentId is a 400 like the missing
// conversationId of /app/history — the parameter names the agent whose
// credential is wanted, so there is no default to fall back to.
func TestServeSTT_RequiresAgentID(t *testing.T) {
	h, tok := sttTestHub(t)
	r := httptest.NewRequest(http.MethodGet, "/app/stt", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeSTT(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

// TestServeSTT_UnknownAgent: an agent id the hub does not know is a clean 404.
func TestServeSTT_UnknownAgent(t *testing.T) {
	h, tok := sttTestHub(t)
	r := httptest.NewRequest(http.MethodGet, "/app/stt?agentId=ghost", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeSTT(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

// TestServeSTT_NoSTTConfigured pins the spec's exact refusal body: an agent
// with no STT provider (conn.stt == nil) gets 404 with
// "no speech-to-text configured for this agent" — one body, byte for byte.
func TestServeSTT_NoSTTConfigured(t *testing.T) {
	h, tok := sttTestHub(t)
	h.agents["mute"] = &appConn{hub: h, agentID: "mute"}
	h.agentOrder = append(h.agentOrder, "mute")

	r := httptest.NewRequest(http.MethodGet, "/app/stt?agentId=mute", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeSTT(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
	if got := strings.TrimSuffix(w.Body.String(), "\n"); got != "no speech-to-text configured for this agent" {
		t.Errorf("body = %q, want the configured refusal", got)
	}
}

// TestServeSTT_OpaqueProviderRefused: a provider that declares no direct-call
// shape gets the same refusal as no provider at all — the client cannot be
// told how to call it, so it must fall back to the server path.
func TestServeSTT_OpaqueProviderRefused(t *testing.T) {
	h, tok := sttTestHub(t)
	h.agents["opaque"] = &appConn{hub: h, agentID: "opaque", stt: fakeSTT{text: "hi"}}
	h.agentOrder = append(h.agentOrder, "opaque")

	r := httptest.NewRequest(http.MethodGet, "/app/stt?agentId=opaque", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeSTT(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
	if got := strings.TrimSuffix(w.Body.String(), "\n"); got != "no speech-to-text configured for this agent" {
		t.Errorf("body = %q, want the same refusal as no provider", got)
	}
}

// TestServeSTT_NeverLogsKey is the disclosure guard: with the log captured at
// INFO (the production default), a successful issue logs exactly one line
// naming the device and agent — and the provider key appears nowhere in it.
// The positive control (the issuance line) matters as much as the absence:
// deleting the log line must fail this test too, not just leaking the key.
func TestServeSTT_NeverLogsKey(t *testing.T) {
	h, tok := sttTestHub(t)

	var buf bytes.Buffer
	flog.SetOutput(&buf)
	flog.SetLevel(flog.INFO)
	t.Cleanup(func() { flog.SetOutput(os.Stderr); flog.SetLevel(flog.INFO) })

	r := httptest.NewRequest(http.MethodGet, "/app/stt?agentId=clutch", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeSTT(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (issue the credential)", w.Code)
	}

	out := buf.String()
	if !strings.Contains(out, "stt credential issued: device=phone-1 agent=clutch") {
		t.Errorf("no issuance line in captured log:\n%s", out)
	}
	if strings.Contains(out, "sk-stt-test-cred-7f3a") {
		t.Fatalf("provider key LEAKED into the log:\n%s", out)
	}
}

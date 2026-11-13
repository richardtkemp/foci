package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// #3062: a socket's identity is the device its Bearer token authenticated, not
// the deviceId its hello self-reports. These tests drive the real hello handler
// (dispatchInbound) and the real revoke handler (ServeRevoke).

// authedClient registers a live socket the way ServeWS does after authenticate:
// identity from the paired device, published through admitClient.
func authedClient(t *testing.T, h *Hub, dev *device) *wsClient {
	t.Helper()
	c := fakeClientFor(h)
	if !h.admitClient(c, dev) {
		t.Fatalf("admitClient(%s) refused a live device", dev.DeviceID)
	}
	return c
}

func revokeVia(t *testing.T, h *Hub, actor *device, deviceID string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeRevoke(w, secReq(http.MethodPost, "/app/pair/revoke", `{"deviceId":"`+deviceID+`"}`, "Bearer "+actor.Token))
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke %s: code %d", deviceID, w.Code)
	}
}

// Requirements 1 and 3: a hello reporting another id neither changes the
// socket's identity nor lets it survive its device's revocation.
func TestHelloIdentity_ConflictingHelloDoesNotEvadeRevocation(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	stolen := h.devices.pair("stolen", "")
	admin := h.devices.pair("admin", "")

	c := authedClient(t, h, stolen)
	h.dispatchInbound(c, []byte(`{"t":"hello","id":"x","d":{"client":{"deviceId":"someone-else"},"pushToken":"ptok"}}`))
	drainEnv(t, c)

	if got := c.device(); got != "stolen" {
		t.Errorf("socket identity after conflicting hello = %q, want the authenticated %q", got, "stolen")
	}

	revokeVia(t, h, admin, "stolen")
	if !isClosed(c.done) {
		t.Fatal("socket of a revoked device is still open after its hello reported another device id")
	}
}

// Requirement 2: eviction and push-token ownership follow the authenticated id.
func TestHelloIdentity_ConflictingHelloCannotEvictOrClaimPushes(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	victim := h.devices.pair("victim", "")
	attacker := h.devices.pair("attacker", "")

	vc := authedClient(t, h, victim)
	h.dispatchInbound(vc, []byte(`{"t":"hello","id":"v","d":{"client":{"deviceId":"victim"},"pushToken":"victim-tok"}}`))
	drainEnv(t, vc)

	ac := authedClient(t, h, attacker)
	h.dispatchInbound(ac, []byte(`{"t":"hello","id":"a","d":{"client":{"deviceId":"victim"},"pushToken":"attacker-tok"}}`))
	drainEnv(t, ac)

	if isClosed(vc.done) {
		t.Error("a hello claiming the victim's id evicted the victim's socket")
	}
	if isClosed(ac.done) {
		t.Error("the conflicting socket was closed; only its claimed identity should be ignored")
	}
	got := h.tokens.targetsExcluding(nil)
	if got["victim"] != "victim-tok" {
		t.Errorf("victim's push token = %q, want %q (a conflicting hello must not take it over)", got["victim"], "victim-tok")
	}
	if got["attacker"] != "attacker-tok" {
		t.Errorf("attacker's push token = %q, want it filed under its authenticated id", got["attacker"])
	}
}

// Requirement 4: the real app's hello (id == authenticated id) behaves as
// before: identity kept, token registered, a second socket for the same device
// replaces the first (4409), and revoke closes it.
func TestHelloIdentity_MatchingHelloUnchanged(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	phone := h.devices.pair("phone", "")
	admin := h.devices.pair("admin", "")

	old := authedClient(t, h, phone)
	h.dispatchInbound(old, []byte(`{"t":"hello","id":"1","d":{"client":{"deviceId":"phone"},"pushToken":"p1"}}`))
	drainEnv(t, old)

	c := authedClient(t, h, phone)
	if !isClosed(old.done) {
		t.Error("a second socket for the same device did not replace the first")
	}
	h.dispatchInbound(c, []byte(`{"t":"hello","id":"2","d":{"client":{"deviceId":"phone"},"pushToken":"p2"}}`))
	drainEnv(t, c)

	if got := c.device(); got != "phone" {
		t.Errorf("identity = %q, want phone", got)
	}
	if got := h.tokens.targetsExcluding(nil)["phone"]; got != "p2" {
		t.Errorf("push token = %q, want p2", got)
	}
	if isClosed(c.done) {
		t.Fatal("matching hello closed the socket")
	}
	revokeVia(t, h, admin, "phone")
	if !isClosed(c.done) {
		t.Error("revoke did not close the device's socket")
	}
}

// Requirement 5: a revoke that lands after authenticate() but before the socket
// is published must still close it.
func TestHelloIdentity_RevokeDuringAdmissionClosesSocket(t *testing.T) {
	h := newTestHub()
	phone := h.devices.pair("phone", "")
	admin := h.devices.pair("admin", "")

	revokeVia(t, h, admin, "phone") // runs while the socket is not yet in h.clients

	c := fakeClientFor(h)
	if h.admitClient(c, phone) {
		t.Fatal("admitClient admitted a device revoked during its connect")
	}
	if !isClosed(c.done) {
		t.Error("socket of a device revoked during connect is open")
	}
	h.mu.RLock()
	_, live := h.clients[c]
	h.mu.RUnlock()
	if live {
		t.Error("refused socket is still registered in h.clients")
	}
}

package app

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"foci/internal/log"
)

const (
	deviceTokenBytes = 32              // 256-bit device tokens
	authFailMax      = 10              // auth failures per window before lockout
	authFailWindow   = 5 * time.Minute // lockout / counting window
)

// device is one paired device's credential record.
type device struct {
	DeviceID string    `json:"deviceId"`
	Token    string    `json:"token"`
	Label    string    `json:"label"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"lastSeen"`
}

// deviceInfo is the token-free view returned by the devices listing.
type deviceInfo struct {
	DeviceID string    `json:"deviceId"`
	Label    string    `json:"label"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"lastSeen"`
}

// deviceStore holds per-device pairing tokens (auth hardening, §4). The shared
// master key mints these once over TLS (POST /app/pair); thereafter a device
// authenticates with its own revocable token. Persisted to a JSON file so
// pairings survive restarts (foci restarts on every deploy). path == "" keeps
// the store purely in-memory.
type deviceStore struct {
	path  string
	mu    sync.Mutex
	byID  map[string]*device
	byTok map[string]*device
}

func newDeviceStore(path string) *deviceStore {
	s := &deviceStore{path: path, byID: make(map[string]*device), byTok: make(map[string]*device)}
	s.load()
	return s
}

func (s *deviceStore) load() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return // absent = no devices paired yet
	}
	var list []*device
	if err := json.Unmarshal(data, &list); err != nil {
		log.Warnf("app", "device store %s: %v", s.path, err)
		return
	}
	for _, d := range list {
		if d.DeviceID == "" || d.Token == "" {
			continue
		}
		s.byID[d.DeviceID] = d
		s.byTok[d.Token] = d
	}
	log.Infof("app", "loaded %d paired device(s)", len(s.byID))
}

// saveLocked writes the store to disk atomically (temp + rename). Caller holds s.mu.
func (s *deviceStore) saveLocked() {
	if s.path == "" {
		return
	}
	list := make([]*device, 0, len(s.byID))
	for _, d := range s.byID {
		list = append(list, d)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		log.Errorf("app", "device store marshal: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Errorf("app", "device store write: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Errorf("app", "device store rename: %v", err)
	}
}

func newDeviceToken() string {
	b := make([]byte, deviceTokenBytes)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// pair mints (or re-mints, replacing the old token) a token for deviceID.
func (s *deviceStore) pair(deviceID, label string) *device {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.byID[deviceID]; ok {
		delete(s.byTok, old.Token)
	}
	now := time.Now()
	d := &device{DeviceID: deviceID, Token: newDeviceToken(), Label: label, Created: now, LastSeen: now}
	s.byID[deviceID] = d
	s.byTok[d.Token] = d
	s.saveLocked()
	return d
}

// validToken returns the device for a token (and refreshes lastSeen). Lookup is
// by map on a 256-bit random token, so it is not byte-by-byte comparable; the
// token's entropy — not constant-time compare — is the defense here.
func (s *deviceStore) validToken(token string) (*device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.byTok[token]
	if !ok {
		return nil, false
	}
	d.LastSeen = time.Now()
	return d, true
}

// revoke removes a device by id, returning it so the caller can tear down any
// live socket.
func (s *deviceStore) revoke(deviceID string) (*device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.byID[deviceID]
	if !ok {
		return nil, false
	}
	delete(s.byID, deviceID)
	delete(s.byTok, d.Token)
	s.saveLocked()
	return d, true
}

func (s *deviceStore) list() []deviceInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]deviceInfo, 0, len(s.byID))
	for _, d := range s.byID {
		out = append(out, deviceInfo{DeviceID: d.DeviceID, Label: d.Label, Created: d.Created, LastSeen: d.LastSeen})
	}
	return out
}

// --- auth failure rate limiter ---

type failWindow struct {
	count int
	until time.Time
}

// authLimiter locks out a remote IP after too many auth failures within a
// window — the endpoint is internet-facing, so brute force is throttled.
type authLimiter struct {
	mu     sync.Mutex
	fails  map[string]*failWindow
	max    int
	window time.Duration
}

func newAuthLimiter(max int, window time.Duration) *authLimiter {
	return &authLimiter{fails: make(map[string]*failWindow), max: max, window: window}
}

func (l *authLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.fails[ip]
	if !ok {
		return false
	}
	if time.Now().After(f.until) {
		delete(l.fails, ip)
		return false
	}
	return f.count >= l.max
}

func (l *authLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.fails[ip]
	if !ok || time.Now().After(f.until) {
		f = &failWindow{}
		l.fails[ip] = f
	}
	f.count++
	f.until = time.Now().Add(l.window)
}

func (l *authLimiter) reset(ip string) {
	l.mu.Lock()
	delete(l.fails, ip)
	l.mu.Unlock()
}

// remoteIP extracts the client IP for rate-limiting. foci sits behind Traefik,
// which APPENDS the downstream socket address to the right of X-Forwarded-For;
// everything to the left of that final hop is attacker-controlled. We therefore
// trust the RIGHTMOST entry (Traefik's appended hop = the real client). Taking
// the leftmost would let an attacker rotate a spoofed XFF to land in a fresh
// rate-limit bucket each request, defeating the brute-force lockout. Falls back
// to RemoteAddr when no proxy header is present (direct/local connections).
func remoteIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.LastIndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[i+1:])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

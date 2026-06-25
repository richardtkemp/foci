package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"foci/internal/app/fap"
	"foci/internal/log"
)

const (
	fcmScope            = "https://www.googleapis.com/auth/firebase.messaging"
	defaultPushCoalesce = 15 * time.Second // at most one wake push per conversation per window
	pushPreviewMax      = 80               // hard cap on the preview hint length
	fcmSendTimeout      = 10 * time.Second
)

// pushTokens is the in-memory deviceId→FCM-token registry. The client re-sends
// its token in every ClientHello, so the map repopulates on each connect — no
// persistence needed for v1 (a device only needs a push while it is offline, and
// it registered its token the last time it was online).
type pushTokens struct {
	mu     sync.RWMutex
	tokens map[string]string // deviceId → FCM registration token
}

func newPushTokens() *pushTokens { return &pushTokens{tokens: make(map[string]string)} }

func (p *pushTokens) set(deviceID, token string) {
	if deviceID == "" || token == "" {
		return
	}
	p.mu.Lock()
	p.tokens[deviceID] = token
	p.mu.Unlock()
}

func (p *pushTokens) all() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.tokens))
	for _, t := range p.tokens {
		out = append(out, t)
	}
	return out
}

// fcmPusher sends data-only FCM v1 messages, authenticating with a
// service-account token source that refreshes itself. A push is only a WAKE
// HINT: it carries the conversationId and a short preview, never the full agent
// text — the app reconnects and replays for the real content (§5/§6).
type fcmPusher struct {
	projectID string
	ts        oauth2.TokenSource
	http      *http.Client
	ctx       context.Context
	tokens    *pushTokens
	window    time.Duration // coalescing window (one wake push per conv per window)
	baseURL   string        // FCM v1 endpoint base; overridable in tests

	mu       sync.Mutex
	lastPush map[string]time.Time // convID → last push time (coalescing)
}

// fcmBaseURL is the production FCM v1 send endpoint base.
const fcmBaseURL = "https://fcm.googleapis.com"

// newFCMPusher builds a pusher from a service-account JSON file. Returns nil (push
// disabled, gracefully) if path is empty or the credentials can't be loaded.
func newFCMPusher(ctx context.Context, path string, tokens *pushTokens, window time.Duration) *fcmPusher {
	if path == "" {
		return nil
	}
	if window <= 0 {
		window = defaultPushCoalesce
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Warnf("app", "fcm credentials %s: %v — push disabled", path, err)
		return nil
	}
	creds, err := google.CredentialsFromJSON(ctx, data, fcmScope)
	if err != nil {
		log.Warnf("app", "fcm credentials parse: %v — push disabled", err)
		return nil
	}
	projectID := creds.ProjectID
	if projectID == "" {
		var sa struct {
			ProjectID string `json:"project_id"`
		}
		_ = json.Unmarshal(data, &sa)
		projectID = sa.ProjectID
	}
	if projectID == "" {
		log.Warnf("app", "fcm: no project_id in credentials — push disabled")
		return nil
	}
	log.Infof("app", "FCM push enabled (project %s)", projectID)
	return &fcmPusher{
		projectID: projectID,
		ts:        creds.TokenSource,
		http:      &http.Client{Timeout: fcmSendTimeout},
		ctx:       ctx,
		tokens:    tokens,
		window:    window,
		lastPush:  make(map[string]time.Time),
	}
}

// notify fires a coalesced wake push for a conversation that received offline
// content. Coalescing drops repeat pushes for the same conversation inside the
// quiet window — the app reconnects + replays, so a single wake suffices.
func (p *fcmPusher) notify(convID, preview string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if time.Since(p.lastPush[convID]) < p.window {
		p.mu.Unlock()
		return
	}
	p.lastPush[convID] = time.Now()
	p.mu.Unlock()

	for _, tok := range p.tokens.all() {
		safeGo("fcm-push", func() { p.send(tok, convID, preview) })
	}
}

func (p *fcmPusher) send(token, convID, preview string) {
	tok, err := p.ts.Token()
	if err != nil {
		log.Warnf("app", "fcm token: %v", err)
		return
	}
	body, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"token": token,
			"data": map[string]string{
				"conversationId": convID,
				"preview":        preview,
			},
			"android": map[string]any{"priority": "high"},
		},
	})
	if err != nil {
		return
	}
	base := p.baseURL
	if base == "" {
		base = fcmBaseURL
	}
	url := fmt.Sprintf("%s/v1/projects/%s/messages:send", base, p.projectID)
	req, err := http.NewRequestWithContext(p.ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	tok.SetAuthHeader(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		log.Warnf("app", "fcm send: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusMultipleChoices {
		// 404/410 indicate a stale token; pruning is a later refinement.
		log.Warnf("app", "fcm send: status %d", resp.StatusCode)
	}
}

// pushPreview classifies a server frame for offline push. It returns a short
// preview hint and true for user-visible frames (those a human should be woken
// for), or ("", false) for control/streaming frames that carry no notification
// value on their own.
func pushPreview(frame fap.ServerFrame) (string, bool) {
	switch f := frame.(type) {
	case fap.ServerMessage:
		return truncatePreview(f.Text), true
	case fap.TextEnd:
		if f.FinalText != nil {
			return truncatePreview(*f.FinalText), true
		}
		return "New message", true
	case fap.Media:
		if f.Caption != "" {
			return truncatePreview(f.Caption), true
		}
		return "Sent " + mediaNoun(f.MIME), true
	case fap.Notification:
		return truncatePreview(f.Text), true
	case fap.Interactive:
		return truncatePreview(f.Text), true
	default:
		// typing, meta, turn.start, text.delta, session.update, error, pong
		return "", false
	}
}

// mediaNoun returns a short human noun for a media MIME, for push previews.
// Derived from the MIME top-level type — the Media frame no longer carries a kind.
func mediaNoun(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "a photo"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.HasPrefix(mime, "video/"):
		return "a video"
	default:
		return "a file"
	}
}

func truncatePreview(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= pushPreviewMax {
		return s
	}
	return s[:pushPreviewMax] + "…"
}

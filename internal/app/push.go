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

	"foci/internal/config"
	"foci/internal/fap"
)

const (
	fcmScope            = "https://www.googleapis.com/auth/firebase.messaging"
	defaultPushCoalesce = 15 * time.Second // at most one wake push per conversation per device per window
	pushPreviewMax      = 80               // hard cap on the preview hint length
	fcmSendTimeout      = 10 * time.Second
	fcmMaxAttempts      = 3                      // total send attempts before giving up
	fcmRetryBase        = 500 * time.Millisecond // backoff doubles each retry (0.5s, 1s, …)
	defaultReadWakeWait = 5 * time.Second        // read wakes: wait this long after a chat's first read, then send its latest watermark
)

// Values of the FCM data payload's "type" key. A client that predates the key
// treats every push as a message wake, so a non-message type may be sent only to
// clients that check it (foci-client #2182 adds the check).
const (
	pushTypeMessage = "message"
	pushTypeRead    = "read"
)

// featureReadWake is the ClientHello feature a client advertises when it
// handles a "read" push without posting a notification (foci-client #2182).
// Older clients show every push as "New message", so read wakes go only to
// devices whose latest hello carried it.
const featureReadWake = "readWake"

// pushTokens is the in-memory deviceId→FCM-token registry. The client re-sends
// its token in every ClientHello, so the map repopulates on each connect — no
// persistence needed for v1 (a device only needs a push while it is offline, and
// it registered its token the last time it was online).
type pushTokens struct {
	mu       sync.RWMutex
	tokens   map[string]string // deviceId → FCM registration token
	readWake map[string]bool   // deviceId → its latest hello advertised featureReadWake
}

func newPushTokens() *pushTokens {
	return &pushTokens{tokens: make(map[string]string), readWake: make(map[string]bool)}
}

// setReadWake records whether deviceID's latest hello advertised
// featureReadWake. Set on every hello, so a device downgraded to an older build
// stops receiving read wakes at its next connect.
func (p *pushTokens) setReadWake(deviceID string, ok bool) {
	if deviceID == "" {
		return
	}
	p.mu.Lock()
	if ok {
		p.readWake[deviceID] = true
	} else {
		delete(p.readWake, deviceID)
	}
	p.mu.Unlock()
}

// readWakeTargetsExcluding is targetsExcluding limited to devices that handle a
// read wake (featureReadWake).
func (p *pushTokens) readWakeTargetsExcluding(exclude map[string]bool) map[string]string {
	out := p.targetsExcluding(exclude)
	p.mu.RLock()
	defer p.mu.RUnlock()
	for id := range out {
		if !p.readWake[id] {
			delete(out, id)
		}
	}
	return out
}

func (p *pushTokens) set(deviceID, token string) {
	if deviceID == "" || token == "" {
		return
	}
	p.mu.Lock()
	p.tokens[deviceID] = token
	p.mu.Unlock()
}

// targetsExcluding returns deviceId→registration token for every device NOT in
// exclude — the currently-connected devices, which receive frames over their
// live socket and so need no wake push. Keyed by device because the pusher
// coalesces per device, not per token.
func (p *pushTokens) targetsExcluding(exclude map[string]bool) map[string]string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]string, len(p.tokens))
	for id, t := range p.tokens {
		if exclude[id] {
			continue
		}
		out[id] = t
	}
	return out
}

// removeByToken drops every deviceId whose registration token is token — called
// when FCM reports it stale (404/410) so we stop retrying a dead token. The
// device re-registers a fresh one in its next ClientHello.
func (p *pushTokens) removeByToken(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, t := range p.tokens {
		if t == token {
			delete(p.tokens, id)
		}
	}
}

// pushPayload carries the full push context from the conversation binding to
// the pusher. The pusher resolves display names (AgentName, SessionTitle) via
// the Hub before sending, so the client can render a rich notification without
// a local DB lookup. ChatID is internal (used for alias resolution) and never
// sent in the FCM wire payload.
type pushPayload struct {
	ConvID       string
	Preview      string
	AgentID      string
	AgentName    string
	SessionKey   string
	SessionTitle string
	ChatID       int64 // internal; not sent to FCM
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
	window    time.Duration // coalescing window (one wake push per conv per device per window)
	baseURL   string        // FCM v1 endpoint base; overridable in tests
	retryBase time.Duration // backoff base; overridable in tests (0 → fcmRetryBase)

	mu       sync.Mutex
	lastPush map[pushKey]time.Time // (conv, device) → last push time (coalescing)

	readWait     time.Duration // read-wake batching delay; 0 → defaultReadWakeWait (overridable in tests)
	readMu       sync.Mutex
	pendingReads map[string]string // convID → newest read watermark waiting for its read wake
}

// pushKey is the coalescing key: one wake per conversation PER DEVICE. Keying by
// conversation alone let a push to one device (or an earlier wake the device
// already acted on) swallow the next wake another device needed (#1204).
type pushKey struct {
	convID   string
	deviceID string
}

// fcmBaseURL is the production FCM v1 send endpoint base.
const fcmBaseURL = "https://fcm.googleapis.com"

// googleJWTTokenURL is the default OAuth token endpoint Google's service-account
// JWT flow uses when a credential's token_uri is empty (mirrors
// golang.org/x/oauth2/google.JWTTokenURL, unexported by that package).
const googleJWTTokenURL = "https://oauth2.googleapis.com/token"

// newFCMPusher builds a pusher from a service-account JSON file. Returns nil (push
// disabled, gracefully) if path is empty or the credentials can't be loaded.
func newFCMPusher(ctx context.Context, path string, tokens *pushTokens, window time.Duration) *fcmPusher {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		appLog.Warnf("fcm credentials %s: %v — push disabled", path, err)
		return nil
	}
	return newFCMPusherFromJSON(ctx, data, tokens, window)
}

// fcmCredentialFields holds the individual Google service-account fields needed
// to authenticate FCM v1, as stored (one secret key per field) in secrets.toml —
// never the raw JSON file and never a file path. Field names mirror the
// standard Google service-account JSON schema; those names are public schema,
// not secret, only the values are.
type fcmCredentialFields struct {
	ProjectID    string // app.fcm_project_id      (required)
	ClientEmail  string // app.fcm_client_email     (required)
	PrivateKey   string // app.fcm_private_key      (required)
	PrivateKeyID string // app.fcm_private_key_id   (optional; sets the JWT "kid" header)
	TokenURI     string // app.fcm_token_uri        (optional; defaults to Google's token endpoint)
}

// normalizeFCMPrivateKey fixes the classic private_key footgun: a PEM key
// pasted into a single-line secrets.toml string often carries literal two-char
// `\n` sequences (an artifact of copying it out of a JSON file) instead of real
// newlines. PEM body content is base64 plus header/footer text, which never
// legitimately contains a backslash, so unconditionally turning `\n` into a
// real newline is safe — it's a no-op for a key that already has real
// newlines (e.g. pasted as a TOML triple-quoted multi-line string).
func normalizeFCMPrivateKey(key string) string {
	return strings.ReplaceAll(key, `\n`, "\n")
}

// newFCMPusherFromFields reconstructs the minimal service-account JSON in
// memory from decomposed secret-store fields and builds a pusher from it — no
// path or file is read. Returns nil if any required field is missing or the
// reconstructed credential fails to parse.
func newFCMPusherFromFields(ctx context.Context, fields fcmCredentialFields, tokens *pushTokens, window time.Duration) *fcmPusher {
	if fields.ProjectID == "" || fields.ClientEmail == "" || fields.PrivateKey == "" {
		return nil
	}
	sa := struct {
		Type         string `json:"type"`
		ProjectID    string `json:"project_id"`
		PrivateKeyID string `json:"private_key_id,omitempty"`
		PrivateKey   string `json:"private_key"`
		ClientEmail  string `json:"client_email"`
		TokenURI     string `json:"token_uri,omitempty"`
	}{
		Type:         "service_account",
		ProjectID:    fields.ProjectID,
		PrivateKeyID: fields.PrivateKeyID,
		PrivateKey:   normalizeFCMPrivateKey(fields.PrivateKey),
		ClientEmail:  fields.ClientEmail,
		TokenURI:     fields.TokenURI,
	}
	if sa.TokenURI == "" {
		sa.TokenURI = googleJWTTokenURL
	}
	data, err := json.Marshal(sa)
	if err != nil {
		appLog.Warnf("fcm credentials (secret fields): marshal: %v — push disabled", err)
		return nil
	}
	return newFCMPusherFromJSON(ctx, data, tokens, window)
}

// newFCMPusherForApp resolves the FCM credential in priority order:
//  1. decomposed secret-store fields (app.fcm_project_id / app.fcm_client_email
//     / app.fcm_private_key, plus optional app.fcm_private_key_id /
//     app.fcm_token_uri) — the preferred path (#967): the credential content
//     never touches foci.toml or disk as a file.
//  2. [platforms.app].fcm_credentials — a service-account JSON file path
//     configured in foci.toml.
//  3. the legacy app.fcm_credentials secret holding a file path (pre-#967
//     compat, superseded by (1) but kept so an already-configured deployment
//     that only set this keeps working).
//
// Returns nil (push disabled, gracefully) if none resolve.
func newFCMPusherForApp(ctx context.Context, appCfg *config.AppSpecific, secrets config.SecretGetter, tokens *pushTokens, window time.Duration) *fcmPusher {
	if secrets != nil {
		var fields fcmCredentialFields
		fields.ProjectID, _ = secrets.Get("app.fcm_project_id")
		fields.ClientEmail, _ = secrets.Get("app.fcm_client_email")
		fields.PrivateKey, _ = secrets.Get("app.fcm_private_key")
		fields.PrivateKeyID, _ = secrets.Get("app.fcm_private_key_id")
		fields.TokenURI, _ = secrets.Get("app.fcm_token_uri")
		if p := newFCMPusherFromFields(ctx, fields, tokens, window); p != nil {
			return p
		}
	}
	fcmPath := ""
	if appCfg != nil {
		fcmPath = appCfg.FCMCredentials
	}
	if fcmPath == "" && secrets != nil {
		if v, ok := secrets.Get("app.fcm_credentials"); ok {
			fcmPath = strings.TrimSpace(v)
		}
	}
	return newFCMPusher(ctx, fcmPath, tokens, window)
}

// newFCMPusherFromJSON builds a pusher from already-loaded service-account JSON
// bytes (from a file or reconstructed from secret-store fields). Returns nil
// (push disabled, gracefully) if the credentials can't be parsed.
func newFCMPusherFromJSON(ctx context.Context, data []byte, tokens *pushTokens, window time.Duration) *fcmPusher {
	if window <= 0 {
		window = defaultPushCoalesce
	}
	creds, err := google.CredentialsFromJSON(ctx, data, fcmScope)
	if err != nil {
		appLog.Warnf("fcm credentials parse: %v — push disabled", err)
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
		appLog.Warnf("fcm: no project_id in credentials — push disabled")
		return nil
	}
	appLog.Infof("FCM push enabled (project %s)", projectID)
	return &fcmPusher{
		projectID: projectID,
		ts:        creds.TokenSource,
		http:      &http.Client{Timeout: fcmSendTimeout},
		ctx:       ctx,
		tokens:    tokens,
		window:    window,
		retryBase: fcmRetryBase,
		lastPush:  make(map[pushKey]time.Time),
	}
}

// notify fires a coalesced wake push for a conversation that received offline
// content. Coalescing drops repeat pushes to the same device for the same
// conversation inside the quiet window — the app reconnects + replays, so a
// single wake suffices. It is per device: each offline device gets its own
// window, and deviceConnected clears a device's windows once it has caught up.
func (p *fcmPusher) notify(payload pushPayload, exclude map[string]bool) {
	if p == nil {
		return
	}
	// Wake every capable device that isn't already connected. A device with a
	// live socket receives the frame over it, so a connected desktop must not
	// suppress an offline phone's wake. Coalesce only when there's a real target:
	// all-connected traffic must not bump the window and drop a later push for a
	// device that goes offline within it.
	targets := p.tokens.targetsExcluding(exclude)
	if len(targets) == 0 {
		return
	}
	now := time.Now()
	send := make([]string, 0, len(targets))
	p.mu.Lock()
	for deviceID, tok := range targets {
		k := pushKey{convID: payload.ConvID, deviceID: deviceID}
		if now.Sub(p.lastPush[k]) < p.window {
			continue
		}
		p.lastPush[k] = now
		send = append(send, tok)
	}
	p.mu.Unlock()

	for _, tok := range send {
		safeGo("fcm-push", func() { p.send(tok, payload) })
	}
}

// deviceConnected clears every coalescing window held for deviceID. A device
// that has just connected replays everything it missed, so the wakes it was sent
// are consumed; without this, a phone woken at T0 that fetched and dropped its
// socket would get no push for a new message at T0+10s (still inside the window).
func (p *fcmPusher) deviceConnected(deviceID string) {
	if p == nil || deviceID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.lastPush {
		if k.deviceID == deviceID {
			delete(p.lastPush, k)
		}
	}
}

// notifyRead queues a read wake for convID (#2182): another device moved the
// chat's read watermark, and a device whose socket is released (a backgrounded
// phone) would otherwise keep showing that chat's notification until its next
// connect. Reads arrive in bursts as the user scrolls, so the first read of a
// quiet chat starts a readWait timer and later reads only replace the pending
// watermark; when it fires, ONE wake carrying the newest watermark goes to every
// device without a live socket at that moment (connected is evaluated then, not
// now: a device that connects in between hears the ReadSync over its socket)
// and whose latest hello advertised featureReadWake. The wait is not reset by later reads, so a long scroll still clears within
// readWait.
func (p *fcmPusher) notifyRead(convID, messageID string, connected func() map[string]bool) {
	if p == nil || convID == "" || messageID == "" {
		return
	}
	p.readMu.Lock()
	if p.pendingReads == nil {
		p.pendingReads = make(map[string]string)
	}
	_, scheduled := p.pendingReads[convID]
	p.pendingReads[convID] = messageID
	p.readMu.Unlock()
	if scheduled {
		return
	}
	wait := p.readWait
	if wait <= 0 {
		wait = defaultReadWakeWait
	}
	time.AfterFunc(wait, func() {
		defer recoverApp("fcm-read-wake")
		p.flushRead(convID, connected)
	})
}

// flushRead sends convID's pending read wake. See notifyRead.
func (p *fcmPusher) flushRead(convID string, connected func() map[string]bool) {
	p.readMu.Lock()
	messageID, ok := p.pendingReads[convID]
	delete(p.pendingReads, convID)
	p.readMu.Unlock()
	if !ok || p.ctx.Err() != nil {
		return
	}
	var exclude map[string]bool
	if connected != nil {
		exclude = connected()
	}
	for _, tok := range p.tokens.readWakeTargetsExcluding(exclude) {
		safeGo("fcm-read-wake", func() { p.sendData(tok, readWakeData(convID, messageID), readWakeAndroid(convID)) })
	}
}

// readWakeData is a read wake's FCM data payload. The client posts no
// notification for it: it reconnects, and the post-hello read replay clears the
// chat's notification once the chat is fully read. messageId is the new
// watermark (informational: the reconnect delivers the authoritative one).
func readWakeData(convID, messageID string) map[string]string {
	return map[string]string{
		"type":           pushTypeRead,
		"conversationId": convID,
		"messageId":      messageID,
	}
}

// readWakeAndroid is a read wake's android block. Normal priority, because
// Android deprioritises an app whose high-priority pushes do not show a
// notification, which would slow the message wakes that matter; clearing a
// notification can wait for the device's next maintenance window. The
// per-chat collapse key keeps only the newest read wake while the device is
// unreachable.
func readWakeAndroid(convID string) map[string]any {
	return map[string]any{"priority": "normal", "collapse_key": "read:" + convID}
}

func (p *fcmPusher) send(token string, payload pushPayload) {
	p.sendData(token, map[string]string{
		"type":           pushTypeMessage,
		"conversationId": payload.ConvID,
		"preview":        payload.Preview,
		"agentId":        payload.AgentID,
		"agentName":      payload.AgentName,
		"sessionKey":     payload.SessionKey,
		"sessionTitle":   payload.SessionTitle,
	}, map[string]any{"priority": "high"})
}

// sendData sends one data-only FCM message to token, retrying transient failures.
func (p *fcmPusher) sendData(token string, data map[string]string, android map[string]any) {
	tok, err := p.ts.Token()
	if err != nil {
		appLog.Warnf("fcm token: %v", err)
		return
	}
	body, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"token":   token,
			"data":    data,
			"android": android,
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

	backoff := p.retryBase
	if backoff <= 0 {
		backoff = fcmRetryBase
	}
	for attempt := 1; ; attempt++ {
		if !p.sendOnce(url, token, tok, body) {
			return // success or permanent outcome — done
		}
		if attempt >= fcmMaxAttempts {
			appLog.Warnf("fcm send: giving up after %d attempts", fcmMaxAttempts)
			return
		}
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// sendOnce performs one FCM send attempt. It returns true only when the failure is
// transient and worth retrying — a transport error (timeout, reset) or a 5xx/429
// from FCM. Success, and permanent failures (a dead token, pruned here; any other
// 4xx), return false so the caller stops.
func (p *fcmPusher) sendOnce(url, token string, tok *oauth2.Token, body []byte) (retry bool) {
	req, err := http.NewRequestWithContext(p.ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	tok.SetAuthHeader(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		appLog.Warnf("fcm send: %v", err)
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusMultipleChoices {
		return false
	}
	appLog.Warnf("fcm send: status %d", resp.StatusCode)
	// 404 (unregistered) / 410 (gone) mean the token is dead — prune it so we
	// stop retrying; the device re-registers on its next ClientHello.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		p.tokens.removeByToken(token)
		return false
	}
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError
}

// pushPreview classifies a server frame for offline push. It returns a short
// preview hint and true for user-visible frames (those a human should be woken
// for), or ("", false) for control/streaming frames that carry no notification
// value on their own.
// isOwnMessage reports whether a frame is a message the user themselves sent
// (role "user"), echoed back to their devices. Such frames must NOT fire a wake
// push — see convBinding.send. Only ServerMessage carries a role; every other
// frame kind is agent/system-authored, so returns false.
func isOwnMessage(frame fap.ServerFrame) bool {
	sm, ok := frame.(fap.ServerMessage)
	return ok && sm.Role == "user"
}

func pushPreview(frame fap.ServerFrame) (string, bool) {
	switch f := frame.(type) {
	case fap.ServerMessage:
		if len(f.Attachments) > 0 {
			// A user echo carrying files (#2161) previews like a Media frame, from
			// its first attachment, noting the rest ("report.pdf +2 — caption") as
			// the app's MediaPreview.mediaListLabel does for a multi-file send.
			a := f.Attachments[0]
			more := ""
			if n := len(f.Attachments); n > 1 {
				more = fmt.Sprintf(" +%d", n-1)
			}
			return mediaPreview(a.MIME, a.Name, f.Text, more), true
		}
		return truncatePreview(f.Text), true
	case fap.TextEnd:
		if f.FinalText != nil {
			return truncatePreview(*f.FinalText), true
		}
		return "New message", true
	case fap.Media:
		// #1061: prefer the real filename over a generic "Sent a file", and fold in any
		// caption ("report.pdf — here you go"). Mirrors the android live-frame preview
		// (MediaPreview.mediaListLabel) so the reconnect/hello seed matches what the app
		// shows for live messages. With no filename, a caption is shown alone, else a
		// "Sent a <noun>" fallback.
		return mediaPreview(f.MIME, f.Name, f.Caption, ""), true
	case fap.Notification:
		return truncatePreview(f.Text), true
	case fap.Interactive:
		// Batched asks carry their text in Questions[0].Text (f.Text is empty);
		// sequential asks use f.Text. Fall back to a generic "Question from agent"
		// when neither has content (shouldn't happen, but guard against empty pushes).
		if f.Text != "" {
			return truncatePreview(f.Text), true
		}
		if len(f.Questions) > 0 && f.Questions[0].Text != "" {
			return truncatePreview(f.Questions[0].Text), true
		}
		return "Question from agent", true
	default:
		// typing, thinking, warming, tool, meta, turn.start, text.delta, error, pong
		return "", false
	}
}

// mediaPreview labels a media message: its filename, else the caption alone,
// else "Sent <noun>"; a caption follows a filename after " — ". more (e.g.
// " +2") is appended to the filename or the noun, for a multi-file message.
func mediaPreview(mime, name, caption, more string) string {
	name = strings.TrimSpace(name)
	caption = strings.TrimSpace(caption)
	if name == "" {
		if caption != "" {
			return truncatePreview(caption)
		}
		return "Sent " + mediaNoun(mime) + more
	}
	if caption != "" {
		return truncatePreview(name + more + " — " + caption)
	}
	return truncatePreview(name + more)
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

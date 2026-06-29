package app

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"foci/internal/agent"
	"foci/internal/app/fap"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
)

// WebSocket tuning. pingPeriod < pongWait so a silent socket is detected
// within one missed pong (dead-socket detection without relying on a clean
// close — mirrors the client's pingInterval).
const (
	writeWait    = 10 * time.Second
	pongWait     = 60 * time.Second
	pingPeriod   = (pongWait * 9) / 10
	maxFrameSize = 1 << 20 // 1 MiB inbound frame cap
	sendBuffer   = 256     // per-socket outbound queue depth
	// How long enqueue blocks for queue space before giving up on a stalled socket
	// and closing it (so the client reconnects and replays). Bounded so a slow
	// consumer can't wedge the producing turn indefinitely.
	enqueueBlockWait = 2 * time.Second
)

// Reliability tuning (wire-protocol §3). The replay buffer + inbound dedup
// window turn at-least-once delivery into effectively exactly-once rendering on
// a phone that drops the socket constantly. These are the code defaults;
// [platforms.app] replay_buffer / replay_ttl override them per the config cascade.
const (
	defaultReplayBufferDepth = 1000                // max retained server frames per conversation (in-memory)
	defaultReplayTTL         = 24 * time.Hour      // max age of a retained in-memory frame
	defaultReplayStoreTTL    = 30 * 24 * time.Hour // max age of a durably-stored frame (backfill DB)
	defaultReplayStoreFile   = "app-frames.db"     // durable replay-frame DB, relative to data_dir
	maxSeenInbound           = 4096                // per-conversation inbound dedup window (mirrors client)
	defaultDevicesFile       = "app-devices.json"
	maxReplayPage            = 2000 // max frames returned per durable-store backfill read
)

// Hub multiplexes all live app WebSockets. It owns the per-agent appConn
// registry, the sessionKey→conversation binding map, and the set of physical
// sockets. It implements platform.ConnectionSource[*appConn] so the generic
// adapter can expose it as a platform.ConnectionManager.
type Hub struct {
	deps     platform.ProviderDeps
	pairKeys *pairKeyStore
	blobs    *blobStore
	tokens   *pushTokens
	pusher   *fcmPusher
	devices  *deviceStore
	authLim  *authLimiter

	host           string          // advertised in hello.caps.host (config [platforms.app].host)
	replayDepth    int             // config-driven replay buffer depth (0 = default)
	replayTTL      time.Duration   // config-driven replay buffer TTL (0 = default)
	frames         *frameStore     // durable replay-frame backstop (nil when no data_dir)
	allowedDevices map[string]bool // non-empty = pairing allowlist

	mu           sync.RWMutex
	agents       map[string]*appConn     // agentID → its connection
	agentOrder   []string                // registration order (for default-agent resolution)
	convs        map[string]*convBinding // conversationId → durable conversation state (outlives sockets)
	bySession    map[string]*convBinding // sessionKey → conversation binding
	clients      map[*wsClient]struct{}  // live sockets
	prompts      map[string]*convBinding // promptID → binding (live interactive prompts)
	batchPrompts map[string]*batchPrompt // promptID → batched-ask callback (app-only multi-question form)
	notifs       map[string]*convBinding // notification messageID → binding (for in-place edit, e.g. compaction ⏳→✅)
}

// featureInteractiveBatch is the ClientHello capability a client advertises to
// receive multi-question asks as one batched form (vs sequential prompts).
const featureInteractiveBatch = "interactiveBatch"

// batchPrompt holds the in-flight callback for one batched (multi-question) ask.
// The app returns all answers in a single InteractiveResponse.Answers; onResp
// feeds them back into the ask layer (tools.AskPresentBatchFn's onResponse).
type batchPrompt struct {
	b      *convBinding
	onResp func(answers []string)
}

func newHub(deps platform.ProviderDeps) *Hub {
	// Resolve the typed [platforms.app] config (nil = all code defaults).
	var appCfg *config.AppSpecific
	if deps.Config != nil {
		if p := deps.Config.Platform("app"); p != nil {
			appCfg = p.App
		}
	}

	tokens := newPushTokens()

	devicesFile := defaultDevicesFile
	if appCfg != nil && appCfg.DevicesPath != "" {
		devicesFile = appCfg.DevicesPath
	}
	devicePath := ""
	if deps.Config != nil && deps.Config.DataDir != "" {
		devicePath = filepath.Join(deps.Config.DataDir, devicesFile)
	}

	blobs := newBlobStore()
	if appCfg != nil {
		if appCfg.MaxBlobMB != nil && *appCfg.MaxBlobMB > 0 {
			blobs.maxBytes = int64(*appCfg.MaxBlobMB) << 20
		}
		blobs.ttl = durationOr(appCfg.BlobTTL, blobs.ttl)
	}

	// Durable replay-frame store (server-side backfill DB). Needs a data_dir;
	// absent → frames stays nil and every frameStore method no-ops, so the hub
	// degrades to the in-memory-only buffer it had before.
	var frames *frameStore
	if deps.Config != nil && deps.Config.DataDir != "" {
		storeFile := defaultReplayStoreFile
		storeTTL := defaultReplayStoreTTL
		if appCfg != nil {
			if appCfg.ReplayStorePath != "" {
				storeFile = appCfg.ReplayStorePath
			}
			storeTTL = durationOr(appCfg.ReplayStoreTTL, defaultReplayStoreTTL)
		}
		storePath := filepath.Join(deps.Config.DataDir, storeFile)
		if fs, err := newFrameStore(storePath, storeTTL); err != nil {
			log.Errorf("app", "durable replay store %s: %v — falling back to in-memory replay only", storePath, err)
		} else {
			frames = fs
		}
	}

	h := &Hub{
		frames:       frames,
		deps:         deps,
		pairKeys:     newPairKeyStore(),
		blobs:        blobs,
		tokens:       tokens,
		devices:      newDeviceStore(devicePath),
		authLim:      newAuthLimiter(authFailMax, authFailWindow),
		agents:       make(map[string]*appConn),
		convs:        make(map[string]*convBinding),
		bySession:    make(map[string]*convBinding),
		clients:      make(map[*wsClient]struct{}),
		prompts:      make(map[string]*convBinding),
		batchPrompts: make(map[string]*batchPrompt),
		notifs:       make(map[string]*convBinding),
	}
	if appCfg != nil {
		h.host = appCfg.Host
		if appCfg.ReplayBuffer != nil && *appCfg.ReplayBuffer > 0 {
			h.replayDepth = *appCfg.ReplayBuffer
		}
		h.replayTTL = durationOr(appCfg.ReplayTTL, 0)
		if len(appCfg.AllowedDevices) > 0 {
			h.allowedDevices = make(map[string]bool, len(appCfg.AllowedDevices))
			for _, id := range appCfg.AllowedDevices {
				h.allowedDevices[id] = true
			}
		}
	}

	if deps.Ctx != nil {
		safeGo("blob-reaper", func() { h.blobs.reaper(deps.Ctx) })
		if h.frames != nil {
			safeGo("frame-store-janitor", func() { h.frames.janitor(deps.Ctx.Done()) })
		}
		// Offline wake-push via FCM. The service-account JSON path comes from
		// [platforms.app].fcm_credentials, falling back to the app.fcm_credentials
		// secret; absent (or push=false) → push stays disabled gracefully.
		pushEnabled := appCfg == nil || appCfg.Push == nil || *appCfg.Push
		fcmPath := ""
		if appCfg != nil {
			fcmPath = appCfg.FCMCredentials
		}
		if fcmPath == "" && deps.SecretStore != nil {
			if v, ok := deps.SecretStore.Get("app.fcm_credentials"); ok {
				fcmPath = strings.TrimSpace(v)
			}
		}
		if !pushEnabled {
			fcmPath = "" // disabled → newFCMPusher returns nil
		}
		window := durationOr(appCfgPushCoalesce(appCfg), defaultPushCoalesce)
		h.pusher = newFCMPusher(deps.Ctx, fcmPath, tokens, window)
	}
	return h
}

// appCfgPushCoalesce returns the configured push-coalesce duration string, or "".
func appCfgPushCoalesce(c *config.AppSpecific) string {
	if c == nil {
		return ""
	}
	return c.PushCoalesce
}

// durationOr parses a duration string, returning fallback on empty or parse error.
func durationOr(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		log.Warnf("app", "bad duration %q: %v — using %s", s, err, fallback)
		return fallback
	}
	return d
}

// pushNotify fires a coalesced offline wake push for a conversation (no-op when
// push is unconfigured). Used as convBinding.notifyOffline.
func (h *Hub) pushNotify(convID, preview string) {
	h.pusher.notify(convID, preview)
}

// caps reports the server capability set advertised in `hello`, including the
// push transports available.
func (h *Hub) caps() fap.Caps {
	c := fap.Caps{Versions: []int{fap.ProtocolVersion}, Host: h.host}
	if h.pusher != nil {
		c.Push = []string{"fcm"}
	}
	h.mu.RLock()
	for _, conn := range h.agents {
		if conn.stt != nil {
			c.Features = append(c.Features, "voice")
			break
		}
	}
	h.mu.RUnlock()
	return c
}

// bearerToken extracts the Bearer credential from a request, or "" if absent.
func bearerToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return auth[len("Bearer "):]
	}
	return ""
}

// authToken validates a per-device token, returning the device and true, or
// (nil, false). There is no longer a shared master key (#862): every credential
// is a revocable per-device token (or, for pairing only, a single-use key —
// handled separately in ServePair).
func (h *Hub) authToken(token string) (*device, bool) {
	if token == "" {
		return nil, false
	}
	if h.devices != nil {
		if d, ok := h.devices.validToken(token); ok {
			return d, true
		}
	}
	return nil, false
}

// authenticate gates a request on a valid device token, with per-IP failure
// lockout. On success it returns the device and true; on failure it writes the
// HTTP error and returns false. (The active-hub nil check lives in withHub.)
func (h *Hub) authenticate(w http.ResponseWriter, r *http.Request) (*device, bool) {
	ip := remoteIP(r)
	if h.authLim.blocked(ip) {
		http.Error(w, "too many auth failures", http.StatusTooManyRequests)
		return nil, false
	}
	token := bearerToken(r)
	if token == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return nil, false
	}
	d, ok := h.authToken(token)
	if !ok {
		h.authLim.fail(ip)
		http.Error(w, "invalid credentials", http.StatusForbidden)
		return nil, false
	}
	h.authLim.reset(ip)
	return d, true
}

// ServePair handles POST /app/pair: a single-use, short-TTL pairing key (minted
// by the /android wizard or `foci` CLI, held in memory) is exchanged for a
// long-lived revocable per-device token. The pairing key is consumed on use.
func (h *Hub) ServePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := remoteIP(r)
	if h.authLim.blocked(ip) {
		http.Error(w, "too many auth failures", http.StatusTooManyRequests)
		return
	}
	if !h.pairKeys.consume(bearerToken(r)) {
		h.authLim.fail(ip)
		http.Error(w, "invalid or expired pairing key", http.StatusForbidden)
		return
	}
	h.authLim.reset(ip)
	var req struct {
		DeviceID  string `json:"deviceId"`
		Label     string `json:"label"`
		PushToken string `json:"pushToken"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.DeviceID == "" {
		http.Error(w, "bad pairing request", http.StatusBadRequest)
		return
	}
	if h.allowedDevices != nil && !h.allowedDevices[req.DeviceID] {
		log.Warnf("app", "pairing rejected: device %q not in allowed_devices", req.DeviceID)
		http.Error(w, "device not allowed", http.StatusForbidden)
		return
	}
	d := h.devices.pair(req.DeviceID, req.Label)
	if req.PushToken != "" {
		h.tokens.set(req.DeviceID, req.PushToken)
	}
	log.Infof("app", "paired device %q (label %q)", req.DeviceID, req.Label)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"deviceToken": d.Token, "label": d.Label})
}

// ServeDevices handles GET /app/devices: any paired device (its own token)
// lists the paired devices (tokens omitted). #862: management no longer needs a
// master key — a device manages from the token it already holds.
func (h *Hub) ServeDevices(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authenticate(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.devices.list())
}

// ServeRevoke handles POST /app/pair/revoke: any paired device (its own token)
// revokes a device's token and closes its live socket(s) with 4403. A device
// may revoke itself or any other paired device (#862).
func (h *Hub) ServeRevoke(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authenticate(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		DeviceID string `json:"deviceId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.DeviceID == "" {
		http.Error(w, "bad revoke request", http.StatusBadRequest)
		return
	}
	if _, ok := h.devices.revoke(req.DeviceID); !ok {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	h.closeDeviceSockets(req.DeviceID)
	log.Infof("app", "revoked device %q", req.DeviceID)
	w.WriteHeader(http.StatusNoContent)
}

// ServePushRegister handles POST /app/push/register: a device refreshes its FCM
// registration token out-of-band (wire §6). The OS can rotate the token while
// the app is offline, so relying on the next ClientHello is not enough — this
// lets the app push the new token immediately. Authenticated as any valid
// credential (master or device token).
func (h *Hub) ServePushRegister(w http.ResponseWriter, r *http.Request) {
	d, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		DeviceID  string `json:"deviceId"`
		PushToken string `json:"pushToken"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.PushToken == "" {
		http.Error(w, "bad push register request", http.StatusBadRequest)
		return
	}
	// A device token authenticates as itself; prefer its authoritative id.
	deviceID := req.DeviceID
	if d != nil {
		deviceID = d.DeviceID
	}
	if deviceID == "" {
		http.Error(w, "deviceId required", http.StatusBadRequest)
		return
	}
	h.tokens.set(deviceID, req.PushToken)
	w.WriteHeader(http.StatusNoContent)
}

// ServeHistory handles GET /app/history?conversationId=<id>: restart
// reconciliation (wire §9). The server's replay buffers are in-memory, so after
// a restart they are gone and a reconnect's resume points cannot be replayed.
// This returns the conversation's current server-side seq high-water (0 after a
// restart) so the offline-first client can detect the reset — if its locally
// rendered seq exceeds what the server reports, the server restarted and the
// app's Room DB is authoritative. Full message bodies live in the app's local
// store by design; this endpoint carries reconciliation state, not content.
func (h *Hub) ServeHistory(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authenticate(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	convID := r.URL.Query().Get("conversationId")
	if convID == "" {
		http.Error(w, "conversationId required", http.StatusBadRequest)
		return
	}
	resp := map[string]any{"conversationId": convID, "lastSeq": int64(0), "present": false}
	if b := h.convForReliability(convID); b != nil {
		b.mu.Lock()
		resp["lastSeq"] = b.seq
		resp["present"] = true
		resp["agentId"] = b.agentID
		resp["sessionKey"] = b.sessionKey
		b.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ServeReplay handles GET /app/replay?conversationId=<id>&fromSeq=<n>&limit=<n>:
// the content-bearing backfill path (wire §9). It returns the durably-stored
// frames with seq > fromSeq, in seq order, as their verbatim encoded wires — the
// app decodes and renders them exactly as live frames (idempotent: its inbound
// tracker dedups by seq). This is what closes the gap reconnect replay cannot
// when the in-memory buffer was trimmed or wiped by a restart. `more` is true
// when the page hit the limit, signalling the client to page again from the last
// returned seq. Distinct from /app/history (which carries reconciliation state,
// not content).
func (h *Hub) ServeReplay(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authenticate(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	convID := r.URL.Query().Get("conversationId")
	if convID == "" {
		http.Error(w, "conversationId required", http.StatusBadRequest)
		return
	}
	fromSeq, _ := strconv.ParseInt(r.URL.Query().Get("fromSeq"), 10, 64)
	limit := maxReplayPage
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v < maxReplayPage {
		limit = v
	}

	frames := h.frames.Range(convID, fromSeq, limit)
	out := make([]map[string]any, 0, len(frames))
	for _, f := range frames {
		out = append(out, map[string]any{"seq": f.seq, "wire": f.wire})
	}
	resp := map[string]any{
		"conversationId": convID,
		"fromSeq":        fromSeq,
		"frames":         out,
		"more":           len(frames) == limit,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// closeDeviceSockets force-closes every live socket for a deviceID with 4403.
func (h *Hub) closeDeviceSockets(deviceID string) {
	h.mu.RLock()
	var victims []*wsClient
	for c := range h.clients {
		c.mu.Lock()
		dev := c.deviceID
		c.mu.Unlock()
		if dev == deviceID {
			victims = append(victims, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range victims {
		c.closeWithCode(fap.CloseForbidden, "device revoked")
	}
}

// evictOtherDeviceSockets closes any OTHER live socket carrying the same
// deviceID as keep, using close code 4409 (wire §9: "on a second socket for the
// same device, close the older with 4409"). A phone that reconnects while its
// previous socket is still half-open (pre-pong-timeout) would otherwise leave
// two sockets attached to the same conversation bindings, double-delivering
// frames and breaking the exactly-once-render guarantee the reliability layer
// exists for. deviceID=="" (un-helloed socket) evicts nothing.
func (h *Hub) evictOtherDeviceSockets(keep *wsClient, deviceID string) {
	if deviceID == "" {
		return
	}
	h.mu.RLock()
	var victims []*wsClient
	for c := range h.clients {
		if c == keep {
			continue
		}
		c.mu.Lock()
		dev := c.deviceID
		c.mu.Unlock()
		if dev == deviceID {
			victims = append(victims, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range victims {
		log.Infof("app", "evicting older socket for device (replaced by new connection)")
		c.closeWithCode(fap.CloseReplaced, "replaced by newer connection")
	}
}

// setupAgent registers an agent's connection and starts its inbox. Returns nil
// if the handler is not an *agent.Agent (the app provider only serves the
// in-process agent core).
func (h *Hub) setupAgent(params platform.AgentConnectionParams) *appConn {
	ag, ok := params.Handler.(*agent.Agent)
	if !ok || ag == nil {
		return nil
	}
	conn := &appConn{hub: h, agentID: params.AgentID, agentRef: ag}
	if reg, ok := params.Commands.(*command.Registry); ok {
		conn.commands = reg
	}
	if cc, ok := params.CommandContext.(command.CommandContext); ok {
		conn.cmdCtx = cc
	}
	conn.stt = params.STT

	h.mu.Lock()
	if _, exists := h.agents[params.AgentID]; !exists {
		h.agentOrder = append(h.agentOrder, params.AgentID)
	}
	h.agents[params.AgentID] = conn
	h.mu.Unlock()

	// App is interactive (like telegram): in-flight messages steer the live turn.
	ag.SetInboxSteerMode(true)
	ag.StartInbox(h.deps.Ctx)
	return conn
}

// --- platform.ConnectionSource[*appConn] ---

func (h *Hub) PrimaryBot(agentID string) *appConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.agents[agentID]
}

// BotForSession returns a view of the agent's connection pinned to sessionKey.
// The stored per-agent instance is unbound (bound==""); this mints a shallow
// copy with bound set so session-blind sends (SendText/SendNotification/media)
// route to the right socket without a mutable shared "default session". The
// copy is a transient throwaway — nobody stores it — and is copy-safe now that
// appConn carries no mutex.
func (h *Hub) BotForSession(sessionKey string) *appConn {
	if sessionKey == "" {
		return nil
	}
	base := h.PrimaryBot(session.AgentIDFromKey(sessionKey))
	if base == nil {
		return nil
	}
	view := *base
	view.bound = sessionKey
	return &view
}

func (h *Hub) BotForSessionOrPrimary(sessionKey, agentID string) *appConn {
	if c := h.BotForSession(sessionKey); c != nil {
		return c
	}
	return h.PrimaryBot(agentID)
}

func (h *Hub) AcquireFacet(string) (*appConn, bool) { return nil, false } // facets: later slice
func (h *Hub) HasFacet(string) bool                 { return false }
func (h *Hub) StartAll(context.Context)             {}
func (h *Hub) Wait()                                {}

// Close shuts every live socket.
func (h *Hub) Close() error {
	h.mu.Lock()
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	for _, c := range clients {
		// Hub.Close runs only on graceful shutdown (the deploy/restart path: a
		// crash or kill -9 skips the main defer that leads here). Close with
		// 1012 ServerRestart so app clients can distinguish a deliberate bounce
		// from a network drop and use the fast restart-reconnect regime (wait
		// 5s, then retry every 1s for ~30s) instead of generic exponential
		// backoff (#900).
		c.closeWithCode(fap.CloseServerRestart, "server restarting")
	}
	// Drain + flush the durable replay store so a graceful shutdown (the deploy
	// path) persists every in-flight frame before exit.
	h.frames.Close()
	return nil
}

// ConnCount returns the number of live app sockets. The goroutine monitor uses
// it to budget its threshold dynamically: each socket runs a writePump
// goroutine plus its accept goroutine (readPump runs inline), and spawns
// transient per-turn goroutines, none of which the static startup formula can
// know — phones connect and disconnect freely.
func (h *Hub) ConnCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// defaultAgentID returns the first-registered agent — the binding target for a
// socket that has not explicitly opened a conversation for a named agent.
func (h *Hub) defaultAgentID() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.agentOrder) > 0 {
		return h.agentOrder[0]
	}
	return ""
}

func (h *Hub) agentRoster() []fap.AgentInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	order := append([]string(nil), h.agentOrder...)
	sort.Strings(order)

	// Per-agent default chat (app platform), memoised so each agent's index is
	// queried at most once per roster build. Marks the matching conversation's
	// IsDefault for the golden pin (#app-default).
	idx := h.deps.SessionIndex
	defaultChatByAgent := make(map[string]int64)
	defaultChat := func(agentID string) int64 {
		if idx == nil {
			return 0
		}
		if v, ok := defaultChatByAgent[agentID]; ok {
			return v
		}
		v := idx.DefaultChatForAgent(agentID, "app")
		defaultChatByAgent[agentID] = v
		return v
	}

	// Group live conversations by agent (the app's Room DB is the durable source
	// of its full thread list; the roster advertises agents + currently-bound
	// conversations and any the app resumes are re-attached on hello).
	byAgent := make(map[string][]fap.ConversationInfo)
	for _, b := range h.convs {
		ci := b.info()
		ci.Title = h.aliasFor(b)
		if dc := defaultChat(b.agentID); dc != 0 && dc == b.chatID {
			ci.IsDefault = true
		}
		byAgent[b.agentID] = append(byAgent[b.agentID], ci)
	}
	out := make([]fap.AgentInfo, 0, len(order))
	for _, id := range order {
		convs := byAgent[id]
		sort.Slice(convs, func(i, j int) bool { return convs[i].ID < convs[j].ID })
		name, emoji := h.agentDisplay(id)
		avatarURL, avatarVer := h.agentAvatarRef(id)
		out = append(out, fap.AgentInfo{
			ID: id, Name: name, Avatar: emoji,
			AvatarURL: avatarURL, AvatarVer: avatarVer,
			Conversations: convs,
			Commands:      commandInfos(h.agents[id]),
		})
	}
	return out
}

// commandInfos builds the app-facing command palette for an agent connection:
// every non-hidden command's name, description and category, mirroring the
// Telegram setMyCommands menu (bot_poll.go RegisterCommands). It is lock-free —
// the caller (agentRoster) already holds h.mu, and a Registry's command set is
// populated once at startup and never mutated, so All() is safe to read here.
// Returns nil for a connection with no registry (e.g. bare test agents), which
// JSON-omits the field.
func commandInfos(conn *appConn) []fap.CommandInfo {
	if conn == nil || conn.commands == nil {
		return nil
	}
	var out []fap.CommandInfo
	for _, c := range conn.commands.All() {
		if c.Hidden {
			continue
		}
		out = append(out, fap.CommandInfo{
			Name:        c.Name,
			Description: c.Description,
			Category:    c.Category,
		})
	}
	return out
}

// agentAvatarPath returns the resolved absolute path to an agent's avatar image
// (config.AgentConfig.Avatar, set at load), or "" if the agent has none.
func (h *Hub) agentAvatarPath(id string) string {
	if h.deps.Config == nil {
		return ""
	}
	for i := range h.deps.Config.Agents {
		if a := &h.deps.Config.Agents[i]; a.ID == id {
			return a.Avatar
		}
	}
	return ""
}

// agentAvatarRef returns the roster fields for an agent's avatar image: the
// fetch path ("/app/avatar/<id>") and a fingerprint (mtime+size) that changes
// when the file changes. Both are "" when the agent has no avatar or the file
// is missing.
func (h *Hub) agentAvatarRef(id string) (url, ver string) {
	p := h.agentAvatarPath(id)
	if p == "" {
		return "", ""
	}
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return "", ""
	}
	return "/app/avatar/" + id, fmt.Sprintf("%x-%x", fi.ModTime().UnixNano(), fi.Size())
}

// agentDisplay resolves an agent's human-readable name + emoji avatar from
// config, falling back to the agent ID when no config entry / name is set.
func (h *Hub) agentDisplay(id string) (name, emoji string) {
	name = id
	if h.deps.Config == nil {
		return name, ""
	}
	for i := range h.deps.Config.Agents {
		if a := &h.deps.Config.Agents[i]; a.ID == id {
			if a.Name != "" {
				name = a.Name
			}
			return name, a.Emoji
		}
	}
	return name, ""
}

// aliasFor returns the user-set conversation alias persisted in the session index's
// chat_metadata (keyed by the stable app chatID), or "" if none / unavailable.
func (h *Hub) aliasFor(b *convBinding) string {
	idx := h.deps.SessionIndex
	if idx == nil {
		return ""
	}
	v, err := idx.GetChatMetadata(b.agentID, "app", b.chatID, "alias")
	if err != nil {
		return ""
	}
	return v
}

// adoptSession re-points a conversation at an explicit session key (e.g. a
// named/independent session named in conversation.open), updating the routing
// map and persisting the mapping. The key must belong to the binding's agent.
func (h *Hub) adoptSession(b *convBinding, sessionKey string) {
	if sessionKey == "" || session.AgentIDFromKey(sessionKey) != b.agentID {
		return
	}
	b.mu.Lock()
	old := b.sessionKey
	b.sessionKey = sessionKey
	b.mu.Unlock()
	h.mu.Lock()
	if h.bySession[old] == b {
		delete(h.bySession, old)
	}
	h.bySession[sessionKey] = b
	h.mu.Unlock()
	if idx := h.deps.SessionIndex; idx != nil {
		_ = idx.SetChatMetadata(b.agentID, "app", b.chatID, "session", sessionKey)
	}
}

// --- session-key + binding bookkeeping ---

// chatIDForConv maps a (string) conversationId to a stable positive int64
// chat ID, so reconnects with the same conversationId resolve to the same
// foci session. FNV-64a; collision probability is negligible for the handful
// of conversations a device holds.
func chatIDForConv(convID string) int64 {
	hsh := fnv.New64a()
	_, _ = hsh.Write([]byte(convID))
	v := int64(hsh.Sum64() & 0x7fffffffffffffff) //nolint:gosec // masked to 63 bits, always fits int64
	if v == 0 {
		v = 1
	}
	return v
}

// sessionKeyForChat resolves (creating + persisting if new) the foci session
// key for an (agent, chatID), mirroring telegram/discord chat-meta persistence
// so a conversation's history survives reconnects and restarts.
func (h *Hub) sessionKeyForChat(agentID string, chatID int64) string {
	idx := h.deps.SessionIndex
	if idx != nil {
		if v, err := idx.GetChatMetadata(agentID, "app", chatID, "session"); err == nil && v != "" {
			return v
		}
	}
	sk := session.NewChatSessionKey(agentID, chatID)
	if idx != nil {
		_ = idx.SetChatMetadata(agentID, "app", chatID, "session", sk)
	}
	return sk
}

// ensureBinding resolves the durable conversation state for (agent, convID),
// (re)attaching it to the calling socket. The state is keyed by conversationId
// and outlives sockets, so a reconnect resumes the same seq stream + replay
// buffer rather than starting fresh.
func (h *Hub) ensureBinding(client *wsClient, agentID, convID string) *convBinding {
	h.mu.RLock()
	b := h.convs[convID]
	h.mu.RUnlock()
	if b != nil {
		b.attach(client)
		return b
	}

	// New conversation: resolve its session key (may touch SessionIndex) outside
	// the hub lock, then publish — re-checking under the lock to lose a race
	// gracefully.
	chatID := chatIDForConv(convID)
	sk := h.sessionKeyForChat(agentID, chatID)

	h.mu.Lock()
	if existing := h.convs[convID]; existing != nil {
		h.mu.Unlock()
		existing.attach(client)
		return existing
	}
	b = &convBinding{convID: convID, sessionKey: sk, agentID: agentID, chatID: chatID, replayDepth: h.replayDepth, replayTTL: h.replayTTL, store: h.frames, seq: h.frames.MaxSeq(convID), seen: make(map[string]struct{}), notifyOffline: h.pushNotify}
	h.convs[convID] = b
	h.bySession[sk] = b
	h.mu.Unlock()

	b.attach(client)
	return b
}

// resumeConversations re-attaches each durable conversation named in a client
// hello's resume points to the new socket and replays buffered frames the client
// has not yet acked (seq > ack), restoring the live stream after a reconnect.
func (h *Hub) resumeConversations(client *wsClient, points []fap.ResumePoint) {
	for _, rp := range points {
		h.mu.RLock()
		b := h.convs[rp.ConversationID]
		h.mu.RUnlock()
		if b == nil {
			continue
		}
		b.attach(client)
		b.replayTo(client, rp.Ack)
	}
}

// convForReliability returns the durable conversation state for a frame's
// conversationId, or nil if none exists yet (e.g. the very first message — the
// binding is created downstream in routeUserText).
func (h *Hub) convForReliability(convID string) *convBinding {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.convs[convID]
}

func (h *Hub) bindingForSession(sessionKey string) *convBinding {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.bySession[sessionKey]
}

// bindingsForAgent returns every live conversation binding for an agent. The
// unbound appConn (PrimaryBot) uses this to fan a session-blind notification
// out to all the agent's conversations instead of guessing a default session.
func (h *Hub) bindingsForAgent(agentID string) []*convBinding {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []*convBinding
	for _, b := range h.convs {
		if b.agentID == agentID {
			out = append(out, b)
		}
	}
	return out
}

// --- interactive prompt registry (slice 2) ---
//
// Maps a live prompt's ID to its conversation binding so foci's proactive
// edits (cancel/expiry, addressed only by the promptID returned from
// SendTextWithButtons) reach the right socket. Click-driven resolution carries
// its own conversationId, so it does not consult this map.

func (h *Hub) registerPrompt(promptID string, b *convBinding) {
	h.mu.Lock()
	h.prompts[promptID] = b
	h.mu.Unlock()
}

func (h *Hub) bindingForPrompt(promptID string) *convBinding {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.prompts[promptID]
}

func (h *Hub) deletePrompt(promptID string) {
	h.mu.Lock()
	delete(h.prompts, promptID)
	h.mu.Unlock()
}

// --- notification edit registry (compaction ⏳→✅) ---
//
// Maps a notification's messageID to its binding so a later EditMessageText
// (the in-place compaction-complete edit) can re-send the notification with the
// same messageID to the right socket. Entries survive a socket disconnect (like
// batchPrompts) so the edit still lands after a reconnect+replay; an edit that
// never arrives leaks one entry, bounded by the rare count of un-completed
// compactions. The binding outlives its socket (convs map), so no disconnect
// cleanup is needed here.

func (h *Hub) registerNotification(msgID string, b *convBinding) {
	h.mu.Lock()
	h.notifs[msgID] = b
	h.mu.Unlock()
}

func (h *Hub) bindingForNotification(msgID string) *convBinding {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.notifs[msgID]
}

func (h *Hub) deleteNotification(msgID string) {
	h.mu.Lock()
	delete(h.notifs, msgID)
	h.mu.Unlock()
}

// --- batched interactive prompt registry (multi-question asks, app only) ---
//
// A batched ask registers ONE callback keyed by the form's promptID; the app's
// single InteractiveResponse.Answers reply fires it with every answer at once.
// Distinct from the single-prompt registry above (which routes through the
// process-global platform imStore); batched callbacks live only here.
//
// Entries survive a socket disconnect on purpose: the app replays the buffered
// form on reconnect and a later submit must still route. An ask that is never
// answered leaks its entry until process exit — bounded by the (rare) count of
// unanswered batched asks, which the ask layer's 24h TTL caps in practice.

func (h *Hub) registerBatchPrompt(promptID string, b *convBinding, onResp func(answers []string)) {
	h.mu.Lock()
	h.batchPrompts[promptID] = &batchPrompt{b: b, onResp: onResp}
	h.mu.Unlock()
}

func (h *Hub) batchPromptByID(promptID string) (*batchPrompt, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	bp, ok := h.batchPrompts[promptID]
	return bp, ok
}

func (h *Hub) deleteBatchPrompt(promptID string) {
	h.mu.Lock()
	delete(h.batchPrompts, promptID)
	h.mu.Unlock()
}

func (h *Hub) addClient(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) removeClient(c *wsClient) {
	c.mu.Lock()
	bindings := make([]*convBinding, 0, len(c.convByID))
	convIDs := make(map[string]struct{}, len(c.convByID))
	for id, b := range c.convByID {
		bindings = append(bindings, b)
		convIDs[id] = struct{}{}
	}
	c.mu.Unlock()

	h.mu.Lock()
	delete(h.clients, c)
	// Durable conversation state survives a disconnect (reconnect resumes its seq
	// stream + replay buffer), so bySession is NOT cleared. Only drop live
	// interactive prompts whose conversation lived on this socket — their buttons
	// die with it.
	for pid, b := range h.prompts {
		if _, ok := convIDs[b.convID]; ok {
			delete(h.prompts, pid)
		}
	}
	h.mu.Unlock()

	// Detach this socket from its conversations; the durable state is retained.
	for _, b := range bindings {
		b.detachIf(c)
	}
}

// --- convBinding: durable per-conversation state (outlives sockets) ---

// bufferedFrame is one sent server frame retained for replay after a reconnect.
type bufferedFrame struct {
	seq  int64
	wire string
	sent time.Time
}

// convBinding is the durable state for one conversation (⇔ one session key). It
// is keyed by conversationId in the hub and survives socket reconnects: the
// wire protocol scopes seq per conversation (§3), so the outbound seq counter
// and replay buffer must persist across the sockets a phone churns through.
// `client` is the currently-attached socket, nil when the device is offline
// (sends still buffer for later replay / push).
type convBinding struct {
	convID     string
	sessionKey string
	agentID    string
	chatID     int64

	replayDepth int           // config-driven buffer depth; 0 = code default
	replayTTL   time.Duration // config-driven buffer TTL; 0 = code default
	store       *frameStore   // durable replay backstop (nil = in-memory only)

	notifyOffline func(convID, preview string) // fires a wake push for offline visible frames; nil = no push

	mu          sync.Mutex
	client      *wsClient           // current socket; nil when offline
	seq         int64               // server→app outbound seq high-water
	clientSeqHW int64               // highest client→server seq seen (stamped into outbound ack)
	buffer      []bufferedFrame     // sent frames retained for replay (trimmed by ack/depth/TTL)
	seen        map[string]struct{} // inbound dedup by envelope id
	seenOrder   []string            // FIFO eviction order for seen
}

// attach points the durable state at a (re)connected socket and registers it in
// the socket's per-conversation map so inbound frames resolve back to it.
func (b *convBinding) attach(client *wsClient) {
	b.mu.Lock()
	b.client = client
	b.mu.Unlock()
	client.mu.Lock()
	client.convByID[b.convID] = b
	client.mu.Unlock()
}

// detachIf clears the attached socket iff it is still `client` (a newer socket
// may already have taken over). The durable state itself is retained.
func (b *convBinding) detachIf(client *wsClient) {
	b.mu.Lock()
	if b.client == client {
		b.client = nil
	}
	b.mu.Unlock()
}

// currentSeq returns the outbound seq high-water (used by the interactive
// seq-advance ordering guard).
func (b *convBinding) currentSeq() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// info snapshots the conversation for the roster.
func (b *convBinding) info() fap.ConversationInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	return fap.ConversationInfo{ID: b.convID, SessionKey: b.sessionKey, LastSeq: b.seq}
}

// send encodes a server frame with the next per-conversation seq and the
// current inbound ack, retains it in the replay buffer, and enqueues it on the
// attached socket (if any). Offline (no socket) it buffers only — reconnect
// replay (and push, a later slice) heal the gap.
func (b *convBinding) send(frame fap.ServerFrame) {
	b.mu.Lock()
	b.seq++
	seq, ack := b.seq, b.clientSeqHW
	wire, err := fap.Encode(frame, seq, ack, "", "")
	if err != nil {
		b.mu.Unlock()
		log.Errorf("app", "encode %s: %v", frame.Type(), err)
		return
	}
	now := time.Now()
	b.buffer = append(b.buffer, bufferedFrame{seq: seq, wire: wire, sent: now})
	b.trimBufferLocked()
	client := b.client
	notify := b.notifyOffline
	store := b.store
	b.mu.Unlock()

	// Persist verbatim to the durable backstop (async; survives restart + the
	// in-memory depth/TTL bound, so a long-offline phone can backfill it). The
	// visible flag marks user-facing content vs transient frames (typing).
	if store != nil {
		_, visible := pushPreview(frame)
		store.Append(b.convID, seq, wire, now.UnixMilli(), visible)
	}

	if client != nil {
		client.enqueue(wire)
		return
	}
	// Offline: the frame is buffered for replay. Fire a coalesced wake push for
	// user-visible content so the device reconnects and replays it.
	if notify != nil {
		if preview, ok := pushPreview(frame); ok {
			notify(b.convID, preview)
		}
	}
}

// trimBufferLocked bounds the replay buffer by depth and TTL. Caller holds b.mu.
// depth/ttl come from the binding (config-driven); a zero value falls back to the
// code default so directly-constructed bindings (tests) still bound their buffer.
func (b *convBinding) trimBufferLocked() {
	depth := b.replayDepth
	if depth <= 0 {
		depth = defaultReplayBufferDepth
	}
	ttl := b.replayTTL
	if ttl <= 0 {
		ttl = defaultReplayTTL
	}
	if n := len(b.buffer); n > depth {
		b.buffer = append(b.buffer[:0:0], b.buffer[n-depth:]...)
	}
	cutoff := time.Now().Add(-ttl)
	drop := 0
	for drop < len(b.buffer) && b.buffer[drop].sent.Before(cutoff) {
		drop++
	}
	if drop > 0 {
		b.buffer = append(b.buffer[:0:0], b.buffer[drop:]...)
	}
}

// ackInbound trims the replay buffer to frames the client has not yet confirmed
// (envelope.ack high-water from an inbound frame).
func (b *convBinding) ackInbound(ack int64) {
	if ack <= 0 {
		return
	}
	b.mu.Lock()
	drop := 0
	for drop < len(b.buffer) && b.buffer[drop].seq <= ack {
		drop++
	}
	if drop > 0 {
		b.buffer = append(b.buffer[:0:0], b.buffer[drop:]...)
	}
	b.mu.Unlock()
}

// acceptInbound dedups an inbound frame by envelope id and advances the client
// seq high-water. Returns false if the frame is a duplicate (a resent outbox
// entry after reconnect) and must be dropped.
func (b *convBinding) acceptInbound(id string, seq int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen == nil {
		b.seen = make(map[string]struct{})
	}
	if id != "" {
		if _, dup := b.seen[id]; dup {
			return false
		}
		b.seen[id] = struct{}{}
		b.seenOrder = append(b.seenOrder, id)
		if len(b.seenOrder) > maxSeenInbound {
			old := b.seenOrder[0]
			b.seenOrder = b.seenOrder[1:]
			delete(b.seen, old)
		}
	}
	if seq > b.clientSeqHW {
		b.clientSeqHW = seq
	}
	return true
}

// replayTo re-sends buffered frames with seq > fromSeq to the socket, in seq
// order — the reconnect resume path.
func (b *convBinding) replayTo(client *wsClient, fromSeq int64) {
	b.mu.Lock()
	hasMem := len(b.buffer) > 0
	memFloor := int64(0) // lowest seq the in-memory buffer still holds
	if hasMem {
		memFloor = b.buffer[0].seq
	}
	pending := make([]string, 0, len(b.buffer))
	for _, bf := range b.buffer {
		if bf.seq > fromSeq {
			pending = append(pending, bf.wire)
		}
	}
	store := b.store
	convID := b.convID
	b.mu.Unlock()

	// Backfill the gap the in-memory buffer can't cover — frames trimmed by
	// depth/TTL, or lost when this process restarted — from the durable store,
	// before the in-memory frames. memFloor is where memory takes over: store
	// frames at seq >= memFloor would duplicate `pending`, so stop there. No
	// in-memory frames (hasMem == false) → the store supplies everything > fromSeq.
	// A gap larger than maxReplayPage is finished by the client via GET /app/replay.
	if store != nil && (!hasMem || fromSeq < memFloor-1) {
		for _, sf := range store.Range(convID, fromSeq, maxReplayPage) {
			if hasMem && sf.seq >= memFloor {
				break
			}
			client.enqueue(sf.wire)
		}
	}
	for _, wire := range pending {
		client.enqueue(wire)
	}
}

// --- wsClient: one physical socket ---

type wsClient struct {
	ws      *websocket.Conn
	hub     *Hub
	send    chan []byte
	done    chan struct{}
	closeMu sync.Once

	mu sync.Mutex
	// No socket-wide "current agent": one socket multiplexes every agent's
	// conversations concurrently. Each inbound frame names its agent (or its
	// conversation's binding does), so the agent is resolved per-frame.
	deviceID string                  // from the client hello
	features map[string]struct{}     // advertised client capabilities (from the hello)
	convByID map[string]*convBinding // conversationId → binding
}

// hasFeature reports whether this socket's client advertised feat in its hello.
func (c *wsClient) hasFeature(feat string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.features[feat]
	return ok
}

// clientHasFeature reports whether the binding's currently-attached socket
// advertised feat. False when offline (no socket) — callers treat that as "can't".
func (b *convBinding) clientHasFeature(feat string) bool {
	b.mu.Lock()
	c := b.client
	b.mu.Unlock()
	return c != nil && c.hasFeature(feat)
}

// featureSet builds a lookup set from the hello's advertised feature list.
func featureSet(feats []string) map[string]struct{} {
	if len(feats) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(feats))
	for _, f := range feats {
		m[f] = struct{}{}
	}
	return m
}

func newWsClient(ws *websocket.Conn, h *Hub) *wsClient {
	return &wsClient{
		ws:       ws,
		hub:      h,
		send:     make(chan []byte, sendBuffer),
		done:     make(chan struct{}),
		convByID: make(map[string]*convBinding),
	}
}

func (c *wsClient) enqueue(wire string) {
	// Fast path: space available (or already closed).
	select {
	case c.send <- []byte(wire):
		return
	case <-c.done:
		return
	default:
	}
	// Queue full. Dropping here would punch an unrecoverable hole *below* the
	// client's resume high-water: the replay buffer holds the frame, but the
	// client acks past it and never asks for it again (see
	// docs/app-message-fragmentation.md). So never drop a live frame — block
	// briefly for space, and if the socket still won't drain it's stalled/dead:
	// close it so the client reconnects and replays from its true contiguous
	// mark, healing the gap.
	t := time.NewTimer(enqueueBlockWait)
	defer t.Stop()
	select {
	case c.send <- []byte(wire):
	case <-c.done:
	case <-t.C:
		log.Warnf("app", "outbound queue stalled %s, closing slow client to force resume", enqueueBlockWait)
		go c.close() // async: close locks the hub; don't reenter from the send path
	}
}

func (c *wsClient) close() {
	c.closeMu.Do(func() {
		close(c.done)
		if c.ws != nil { // nil only for in-memory test clients
			_ = c.ws.Close()
		}
		if c.hub != nil {
			c.hub.removeClient(c)
		}
	})
}

// closeWithCode sends a WebSocket close frame with the given code/reason (so the
// client knows e.g. it was revoked vs a transient drop) then tears down.
func (c *wsClient) closeWithCode(code int, reason string) {
	if c.ws != nil {
		_ = c.ws.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(code, reason),
			time.Now().Add(writeWait),
		)
	}
	c.close()
}

func (c *wsClient) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case msg, ok := <-c.send:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.ws.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			if err := c.ws.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *wsClient) readPump() {
	defer c.close()
	c.ws.SetReadLimit(maxFrameSize)
	_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(string) error {
		_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	for {
		mt, data, err := c.ws.ReadMessage()
		if err != nil {
			// 1006/EOF is how a mobile client normally vanishes — cell<->wifi
			// handoff, backgrounding, doze, tunnel drop: the socket dies with no
			// close frame, so there's never a clean 1000/1001. Treat it (and the
			// clean closes) as expected; only genuinely anomalous close codes
			// (protocol error, oversize frame, ...) warrant a WARN.
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway,
				websocket.CloseNormalClosure,
				websocket.CloseAbnormalClosure) {
				log.Warnf("app", "read error: %v", err)
			} else {
				log.Debugf("app", "client disconnected: %v", err)
			}
			return
		}
		if mt != websocket.TextMessage {
			continue
		}
		c.hub.dispatchInbound(c, data)
	}
}

// --- HTTP endpoint ---

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     checkOrigin,
	// Echo the fap.v1 subprotocol the client offers (wire §1). gorilla only
	// echoes a Sec-WebSocket-Protocol it is told to support; a strict proxy
	// (Traefik) or future client may assert the negotiated protocol.
	Subprotocols: []string{fap.Subprotocol},
}

// checkOrigin accepts the native client (no Origin header) and same-host
// upgrades; the Bearer key is the real authentication gate.
func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	return strings.Contains(origin, r.Host)
}

// ServeWS authenticates (Bearer device token) and upgrades the
// connection, then runs the socket's read/write pumps.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	dev, ok := h.authenticate(w, r)
	if !ok {
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Errorf("app", "ws upgrade: %v", err)
		return
	}
	client := newWsClient(ws, h)
	if dev != nil {
		// Authenticated with a device token: the deviceId is authoritative (the
		// client hello's is advisory and should match).
		client.mu.Lock()
		client.deviceID = dev.DeviceID
		client.mu.Unlock()
	}
	h.addClient(client)
	if dev != nil {
		// Device-token auth gives the deviceId up front, so we can evict a stale
		// prior socket immediately. Master-key sockets learn their deviceId from
		// the hello frame and are evicted in the ClientHello handler instead.
		h.evictOtherDeviceSockets(client, dev.DeviceID)
	}
	log.Infof("app", "device connected")
	safeGo("ws-writepump", client.writePump)
	client.readPump() // blocks until the socket closes
	log.Infof("app", "device disconnected")
}

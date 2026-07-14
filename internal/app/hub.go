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
	maxReplayPage            = 10000 // max frames per GET /app/replay page (HTTP backfill; the app applies a page as ONE Room transaction, so big pages are cheap)
	maxResumeStoreReplay     = 2000  // max durable-store frames pushed over the ws on reconnect resume — the app dispatches ws frames one-by-one, so keep this burst modest
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
	deliverMu    sync.Mutex              // serialises deliverBinding's find-or-create + default pin
	prompts      map[string]*convBinding // promptID → binding (live interactive prompts)
	batchPrompts map[string]*batchPrompt // promptID → batched-ask callback (app-only multi-question form)
	notifs       map[string]*convBinding // notification messageID → binding (for in-place edit, e.g. compaction ⏳→✅)
	toolCalls    *toolCallRegistry       // InvocationID → waiting InvokeTool caller

	wizardMu      sync.Mutex
	wizards       map[string]*wizardSession // wizardId → live out-of-band wizard session
	wizardByScope map[string]string         // wizard scope (session key) → wizardId
}

// featureInteractiveBatch is the ClientHello capability a client advertises to
// receive multi-question asks as one batched form (vs sequential prompts).
const featureInteractiveBatch = "interactiveBatch"

// featureWizard is the ClientHello capability a client advertises to receive
// command wizards as structured wizard.step/wizard.end frames (out-of-band
// rendering) instead of plain system messages.
const featureWizard = "wizard"

// featureSettingsSync is the ClientHello capability a client advertises to
// receive the synced app-preferences bag (settings.snapshot) and mirror its own
// changes back via setting.put.
const featureSettingsSync = "settingsSync"

// systemStateAppSettings is the system_state key under which the whole synced
// app-preferences bag is stored as a JSON object.
const systemStateAppSettings = "app_settings"

// systemStateOpenChats is the system_state key under which the user's shared
// open-set (the conversations open across their devices) is stored as a JSON
// array of conversation ids, so a device offline during a change catches up on
// reconnect.
const systemStateOpenChats = "open_chats"

// batchPrompt holds the in-flight callback for one batched (multi-question) ask.
// The app returns all answers in a single InteractiveResponse.Answers; onResp
// feeds them back into the ask layer (tools.AskPresentBatchFn's onResponse).
type batchPrompt struct {
	b      *convBinding
	onResp func(answers []string)

	mu      sync.Mutex
	answers []string // accumulated per-question answers (streamed via InteractiveProgress)
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
		frames:        frames,
		deps:          deps,
		pairKeys:      newPairKeyStore(),
		blobs:         blobs,
		tokens:        tokens,
		devices:       newDeviceStore(devicePath),
		authLim:       newAuthLimiter(authFailMax, authFailWindow),
		agents:        make(map[string]*appConn),
		convs:         make(map[string]*convBinding),
		bySession:     make(map[string]*convBinding),
		clients:       make(map[*wsClient]struct{}),
		prompts:       make(map[string]*convBinding),
		batchPrompts:  make(map[string]*batchPrompt),
		notifs:        make(map[string]*convBinding),
		toolCalls:     newToolCallRegistry(),
		wizards:       make(map[string]*wizardSession),
		wizardByScope: make(map[string]string),
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
		if pushEnabled && fcmPath == "" {
			log.Warnf("app", "app push enabled but no FCM credentials found (neither [platforms.app].fcm_credentials nor app.fcm_credentials secret) — offline wake pushes disabled")
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
// push is unconfigured). Used as convBinding.notifyOffline. Resolves the agent
// display name and session title from the payload's identity fields so the
// client notification shows who the message is from.
func (h *Hub) pushNotify(p pushPayload) {
	if h.pusher == nil {
		return
	}
	p.AgentName, _ = h.agentDisplay(p.AgentID)
	p.SessionTitle = h.aliasForChat(p.AgentID, p.ChatID)
	h.pusher.notify(p, h.connectedDeviceIDs())
}

// connectedDeviceIDs returns the set of device IDs that currently hold at least
// one live socket. Such devices receive frames over that socket, so they are
// excluded from wake pushes. Snapshots the client set under h.mu, then reads
// each deviceID under its own lock (never both at once — attach takes b.mu
// before client.mu, so nesting the other way here could deadlock).
func (h *Hub) connectedDeviceIDs() map[string]bool {
	h.mu.RLock()
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()
	ids := make(map[string]bool, len(clients))
	for _, c := range clients {
		c.mu.Lock()
		id := c.deviceID
		c.mu.Unlock()
		if id != "" {
			ids[id] = true
		}
	}
	return ids
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
	if h.deps.SessionIndex != nil {
		c.Features = append(c.Features, featureSettingsSync)
	}
	if h.configEditAvailable() {
		c.Features = append(c.Features, featureConfigEdit)
	}
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
	log.Debugf("app", "replay GET: conv=%s fromSeq=%d returned=%d more=%v", convID, fromSeq, len(frames), len(frames) == limit)
	// Same closed-ask substitution as the reconnect replayTo path (see there).
	orphaned := h.frames.OrphanedResolvedAsks(convID)
	out := make([]map[string]any, 0, len(frames))
	for _, f := range frames {
		out = append(out, map[string]any{"seq": f.seq, "wire": substituteResolvedAsk(f.wire, orphaned)})
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

	// Re-link persisted out-of-band wizard sessions to the wizards the command
	// Registry restored at startup, so a server restart doesn't strand an app
	// mid-wizard (wizard.go).
	h.restoreWizardSessions(conn, params.AgentID)

	// App is interactive (like telegram): steer dispatch follows the
	// agent's behavior config (steer_mode), mirroring telegram/discord.
	ag.SetInboxSteerMode(params.Resolved.Behavior.SteerMode) // static-cfg:ignore: fallback, LiveConfigFn takes over via the steerMode() getter (bucket D)
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

// StartAll rebuilds bindings for every conversation at startup so an
// unsolicited message (cron/keepalive/reflection) lands correctly BEFORE the app
// reconnects — closing the post-restart window where bindingForSession would
// otherwise return nil and the send would drop. The restore set is the union of
// two durable sources: every conv with a visible frame and a known agent in the
// frame store, plus every persisted conv_id row (a conversation registered at
// binding creation but without a durable frame yet — created and maybe starred,
// never used — must survive restarts too). Archived convs are included (archive
// is a reversible flag, not a frame purge); their archived state is surfaced via
// the roster. ensureBinding with a nil client creates the socketless durable
// binding; the real socket re-attaches on the app's next hello/resume
// (#app-binding-restore).
func (h *Hub) StartAll(context.Context) {
	if h.frames == nil {
		return
	}
	convs := h.frames.RestorableConvs()
	seen := make(map[string]struct{}, len(convs))
	for _, c := range convs {
		h.ensureBinding(nil, c.agentID, c.convID)
		seen[c.convID] = struct{}{}
	}
	restored := len(convs)
	if idx := h.deps.SessionIndex; idx != nil {
		refs, err := idx.ConvRefs("app")
		if err != nil {
			log.Errorf("app", "startup restore: conv_id rows: %v", err)
		}
		for _, r := range refs {
			if _, ok := seen[r.ConvID]; ok {
				continue
			}
			h.ensureBinding(nil, r.AgentID, r.ConvID)
			restored++
		}
	}
	if restored > 0 {
		log.Infof("app", "restored %d binding(s) from durable store at startup", restored)
	}
	// One-time: resolve asks stored before durable resolution tracking existed,
	// so a freshly-paired device doesn't replay them as fresh (#981).
	if h.frames.NeedsLegacyAskSweep() {
		legacy := h.frames.LegacyOpenAsks()
		for _, a := range legacy {
			h.ensureBinding(nil, a.agentID, a.convID).
				send(fap.InteractiveEdit{ConversationID: a.convID, PromptID: a.promptID, Text: a.text})
		}
		h.frames.MarkLegacyAsksSwept()
		if len(legacy) > 0 {
			log.Infof("app", "swept %d legacy open ask(s) into resolved on startup", len(legacy))
		}
	}
}
func (h *Hub) Wait() {}

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

	// Per-agent archived-chat set (app platform), memoised the same way as
	// defaultChat so each agent's chat_metadata is queried at most once per
	// roster build. Marks matching conversations' Archived flag — the roster is
	// the app's source of truth for archived state (see ConversationInfo.Archived).
	archivedByAgent := make(map[string]map[int64]bool)
	archivedChats := func(agentID string) map[int64]bool {
		if idx == nil {
			return nil
		}
		if v, ok := archivedByAgent[agentID]; ok {
			return v
		}
		v := idx.ArchivedChatsForAgent(agentID, "app")
		archivedByAgent[agentID] = v
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
		if archivedChats(b.agentID)[b.chatID] {
			ci.Archived = true
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
	return h.aliasForChat(b.agentID, b.chatID)
}

// aliasForChat resolves the conversation alias by agent + chatID (the identity
// fields available in the push path), without requiring a *convBinding.
func (h *Hub) aliasForChat(agentID string, chatID int64) string {
	idx := h.deps.SessionIndex
	if idx == nil {
		return ""
	}
	v, err := idx.GetChatMetadata(agentID, "app", chatID, "alias")
	if err != nil {
		return ""
	}
	return v
}

// adoptSession re-points a conversation at an explicit session key (e.g. a
// named/independent session named in conversation.open), updating the routing
// map and persisting the mapping. The key must belong to the binding's agent.
func (h *Hub) adoptSession(b *convBinding, sessionKey string) {
	if sessionKey == "" {
		return
	}
	// Clients may replay keys stored before the stable-identity migration
	// (with a version segment); normalise rather than adopting a key the
	// store can no longer resolve. Anything else that doesn't parse is
	// refused — a bad adoption would wedge the conversation.
	if stable, isLegacy := session.LegacyKeyToStable(sessionKey); isLegacy {
		log.Infof("app", "conversation %s: normalised legacy session key %q → %q", b.convID, sessionKey, stable)
		sessionKey = stable
	}
	if sk, err := session.ParseSessionKey(sessionKey); err != nil || sk.AgentID != b.agentID {
		log.Warnf("app", "conversation %s: refusing to adopt session key %q (agent %s): invalid or cross-agent", b.convID, sessionKey, b.agentID)
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
		_ = idx.SetChatMetadata(b.agentID, "app", b.chatID, "session_key", sessionKey)
	}
}

// mintFacetConversation surfaces a facet branch as a new app conversation: it
// mints a conversation bound to sessionKey, adds it to the shared open-set so the
// tab opens on every device, and pushes the updated roster. Returns the new
// conversation ID. Focus stays device-local — the requesting client foregrounds
// it via the command response's OpenConversationID (a ConversationForeground
// frame sent only to that socket).
func (h *Hub) mintFacetConversation(agentID, sessionKey string) (string, error) {
	b := h.ensureBinding(nil, agentID, fap.NewULID())
	h.adoptSession(b, sessionKey)
	b.mu.Lock()
	adopted := b.sessionKey
	b.mu.Unlock()
	if adopted != sessionKey {
		return "", fmt.Errorf("facet conversation %s: session key %q refused", b.convID, sessionKey)
	}

	ids := h.loadOpenChats()
	present := false
	for _, id := range ids {
		if id == b.convID {
			present = true
			break
		}
	}
	if !present {
		ids = append(ids, b.convID)
		h.storeOpenChats(ids)
		h.broadcastOpenSetExcept(ids, nil)
	}
	h.pushRosterAll()
	return b.convID, nil
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

// sessionKeyForChat resolves the foci session key for an (agent, chatID).
// Chat session keys are deterministic (agent/c<chatID>); a persisted
// "session_key" row overrides that when the conversation adopted an explicit
// session — e.g. a named/independent session via conversation.open (see
// adoptSession). The platform-ownership row is registered either way so
// outbound routing (SessionIndex.PlatformForChat) can find the app.
func (h *Hub) sessionKeyForChat(agentID string, chatID int64) string {
	idx := h.deps.SessionIndex
	if idx != nil {
		if v, err := idx.GetChatMetadata(agentID, "app", chatID, "session_key"); err == nil && v != "" {
			return v
		}
		// registered='true' is the universal "real user-facing chat" signal
		// (telegram/discord write it via chatmeta.Resolver.RegisterChat). It's
		// what default-session routing filters on, so an app conversation is
		// treated as a routable chat rather than incidental metadata.
		_ = idx.SetChatMetadata(agentID, "app", chatID, "registered", "true")
	}
	return session.NewChatSessionKey(agentID, chatID)
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
	if idx := h.deps.SessionIndex; idx != nil {
		// Persist the hash preimage: this row is what makes the conversation
		// durable before its first frame (StartAll restores from it) and the
		// default-chat pin resolvable when the binding isn't live
		// (defaultChatBinding reverses the one-way chatID hash through it).
		_ = idx.SetChatMetadata(agentID, "app", chatID, "conv_id", convID)
	}

	h.mu.Lock()
	if existing := h.convs[convID]; existing != nil {
		h.mu.Unlock()
		existing.attach(client)
		return existing
	}
	b = &convBinding{convID: convID, sessionKey: sk, agentID: agentID, chatID: chatID, replayDepth: h.replayDepth, replayTTL: h.replayTTL, store: h.frames, seq: h.frames.MaxSeq(convID), seen: make(map[string]struct{}), notifyOffline: h.pushNotify}
	if preview, sentMs, ok := h.frames.LastVisible(convID); ok {
		b.lastPreview = preview
		b.lastActMs = sentMs
	}
	// Rehydrate the app's last-advertised caps so a binding rebuilt after a
	// restart (before the app reconnects) still resolves capability checks.
	if idx := h.deps.SessionIndex; idx != nil {
		if v, err := idx.GetChatMetadata(agentID, "app", chatID, "features"); err == nil && v != "" {
			b.features = featureSet(strings.Split(v, ","))
		}
	}
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
			log.Debugf("app", "resume: conv=%s ack=%d — no binding, skipped", rp.ConversationID, rp.Ack)
			continue
		}
		b.attach(client)
		// Seed this client's acked high-water from its resume point so a pure
		// reader (which never sends a frame, hence never hits ackInbound) doesn't
		// pin the trim floor at 0 forever (#4).
		b.seedClientAck(client, rp.Ack)
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

// defaultChatBinding returns the binding for the agent's default app chat, or
// nil when no default is set. A live binding is matched by chatID; when the
// pinned conversation isn't bound (restart edge cases, pin set from another
// device), its persisted conv_id row — the preimage of the one-way chatID hash,
// written at binding creation — resurrects it. nil is also returned for a pin
// with no conv_id row (set before conv_id persistence existed), in which case
// the caller falls back. Used as the never-bound send fallback (#959).
func (h *Hub) defaultChatBinding(agentID string) *convBinding {
	idx := h.deps.SessionIndex
	if idx == nil {
		return nil
	}
	dc := idx.DefaultChatForAgent(agentID, "app")
	if dc == 0 {
		return nil
	}
	h.mu.RLock()
	for _, b := range h.convs {
		if b.agentID == agentID && b.chatID == dc {
			h.mu.RUnlock()
			return b
		}
	}
	h.mu.RUnlock()
	convID, err := idx.GetChatMetadata(agentID, "app", dc, "conv_id")
	if err != nil || convID == "" {
		log.Warnf("app", "default chat %d (agent %s): no live binding and no persisted conv_id — pin unresolvable, falling back", dc, agentID)
		return nil
	}
	log.Infof("app", "default chat %d (agent %s): resurrecting conversation %s", dc, agentID, convID)
	return h.ensureBinding(nil, agentID, convID)
}

// deliverBinding resolves — or creates — the destination conversation for a
// session-blind send to this agent. The app is a session-creating surface, so
// "nowhere to deliver" is never the right answer:
//
//  1. The pinned default conversation, when set (resurrected from its
//     persisted conv_id if not live).
//  2. No default: the most recently active conversation at the time of the
//     send. The default pin is user-owned (conversation.setDefault) — it is
//     never set automatically.
//  3. No conversations at all: a server-minted conversation. Live sockets
//     learn it from an immediate roster push; offline devices on their next
//     hello. (NB: the Android client must upsert roster conversations it has
//     never seen locally.) Subsequent sends find it as the newest
//     conversation via rung 2.
//
// The second return names the rung that fired ("default", "most-recent",
// "server-created") so callers can log the true destination. Serialised by
// deliverMu so concurrent session-blind sends can't mint duplicate
// conversations.
func (h *Hub) deliverBinding(agentID string) (*convBinding, string) {
	h.deliverMu.Lock()
	defer h.deliverMu.Unlock()

	if b := h.defaultChatBinding(agentID); b != nil {
		return b, "default"
	}

	// Most recently active existing conversation.
	var latest *convBinding
	h.mu.RLock()
	for _, b := range h.convs {
		if b.agentID != agentID {
			continue
		}
		if latest == nil || b.lastActivity() > latest.lastActivity() {
			latest = b
		}
	}
	h.mu.RUnlock()
	if latest != nil {
		return latest, "most-recent"
	}

	convID := fap.NewULID()
	created := h.ensureBinding(nil, agentID, convID)
	log.Infof("app", "server-created conversation %s for agent %s (no existing conversations)", convID, agentID)
	h.pushRosterAll()
	return created, "server-created"
}

// pushRoster re-advertises the roster to one socket — the ack half of every
// roster-changing round-trip (hello, conversation.list/open/rename/setDefault/
// archive), through which the client reconciles server-authoritative state.
func (h *Hub) pushRoster(client *wsClient) {
	client.sendRaw(fap.HelloServer{Version: fap.ProtocolVersion, Caps: h.caps(), Agents: h.agentRoster()})
}

// pushRosterAll re-advertises the roster to every live socket, so connected
// devices learn server-originated conversation changes without reconnecting.
func (h *Hub) pushRosterAll() {
	hello := fap.HelloServer{Version: fap.ProtocolVersion, Caps: h.caps(), Agents: h.agentRoster()}
	h.mu.RLock()
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()
	for _, c := range clients {
		c.sendRaw(hello)
	}
}

func (h *Hub) loadAppSettings() map[string]string {
	idx := h.deps.SessionIndex
	if idx == nil {
		return map[string]string{}
	}
	raw, err := idx.GetSystemState(systemStateAppSettings)
	if err != nil || raw == "" {
		return map[string]string{}
	}
	m := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return map[string]string{}
	}
	return m
}

// storeAppSetting applies one key=value to the persisted bag and returns the
// merged map for fan-out. Last-write-wins.
func (h *Hub) storeAppSetting(key, value string) map[string]string {
	m := h.loadAppSettings()
	m[key] = value
	if idx := h.deps.SessionIndex; idx != nil {
		if b, err := json.Marshal(m); err == nil {
			_ = idx.SetSystemState(systemStateAppSettings, string(b))
		}
	}
	return m
}

// loadOpenChats reads the persisted shared open-set. Empty (nil) when unset or
// unreadable.
func (h *Hub) loadOpenChats() []string {
	idx := h.deps.SessionIndex
	if idx == nil {
		return nil
	}
	raw, err := idx.GetSystemState(systemStateOpenChats)
	if err != nil || raw == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	return ids
}

// storeOpenChats persists the shared open-set as a JSON array. Last-write-wins.
func (h *Hub) storeOpenChats(ids []string) {
	idx := h.deps.SessionIndex
	if idx == nil {
		return
	}
	if b, err := json.Marshal(ids); err == nil {
		_ = idx.SetSystemState(systemStateOpenChats, string(b))
	}
}

func (h *Hub) pushSettings(client *wsClient) {
	client.mu.Lock()
	_, ok := client.features[featureSettingsSync]
	client.mu.Unlock()
	if !ok {
		return
	}
	client.sendRaw(fap.SettingsSnapshot{Settings: h.loadAppSettings()})
}

// broadcastSettings fans the bag out to every settings-capable client, so a
// change on one device reconciles on the others without a reconnect.
func (h *Hub) broadcastSettings(settings map[string]string) {
	snap := fap.SettingsSnapshot{Settings: settings}
	h.mu.RLock()
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()
	for _, c := range clients {
		c.mu.Lock()
		_, ok := c.features[featureSettingsSync]
		c.mu.Unlock()
		if ok {
			c.sendRaw(snap)
		}
	}
}

// broadcastReadExcept fans a read watermark out to every client EXCEPT the one
// that sent it (which already advanced locally). Safe to send to a client that
// lacks the conversation — its read advance is monotonic and no-ops.
func (h *Hub) broadcastReadExcept(convID, messageID string, sender *wsClient) {
	frame := fap.ReadSync{ConversationID: convID, MessageID: messageID}
	h.mu.RLock()
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		if c != sender {
			clients = append(clients, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range clients {
		c.sendRaw(frame)
	}
}

// broadcastDraftExcept fans a conversation's draft out to every client EXCEPT
// the one that put it (whose composer already holds it). A client that lacks
// the conversation just stores the draft for its next open; a client actively
// typing in it suppresses the apply, so this never clobbers in-progress input.
func (h *Hub) broadcastDraftExcept(convID, text string, sender *wsClient) {
	frame := fap.DraftSync{ConversationID: convID, Text: text}
	h.mu.RLock()
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		if c != sender {
			clients = append(clients, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range clients {
		c.sendRaw(frame)
	}
}

// broadcastOpenSetExcept fans the user's open-set out to every client EXCEPT the
// one that sent it (whose pager already holds it). The receiving client
// reconciles its open tabs to match; the sender is skipped so its own change
// doesn't echo back.
func (h *Hub) broadcastOpenSetExcept(ids []string, sender *wsClient) {
	frame := fap.ConversationOpenSync{ConversationIDs: ids}
	h.mu.RLock()
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		if c != sender {
			clients = append(clients, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range clients {
		c.sendRaw(frame)
	}
}

// pushOpenSet replays the persisted shared open-set to a just-connected client,
// so a device offline during a change adopts it. Skipped when nothing is stored.
func (h *Hub) pushOpenSet(client *wsClient) {
	if ids := h.loadOpenChats(); len(ids) > 0 {
		client.sendRaw(fap.ConversationOpenSync{ConversationIDs: ids})
	}
}

// pushChatScalar replays a per-chat scalar metadata value (keyed by metaKey) of
// every live conversation to a just-connected client, so a device offline during
// the change catches up. frame builds the server frame from the conversationId and
// stored value. When replayEmpty is false an empty stored value is skipped (right
// for read watermarks — an empty watermark carries nothing); drafts set it true so
// a cleared draft still reaches devices that were offline during the clear.
func (h *Hub) pushChatScalar(client *wsClient, metaKey string, replayEmpty bool, frame func(convID, value string) fap.ServerFrame) {
	idx := h.deps.SessionIndex
	if idx == nil {
		return
	}
	h.mu.RLock()
	bindings := make([]*convBinding, 0, len(h.convs))
	for _, b := range h.convs {
		bindings = append(bindings, b)
	}
	h.mu.RUnlock()
	for _, b := range bindings {
		if v, err := idx.GetChatMetadata(b.agentID, "app", b.chatID, metaKey); err == nil && (replayEmpty || v != "") {
			client.sendRaw(frame(b.convID, v))
		}
	}
}

// pushDrafts replays the stored draft of every live conversation to a
// just-connected client, so a device offline during an edit catches up.
func (h *Hub) pushDrafts(client *wsClient) {
	h.pushChatScalar(client, "draft", true, func(convID, v string) fap.ServerFrame {
		return fap.DraftSync{ConversationID: convID, Text: v}
	})
}

// pushReads replays the stored read watermark of every live conversation to a
// just-connected client, so a device offline during a read catches up.
func (h *Hub) pushReads(client *wsClient) {
	h.pushChatScalar(client, "last_read", false, func(convID, v string) fap.ServerFrame {
		return fap.ReadSync{ConversationID: convID, MessageID: v}
	})
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
	h.frames.PutPrompt(promptID, b.convID, b.agentID, time.Now().UnixMilli())
}

func (h *Hub) bindingForPrompt(promptID string) *convBinding {
	h.mu.RLock()
	b := h.prompts[promptID]
	h.mu.RUnlock()
	if b != nil {
		return b
	}
	// Durable fallback: the prompt was registered before a restart wiped the
	// in-memory registry. Rebuild the binding so the resolution still emits a
	// resolve frame that persists and replays to the app (else a resolved ask
	// re-appears as fresh after a fresh pair).
	if convID, agentID, ok := h.frames.PromptConv(promptID); ok {
		return h.ensureBinding(nil, agentID, convID)
	}
	return nil
}

func (h *Hub) deletePrompt(promptID string) {
	h.mu.Lock()
	delete(h.prompts, promptID)
	h.mu.Unlock()
	h.frames.DeletePrompt(promptID)
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

func (h *Hub) registerBatchPrompt(promptID string, b *convBinding, questionCount int, onResp func(answers []string)) {
	h.mu.Lock()
	h.batchPrompts[promptID] = &batchPrompt{b: b, onResp: onResp, answers: make([]string, questionCount)}
	h.mu.Unlock()
	h.frames.PutPrompt(promptID, b.convID, b.agentID, time.Now().UnixMilli())
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
	h.frames.DeletePrompt(promptID)
}

func (h *Hub) addClient(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) removeClient(c *wsClient) {
	c.mu.Lock()
	bindings := make([]*convBinding, 0, len(c.convByID))
	for _, b := range c.convByID {
		bindings = append(bindings, b)
	}
	c.mu.Unlock()

	// Detach this socket from its conversations FIRST, so hasLiveClients below
	// reflects it leaving. The durable state (buffer/seen/seq) is retained.
	for _, b := range bindings {
		b.detachIf(c)
	}
	// A prompt's buttons only die if NO device is left on its conversation. With
	// multiple devices attached, purging on any one disconnect would strand the
	// survivors' Allow/Deny taps on an unknown prompt id (#1). Compute the now-
	// dead conversations (b.mu, no h.mu held — the two are never nested).
	dead := make(map[string]struct{}, len(bindings))
	for _, b := range bindings {
		if !b.hasLiveClients() {
			dead[b.convID] = struct{}{}
		}
	}

	h.mu.Lock()
	delete(h.clients, c)
	// Durable conversation state survives a disconnect (reconnect resumes its seq
	// stream + replay buffer), so bySession is NOT cleared. Drop live interactive
	// prompts only for conversations with no remaining device.
	for pid, b := range h.prompts {
		if _, ok := dead[b.convID]; ok {
			delete(h.prompts, pid)
		}
	}
	h.mu.Unlock()
}

// OpenSessionsForAgent returns the deduped session keys of the conversations the
// agent's app clients currently have open (their pager tabs). Used by keepalive
// to warm open chats. Snapshots bindings under each client's lock, then reads
// session keys under the binding lock — never both at once (attach takes b.mu
// before client.mu, so the reverse order here would deadlock).
func (h *Hub) OpenSessionsForAgent(agentID string) []string {
	h.mu.RLock()
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	var bindings []*convBinding
	for _, c := range clients {
		c.mu.Lock()
		for convID := range c.openConvIDs {
			if b := c.convByID[convID]; b != nil {
				bindings = append(bindings, b)
			}
		}
		c.mu.Unlock()
	}

	seen := make(map[string]struct{})
	var out []string
	for _, b := range bindings {
		b.mu.Lock()
		sk, ag := b.sessionKey, b.agentID
		b.mu.Unlock()
		if ag != agentID || sk == "" {
			continue
		}
		if _, dup := seen[sk]; dup {
			continue
		}
		seen[sk] = struct{}{}
		out = append(out, sk)
	}
	return out
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
// clientState is one attached socket's reliability high-water pair. Devices
// number their client→server streams independently, so seqHW (what we've
// received FROM this client, stamped as the ack on frames sent TO it) and ackHW
// (what this client has acked of OUR stream, which floors replay-buffer trimming)
// must be per-client, never shared across devices.
type clientState struct {
	ackHW int64 // highest outbound seq this client has acknowledged
	seqHW int64 // highest inbound seq seen from this client
}

type convBinding struct {
	convID     string
	sessionKey string
	agentID    string
	chatID     int64

	replayDepth int           // config-driven buffer depth; 0 = code default
	replayTTL   time.Duration // config-driven buffer TTL; 0 = code default
	store       *frameStore   // durable replay backstop (nil = in-memory only)

	notifyOffline func(p pushPayload) // fires a wake push for offline visible frames; nil = no push

	mu           sync.Mutex
	clients      map[*wsClient]struct{}     // live sockets currently bound (≥1 when online); multi-device: a phone and tablet can both be attached
	clientStates map[*wsClient]*clientState // per-client reliability state (ack/seq high-water); NOT shared — each device numbers its stream independently
	features     map[string]struct{}        // UNION of advertised capabilities across attached clients; cached so checks survive disconnect
	seq          int64                      // server→app outbound seq high-water
	buffer       []bufferedFrame            // sent frames retained for replay (trimmed by ack/depth/TTL)
	seen         map[string]struct{}        // inbound dedup by envelope id
	seenOrder    []string                   // FIFO eviction order for seen

	// Unified activity inputs (see resolveActivity for precedence). turnKind /
	// turnDetail are turn-scoped, driven by appSink off the turn-event stream;
	// subagentDetail and waitingDetail are session-scoped, outliving any single
	// turn. activityKind / activityDetail cache the last-emitted resolved value
	// so a setter only sends an Activity frame (and updates the roster snapshot)
	// on an actual change.
	turnKind       fap.ActivityKind // turn-scoped kind (idle when no turn in flight)
	turnDetail     string           // turn-scoped detail (e.g. tool name)
	subagentDetail string           // running-subagent descriptions, empty if none
	waitingDetail  string           // target agent id we're awaiting, empty if none
	activityKind   fap.ActivityKind // last-emitted resolved kind ("" == idle)
	activityDetail string           // last-emitted resolved detail

	lastPreview string // last visible frame's preview; seeds the roster row
	lastActMs   int64  // last visible frame's send time (unix ms); seeds the roster row

	cacheExpiryMs int64 // last-emitted prompt-cache expiry (unix ms); seeds the roster snapshot, 0 = unknown/cold
}

// attach points the durable state at a (re)connected socket and registers it in
// the socket's per-conversation map so inbound frames resolve back to it.
//
// Multi-device: a conversation may have MANY live sockets simultaneously — a
// phone and a tablet both looking at the same chat. Each attached socket
// receives every outbound frame (fan-out in [send]); per-client ack state
// ([clientAcks]) drives replay-buffer trimming. The previous single-client
// model silently shadowed the displaced socket, leaving it deaf; this design
// keeps both live.
//
// attach(nil) is the socketless restore path (startup binding reconstruction):
// it clears the live set without touching the durable buffer.
func (b *convBinding) attach(client *wsClient) {
	b.mu.Lock()
	if b.clients == nil {
		b.clients = make(map[*wsClient]struct{})
	}
	if client == nil {
		// Socketless restore: clear any live sockets (none should exist at
		// startup, but be safe) without evicting them — close is the caller's
		// responsibility, not attach's. Just drop our references. Leave the
		// cached feature union intact so checks still resolve while offline.
		b.clients = make(map[*wsClient]struct{})
		b.clientStates = nil
		b.mu.Unlock()
		return
	}
	b.clients[client] = struct{}{}
	if b.clientStates == nil {
		b.clientStates = make(map[*wsClient]*clientState)
	}
	if b.clientStates[client] == nil {
		b.clientStates[client] = &clientState{}
	}
	b.recomputeFeaturesLocked()
	b.mu.Unlock()
	client.mu.Lock()
	client.convByID[b.convID] = b
	client.mu.Unlock()
}

// detachIf removes `client` from the live set iff it is currently attached.
// Other attached sockets (a different device on the same conversation) stay
// live — only the specified socket is detached. The durable state (buffer,
// seen) is retained; the feature union is recomputed across who's left.
func (b *convBinding) detachIf(client *wsClient) {
	b.mu.Lock()
	delete(b.clients, client)
	delete(b.clientStates, client)
	b.recomputeFeaturesLocked()
	b.mu.Unlock()
}

// hasLiveClients reports whether any socket is still attached to this binding.
func (b *convBinding) hasLiveClients() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.clients) > 0
}

// recomputeFeaturesLocked rebuilds b.features as the UNION of advertised caps
// across currently-attached clients, so a less-capable device attaching second
// can't narrow gating for the whole binding. When the LAST client detaches
// (empty set) the previous union is retained, so offline checks keep resolving;
// while any client remains the union reflects exactly who's attached (an
// all-incapable set legitimately narrows it). Caller holds b.mu; this locks each
// client's mu (b.mu → client.mu order).
func (b *convBinding) recomputeFeaturesLocked() {
	if len(b.clients) == 0 {
		return
	}
	union := make(map[string]struct{})
	for c := range b.clients {
		c.mu.Lock()
		for f := range c.features {
			union[f] = struct{}{}
		}
		c.mu.Unlock()
	}
	b.features = union
}

// seedClientAck raises an attached client's acked high-water — used on resume so
// a pure-reader device (never sends a frame, so never enters ackInbound) still
// floors the trim from its resume point instead of pinning the buffer forever.
func (b *convBinding) seedClientAck(client *wsClient, ack int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if st := b.clientStates[client]; st != nil && ack > st.ackHW {
		st.ackHW = ack
	}
}

// featuresCSV serializes the cached feature union for persistence.
func (b *convBinding) featuresCSV() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	feats := make([]string, 0, len(b.features))
	for f := range b.features {
		feats = append(feats, f)
	}
	sort.Strings(feats)
	return strings.Join(feats, ",")
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
	kind, detail := b.resolveActivity()
	return fap.ConversationInfo{ID: b.convID, SessionKey: b.sessionKey, LastSeq: b.seq, Activity: string(kind), ActivityDetail: detail, LastActivityTs: b.lastActMs, LastPreview: b.lastPreview, CacheExpiryMs: b.cacheExpiryMs}
}

// resolveActivity collapses the turn-scoped and session-scoped inputs to a
// single (kind, detail) by the ActivityKind precedence:
//
//	subagents > waiting > tool > thinking > warming > typing > idle
//
// The turn-scoped inputs (turnKind/turnDetail) already carry exactly one of
// tool/thinking/warming/typing (appSink sets the latest per turn event), so the
// resolver only layers the two session-scoped states above them. Caller holds mu.
func (b *convBinding) resolveActivity() (fap.ActivityKind, string) {
	if b.subagentDetail != "" {
		return fap.ActivityKindSubagents, b.subagentDetail
	}
	if b.waitingDetail != "" {
		return fap.ActivityKindWaiting, b.waitingDetail
	}
	if b.turnKind != "" && b.turnKind != fap.ActivityKindIdle {
		return b.turnKind, b.turnDetail
	}
	return fap.ActivityKindIdle, ""
}

// setTurnActivity records the turn-scoped activity (kind + detail) and re-emits
// if the resolved value changed. Called by appSink off the turn-event stream.
func (b *convBinding) setTurnActivity(kind fap.ActivityKind, detail string) {
	b.applyActivity(func() {
		b.turnKind = kind
		b.turnDetail = detail
	})
}

// setCacheExpiry records the prompt-cache expiry (unix ms) and sends a
// CacheExpiry frame, deduping on an unchanged value. Called by appSink at turn
// completion (a turn refreshes the cache).
func (b *convBinding) setCacheExpiry(ms int64) {
	b.mu.Lock()
	changed := ms != b.cacheExpiryMs
	if changed {
		b.cacheExpiryMs = ms
	}
	convID := b.convID
	b.mu.Unlock()
	if changed {
		b.send(fap.CacheExpiry{ConversationID: convID, ExpiryMs: ms})
	}
}

// setSubagentDetail records the session-scoped running-subagent descriptions
// (empty = none running) and re-emits if the resolved value changed.
func (b *convBinding) setSubagentDetail(detail string) {
	b.applyActivity(func() { b.subagentDetail = detail })
}

// setWaitingDetail records the session-scoped target agent this conversation is
// awaiting a reply from (empty = not waiting) and re-emits on a resolved change.
func (b *convBinding) setWaitingDetail(detail string) {
	b.applyActivity(func() { b.waitingDetail = detail })
}

// applyActivity mutates the activity inputs under mu, recomputes the resolved
// (kind, detail), and — only on an actual change vs the last-emitted value —
// updates the cached snapshot (read by info()) and sends one Activity frame.
// The send happens after mu is released because b.send takes mu itself.
func (b *convBinding) applyActivity(mutate func()) {
	b.mu.Lock()
	mutate()
	kind, detail := b.resolveActivity()
	prevKind := b.activityKind
	if prevKind == "" {
		prevKind = fap.ActivityKindIdle
	}
	changed := kind != prevKind || detail != b.activityDetail
	if changed {
		b.activityKind = kind
		b.activityDetail = detail
	}
	convID := b.convID
	b.mu.Unlock()
	if changed {
		b.send(fap.Activity{ConversationID: convID, Kind: string(kind), Detail: detail})
	}
}

// send encodes a server frame with the next per-conversation seq and the
// current inbound ack, retains it in the replay buffer, and enqueues it on the
// attached socket (if any). Offline (no socket) it buffers only — reconnect
// replay (and push, a later slice) heal the gap.
// lastActivity returns the last visible frame's send time (unix ms) under
// the binding's lock, for recency comparisons.
func (b *convBinding) lastActivity() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastActMs
}

// stampAck rewrites a canonical (ack=0) wire's envelope ack for one client,
// falling back to the original wire on a zero ack or an encode error (never
// drops the frame — a slightly stale ack only delays the client's outbox trim).
func stampAck(wire string, ack int64) string {
	if ack <= 0 {
		return wire
	}
	out, err := fap.StampAck(wire, ack)
	if err != nil {
		log.Errorf("app", "stampAck: %v", err)
		return wire
	}
	return out
}

func (b *convBinding) send(frame fap.ServerFrame) {
	b.mu.Lock()
	b.seq++
	seq := b.seq
	// Encode the canonical wire once with ack=0; the buffer + durable store keep
	// this copy (its envelope id must stay identical to what each client sees, or
	// client-side dedup breaks). Each client's own inbound high-water is stamped
	// as the ack at enqueue below — devices number their streams independently, so
	// a shared max-ack would ack a lagging device for seqs it never sent (#2).
	wire, err := fap.Encode(frame, seq, 0, "", "")
	if err != nil {
		b.mu.Unlock()
		log.Errorf("app", "encode %s: %v", frame.Type(), err)
		return
	}
	now := time.Now()
	preview, visible := pushPreview(frame)
	if visible {
		b.lastPreview = preview
		b.lastActMs = now.UnixMilli()
	}
	b.buffer = append(b.buffer, bufferedFrame{seq: seq, wire: wire, sent: now})
	b.trimBufferLocked()
	// Snapshot the live socket set + each client's ack (its own inbound seqHW)
	// under the lock; enqueue outside it so a slow/stalled socket's
	// enqueueBlockWait can't hold b.mu and wedge every other client's send.
	type target struct {
		c   *wsClient
		ack int64
	}
	targets := make([]target, 0, len(b.clients))
	for c := range b.clients {
		var ack int64
		if st := b.clientStates[c]; st != nil {
			ack = st.seqHW
		}
		targets = append(targets, target{c, ack})
	}
	notify := b.notifyOffline
	store := b.store
	sessionKey := b.sessionKey
	b.mu.Unlock()

	// Persist the canonical (ack=0) wire to the durable backstop (async; survives
	// restart + the in-memory depth/TTL bound, so a long-offline phone can backfill
	// it). The visible flag marks user-facing content vs transient frames (typing).
	if store != nil {
		store.Append(b.convID, b.agentID, seq, wire, now.UnixMilli(), visible, preview)
	}

	// Fan out: every attached device receives the frame, ack-stamped for ITS own
	// stream. Per-client app dedups by envelope id and tracks its own resume point.
	for _, t := range targets {
		t.c.enqueue(stampAck(wire, t.ack))
	}

	// Wake every capable device that isn't currently connected — even if another
	// device is attached to this conversation live (the pusher excludes connected
	// devices, so an attached desktop no longer suppresses an offline phone's
	// wake). Offline devices reconnect and replay the buffered frame.
	if notify != nil && visible {
		notify(pushPayload{
			ConvID:     b.convID,
			Preview:    preview,
			AgentID:    b.agentID,
			SessionKey: sessionKey,
			ChatID:     b.chatID,
		})
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

// ackInbound records the piggybacked ack from `client` and trims the replay
// buffer to frames ALL attached clients have confirmed. Without per-client
// tracking, a second device reconnecting with ack=0 would let the buffer trim
// past frames the first device hadn't acked yet — losing replay history for
// the lagging device.
//
// If any attached client has not yet sent an ack (e.g. just connected, mid-
// replay), no trimming happens — their resume point may be far behind and we
// can't safely advance past them.
func (b *convBinding) ackInbound(client *wsClient, ack int64) {
	if ack <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// #4: ignore acks from a socket not attached to this binding — the reliability
	// gate can run before routeUserTurn attaches, and a ghost entry would pin the
	// trim floor forever.
	if _, live := b.clients[client]; !live {
		return
	}
	if b.clientStates == nil {
		b.clientStates = make(map[*wsClient]*clientState)
	}
	st := b.clientStates[client]
	if st == nil {
		st = &clientState{}
		b.clientStates[client] = st
	}
	if ack > st.ackHW {
		st.ackHW = ack
	}
	// Trim only past what EVERY attached client has confirmed. A client still at
	// ackHW 0 (just connected, mid-replay, and not resume-seeded) hasn't confirmed
	// anything — bail rather than trim frames its resume point may still need.
	minAck := int64(1 << 62)
	for c := range b.clients {
		s := b.clientStates[c]
		if s == nil || s.ackHW == 0 {
			return
		}
		if s.ackHW < minAck {
			minAck = s.ackHW
		}
	}
	drop := 0
	for drop < len(b.buffer) && b.buffer[drop].seq <= minAck {
		drop++
	}
	if drop > 0 {
		b.buffer = append(b.buffer[:0:0], b.buffer[drop:]...)
	}
}

// acceptInbound dedups an inbound frame by envelope id and advances THIS
// client's inbound seq high-water (stamped as the ack on frames sent back to it).
// Returns false if the frame is a duplicate (a resent outbox entry after
// reconnect) and must be dropped.
func (b *convBinding) acceptInbound(client *wsClient, id string, seq int64) bool {
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
	// Created even if attach hasn't run yet: the gate can fire before
	// routeUserTurn attaches, and this client's seqHW must not be lost.
	if b.clientStates == nil {
		b.clientStates = make(map[*wsClient]*clientState)
	}
	st := b.clientStates[client]
	if st == nil {
		st = &clientState{}
		b.clientStates[client] = st
	}
	if seq > st.seqHW {
		st.seqHW = seq
	}
	return true
}

// substituteResolvedAsk rewrites the wire of an `interactive` frame whose prompt
// is in `orphaned` (closed, but with no stored resolve frame) into an
// `interactive.edit` at the same seq/id/ts. On a cold replay the prompt row was
// never inserted, so the client's resolve() UPDATE touches nothing and the ask
// silently doesn't render — instead of resurrecting as a live prompt. Same seq
// keeps the reliable stream contiguous (a dropped frame would stall the client's
// resume mark forever). Any other frame is returned unchanged.
func substituteResolvedAsk(wire string, orphaned map[string]struct{}) string {
	if len(orphaned) == 0 {
		return wire
	}
	var env fap.Envelope
	if json.Unmarshal([]byte(wire), &env) != nil || env.T != fap.TypeInteractive {
		return wire
	}
	var p struct {
		ConversationID string `json:"conversationId"`
		PromptID       string `json:"promptId"`
	}
	if json.Unmarshal(env.D, &p) != nil {
		return wire
	}
	if _, ok := orphaned[p.PromptID]; !ok {
		return wire
	}
	out, err := fap.Encode(
		fap.InteractiveEdit{ConversationID: p.ConversationID, PromptID: p.PromptID},
		env.Seq, env.Ack, env.ID, env.TS)
	if err != nil {
		return wire
	}
	return out
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
	// Buffered/stored wire is the canonical ack=0 copy; stamp this client's own
	// inbound high-water so the replayed ack matches the live-send semantics.
	var ack int64
	if st := b.clientStates[client]; st != nil {
		ack = st.seqHW
	}
	store := b.store
	convID := b.convID
	b.mu.Unlock()

	// Asks closed on another platform (or before durable resolve tracking) have no
	// stored resolve frame, so their open `interactive` frame would resurrect as a
	// live prompt on cold replay. Rewrite each to a same-seq resolve so it renders
	// closed instead. Empty (the common case) makes the substitution a no-op.
	orphaned := store.OrphanedResolvedAsks(convID)

	// Backfill the gap the in-memory buffer can't cover — frames trimmed by
	// depth/TTL, or lost when this process restarted — from the durable store,
	// before the in-memory frames. memFloor is where memory takes over: store
	// frames at seq >= memFloor would duplicate `pending`, so stop there. No
	// in-memory frames (hasMem == false) → the store supplies everything > fromSeq.
	// A gap larger than maxResumeStoreReplay is finished by the client via GET /app/replay.
	storeCount := 0
	if store != nil && (!hasMem || fromSeq < memFloor-1) {
		for _, sf := range store.Range(convID, fromSeq, maxResumeStoreReplay) {
			if hasMem && sf.seq >= memFloor {
				break
			}
			client.enqueue(stampAck(substituteResolvedAsk(sf.wire, orphaned), ack))
			storeCount++
		}
	}
	for _, wire := range pending {
		client.enqueue(stampAck(substituteResolvedAsk(wire, orphaned), ack))
	}
	log.Debugf("app", "replayTo: conv=%s fromSeq=%d memFloor=%d fromStore=%d fromBuffer=%d", convID, fromSeq, memFloor, storeCount, len(pending))
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
	deviceID    string                  // from the client hello
	features    map[string]struct{}     // advertised client capabilities (from the hello)
	convByID    map[string]*convBinding // conversationId → binding
	openConvIDs map[string]struct{}     // conversations the app currently has open (its pager tabs)
}

// supportsFeature reports whether the binding's app advertised feat. It reads the
// cached set from the last hello rather than the live socket, so it stays true
// across a disconnect — a known-capable but offline app still gets capability-gated
// frames, queued and replayed on reconnect. Canonical check: route all capability
// gating through it.
func (b *convBinding) supportsFeature(feat string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.features[feat]
	return ok
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

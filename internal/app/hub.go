package app

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"path/filepath"
	"sort"
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
	sendBuffer   = 64      // per-socket outbound queue depth
)

// Reliability tuning (wire-protocol §3). The replay buffer + inbound dedup
// window turn at-least-once delivery into effectively exactly-once rendering on
// a phone that drops the socket constantly. These are the code defaults;
// [platforms.app] replay_buffer / replay_ttl override them per the config cascade.
const (
	defaultReplayBufferDepth = 1000           // max retained server frames per conversation
	defaultReplayTTL         = 24 * time.Hour // max age of a retained frame
	maxSeenInbound           = 4096           // per-conversation inbound dedup window (mirrors client)
	defaultDevicesFile       = "app-devices.json"
)

// Hub multiplexes all live app WebSockets. It owns the per-agent appConn
// registry, the sessionKey→conversation binding map, and the set of physical
// sockets. It implements platform.ConnectionSource[*appConn] so the generic
// adapter can expose it as a platform.ConnectionManager.
type Hub struct {
	deps    platform.ProviderDeps
	apiKey  string
	blobs   *blobStore
	tokens  *pushTokens
	pusher  *fcmPusher
	devices *deviceStore
	authLim *authLimiter

	host           string          // advertised in hello.caps.host (config [platforms.app].host)
	replayDepth    int             // config-driven replay buffer depth (0 = default)
	replayTTL      time.Duration   // config-driven replay buffer TTL (0 = default)
	allowedDevices map[string]bool // non-empty = pairing allowlist

	mu         sync.RWMutex
	agents     map[string]*appConn     // agentID → its connection
	agentOrder []string                // registration order (for default-agent resolution)
	convs      map[string]*convBinding // conversationId → durable conversation state (outlives sockets)
	bySession  map[string]*convBinding // sessionKey → conversation binding
	clients    map[*wsClient]struct{}  // live sockets
	prompts    map[string]*convBinding // promptID → binding (live interactive prompts)
}

func newHub(deps platform.ProviderDeps) *Hub {
	key := ""
	if deps.SecretStore != nil {
		if v, ok := deps.SecretStore.Get("app.api_key"); ok {
			key = strings.TrimSpace(v)
		}
	}
	if key == "" {
		log.Warnf("app", "no app.api_key secret — /app/ws endpoint will reject all connections until one is set")
	}

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

	h := &Hub{
		deps:      deps,
		apiKey:    key,
		blobs:     blobs,
		tokens:    tokens,
		devices:   newDeviceStore(devicePath),
		authLim:   newAuthLimiter(authFailMax, authFailWindow),
		agents:    make(map[string]*appConn),
		convs:     make(map[string]*convBinding),
		bySession: make(map[string]*convBinding),
		clients:   make(map[*wsClient]struct{}),
		prompts:   make(map[string]*convBinding),
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
		go h.blobs.reaper(deps.Ctx)
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

// authToken validates a credential. Returns (nil, true) for the master key,
// (device, true) for a valid per-device token, or (nil, false) otherwise.
func (h *Hub) authToken(token string) (*device, bool) {
	if token == "" {
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(h.apiKey)) == 1 {
		return nil, true
	}
	if h.devices != nil {
		if d, ok := h.devices.validToken(token); ok {
			return d, true
		}
	}
	return nil, false
}

// authenticate gates a request on the master key OR a device token, with
// per-IP failure lockout. On success it returns the device (nil for the master
// key) and true; on failure it writes the HTTP error and returns false.
func (h *Hub) authenticate(w http.ResponseWriter, r *http.Request) (*device, bool) {
	if h.apiKey == "" {
		http.Error(w, "app endpoint not configured", http.StatusServiceUnavailable)
		return nil, false
	}
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

// authMaster gates a request on the master key ONLY (pairing/management),
// rate-limited. Device tokens cannot mint or revoke other devices.
func (h *Hub) authMaster(w http.ResponseWriter, r *http.Request) bool {
	if h.apiKey == "" {
		http.Error(w, "app endpoint not configured", http.StatusServiceUnavailable)
		return false
	}
	ip := remoteIP(r)
	if h.authLim.blocked(ip) {
		http.Error(w, "too many auth failures", http.StatusTooManyRequests)
		return false
	}
	token := bearerToken(r)
	if token == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return false
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(h.apiKey)) != 1 {
		h.authLim.fail(ip)
		http.Error(w, "invalid credentials", http.StatusForbidden)
		return false
	}
	h.authLim.reset(ip)
	return true
}

// ServePair handles POST /app/pair: the master key mints a per-device token.
func (h *Hub) ServePair(w http.ResponseWriter, r *http.Request) {
	if !h.authMaster(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
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

// ServeDevices handles GET /app/devices: the master key lists paired devices
// (tokens omitted).
func (h *Hub) ServeDevices(w http.ResponseWriter, r *http.Request) {
	if !h.authMaster(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.devices.list())
}

// ServeRevoke handles POST /app/pair/revoke: the master key revokes a device's
// token and closes its live socket(s) with 4403.
func (h *Hub) ServeRevoke(w http.ResponseWriter, r *http.Request) {
	if !h.authMaster(w, r) {
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

func (h *Hub) BotForSession(sessionKey string) *appConn {
	if sessionKey == "" {
		return nil
	}
	return h.PrimaryBot(session.AgentIDFromKey(sessionKey))
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
		c.close()
	}
	return nil
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

	// Group live conversations by agent (the app's Room DB is the durable source
	// of its full thread list; the roster advertises agents + currently-bound
	// conversations and any the app resumes are re-attached on hello).
	byAgent := make(map[string][]fap.ConversationInfo)
	for _, b := range h.convs {
		byAgent[b.agentID] = append(byAgent[b.agentID], b.info())
	}
	out := make([]fap.AgentInfo, 0, len(order))
	for _, id := range order {
		convs := byAgent[id]
		sort.Slice(convs, func(i, j int) bool { return convs[i].ID < convs[j].ID })
		out = append(out, fap.AgentInfo{ID: id, Name: id, Conversations: convs})
	}
	return out
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
	b = &convBinding{convID: convID, sessionKey: sk, agentID: agentID, chatID: chatID, replayDepth: h.replayDepth, replayTTL: h.replayTTL, seen: make(map[string]struct{}), notifyOffline: h.pushNotify}
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
	b.buffer = append(b.buffer, bufferedFrame{seq: seq, wire: wire, sent: time.Now()})
	b.trimBufferLocked()
	client := b.client
	notify := b.notifyOffline
	b.mu.Unlock()

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
	pending := make([]string, 0, len(b.buffer))
	for _, bf := range b.buffer {
		if bf.seq > fromSeq {
			pending = append(pending, bf.wire)
		}
	}
	b.mu.Unlock()
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

	mu       sync.Mutex
	agentID  string                  // socket's bound agent (set on conversation.open / first message)
	deviceID string                  // from the client hello
	convByID map[string]*convBinding // conversationId → binding
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
	select {
	case c.send <- []byte(wire):
	case <-c.done:
	default:
		// Outbound queue full — drop and warn rather than block the turn.
		log.Warnf("app", "outbound queue full, dropping frame")
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
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Warnf("app", "read error: %v", err)
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

// ServeWS authenticates (Bearer app.api_key, constant-time) and upgrades the
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
	go client.writePump()
	client.readPump() // blocks until the socket closes
	log.Infof("app", "device disconnected")
}

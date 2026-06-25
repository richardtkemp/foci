package app

import (
	"context"
	"crypto/subtle"
	"hash/fnv"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"foci/internal/agent"
	"foci/internal/app/fap"
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

// Hub multiplexes all live app WebSockets. It owns the per-agent appConn
// registry, the sessionKey→conversation binding map, and the set of physical
// sockets. It implements platform.ConnectionSource[*appConn] so the generic
// adapter can expose it as a platform.ConnectionManager.
type Hub struct {
	deps   platform.ProviderDeps
	apiKey string

	mu         sync.RWMutex
	agents     map[string]*appConn     // agentID → its connection
	agentOrder []string                // registration order (for default-agent resolution)
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
	return &Hub{
		deps:      deps,
		apiKey:    key,
		agents:    make(map[string]*appConn),
		bySession: make(map[string]*convBinding),
		clients:   make(map[*wsClient]struct{}),
		prompts:   make(map[string]*convBinding),
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
	out := make([]fap.AgentInfo, 0, len(order))
	for _, id := range order {
		out = append(out, fap.AgentInfo{ID: id, Name: id})
	}
	return out
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

func (h *Hub) ensureBinding(client *wsClient, agentID, convID string) *convBinding {
	client.mu.Lock()
	if b := client.convByID[convID]; b != nil {
		client.mu.Unlock()
		return b
	}
	chatID := chatIDForConv(convID)
	sk := h.sessionKeyForChat(agentID, chatID)
	b := &convBinding{convID: convID, sessionKey: sk, agentID: agentID, client: client, chatID: chatID}
	client.convByID[convID] = b
	client.mu.Unlock()

	h.mu.Lock()
	h.bySession[sk] = b
	h.mu.Unlock()
	return b
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
	h.mu.Lock()
	delete(h.clients, c)
	c.mu.Lock()
	for _, b := range c.convByID {
		// Only drop the global mapping if it still points at this socket.
		if h.bySession[b.sessionKey] == b {
			delete(h.bySession, b.sessionKey)
		}
	}
	// Drop any live prompts bound to this socket — their buttons die with it.
	for id, b := range h.prompts {
		if b.client == c {
			delete(h.prompts, id)
		}
	}
	c.mu.Unlock()
	h.mu.Unlock()
}

// --- convBinding: one (conversation ⇔ session) on one socket ---

type convBinding struct {
	convID     string
	sessionKey string
	agentID    string
	client     *wsClient
	chatID     int64
	seq        int64 // server→app outbound sequence (atomic)
}

func (b *convBinding) nextSeq() int64 { return atomic.AddInt64(&b.seq, 1) }

// send encodes a server frame with the next per-conversation seq and queues it
// on the binding's socket. Best-effort: a full/closed socket drops the frame
// (reconnect + replay — a later slice — heals the gap).
func (b *convBinding) send(frame fap.ServerFrame) {
	wire, err := fap.Encode(frame, b.nextSeq(), 0, "", "")
	if err != nil {
		log.Errorf("app", "encode %s: %v", frame.Type(), err)
		return
	}
	b.client.enqueue(wire)
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
		_ = c.ws.Close()
		c.hub.removeClient(c)
	})
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
	if h.apiKey == "" {
		http.Error(w, "app endpoint not configured", http.StatusServiceUnavailable)
		return
	}
	token := ""
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		token = auth[len("Bearer "):]
	}
	if token == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(h.apiKey)) != 1 {
		http.Error(w, "invalid credentials", http.StatusForbidden)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Errorf("app", "ws upgrade: %v", err)
		return
	}
	client := newWsClient(ws, h)
	h.addClient(client)
	log.Infof("app", "device connected")
	go client.writePump()
	client.readPump() // blocks until the socket closes
	log.Infof("app", "device disconnected")
}

package app

import (
	"database/sql"
	"errors"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/fap"
	"foci/internal/turnevent"
)

// Message info (#2992): the app's message context menu shows how a message was
// made and delivered. Three sources feed it:
//
//   - The app sink measures each turn (first output, tool calls and tool time)
//     and, at TurnComplete, records the turn's ledger id and those figures
//     against every message the turn delivered (app_message_info). The ledger
//     id is the only link from an app message to its usage and cost.
//   - The cost ledger (api.db) supplies tokens, cost, calls, context fill,
//     stop reason and the turn's times, read at request time.
//   - The frame store supplies when the server sent a message and when the
//     backend consumed a user message (its message.consumed frame); a user
//     message's device send time and server receipt are recorded on arrival.
//
// A figure none of them holds is left out of the answer, and the app shows it
// as not recorded.

// turnInfoMeter measures one turn as its events pass through the app sink.
type turnInfoMeter struct {
	start, firstOutput time.Time
	toolCalls          int
	toolOpen           int
	toolSince          time.Time
	toolTime           time.Duration
	// pendingVoice is the synthesis of the clip about to be attached;
	// attachVoiceClip files it under the message the clip reads, in voice.
	pendingVoice *messageVoiceRecord
	voice        map[string]messageVoiceRecord
}

// messageVoiceRecord is one voice-mode clip's synthesis.
type messageVoiceRecord struct {
	provider string
	ms       int64
}

// observe notes ev, seen at now.
func (m *turnInfoMeter) observe(ev turnevent.Event, now time.Time) {
	switch ev.(type) {
	case turnevent.TurnStart:
		if m.start.IsZero() {
			m.start = now
		}
	case turnevent.TextDelta, turnevent.TextBlock, turnevent.ThinkingDelta, turnevent.ThinkingBlock:
		m.output(now)
	case turnevent.ToolCall:
		m.output(now)
		m.toolCalls++
		if m.toolOpen == 0 {
			m.toolSince = now
		}
		m.toolOpen++
	case turnevent.ToolResult:
		if m.toolOpen > 0 {
			m.toolOpen--
			if m.toolOpen == 0 {
				m.toolTime += now.Sub(m.toolSince)
			}
		}
	}
}

func (m *turnInfoMeter) output(now time.Time) {
	if m.firstOutput.IsZero() {
		m.firstOutput = now
	}
}

// messageTurnRecord is what app_message_info holds for an agent message.
type messageTurnRecord struct {
	turnID, trigger, via, effort      string
	startMs, firstOutputMs, endMs     int64 // 0 = not recorded
	dispatchedMs                      int64 // when the input went to the model; 0 = not recorded
	delivery, deliveryTurn            string
	deliveredMs                       int64
	toolMs                            int64
	toolCalls                         int
	measured                          bool // start/end/tool figures are real
	voiceProvider                     string
	voiceMs                           int64
	hasVoice                          bool
	deviceSentMs, serverRecvMs, rowMs int64
}

// record closes the measurement at end and returns the turn's record.
// Tools still running at the end count up to it.
func (m *turnInfoMeter) record(e turnevent.TurnComplete, end time.Time) messageTurnRecord {
	r := messageTurnRecord{turnID: e.TurnID, trigger: e.Trigger, via: e.Via, effort: e.Effort}
	if !e.DispatchedAt.IsZero() {
		r.dispatchedMs = e.DispatchedAt.UnixMilli()
	}
	if m.start.IsZero() {
		return r
	}
	tools := m.toolTime
	if m.toolOpen > 0 {
		tools += end.Sub(m.toolSince)
	}
	r.measured = true
	r.startMs, r.endMs = m.start.UnixMilli(), end.UnixMilli()
	r.toolMs, r.toolCalls = tools.Milliseconds(), m.toolCalls
	if !m.firstOutput.IsZero() {
		r.firstOutputMs = m.firstOutput.UnixMilli()
	}
	return r
}

// recordMessageInfo files the finished turn's Message info under every message
// it delivered. A turn with no ledger id (none to join usage to) and nothing
// measured records nothing.
func (s *appSink) recordMessageInfo(e turnevent.TurnComplete) {
	ids := s.backend.delivered
	if len(ids) == 0 || s.b.store == nil {
		return
	}
	r := s.info.record(e, time.Now())
	if r.turnID == "" && !r.measured {
		return
	}
	s.b.store.PutAgentMessageInfo(s.b.convID, ids, r, s.info.voice)
}

// PutAgentMessageInfo records turn r against each of msgIDs, with the voice
// clip of any that had one. A re-record (the same message id) replaces it.
func (s *frameStore) PutAgentMessageInfo(convID string, msgIDs []string, r messageTurnRecord, voice map[string]messageVoiceRecord) {
	if s == nil || len(msgIDs) == 0 {
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		appLog.Warnf("message info: begin: %v", err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	for _, id := range msgIDs {
		var provider, voiceMs any
		if v, ok := voice[id]; ok {
			provider, voiceMs = nullString(v.provider), v.ms
		}
		var start, first, end, toolMs, toolCalls, dispatched any
		if r.dispatchedMs != 0 {
			dispatched = r.dispatchedMs
		}
		if r.measured {
			start, end, toolMs, toolCalls = r.startMs, r.endMs, r.toolMs, r.toolCalls
			if r.firstOutputMs != 0 {
				first = r.firstOutputMs
			}
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO app_message_info
			(message_id, conv_id, role, turn_id, trigger, via, effort, turn_start_ms, first_output_ms,
			 turn_end_ms, tool_ms, tool_calls, voice_provider, voice_ms, dispatched_ms, created_ms)
			VALUES (?, ?, 'agent', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, convID, nullString(r.turnID), nullString(r.trigger), nullString(r.via), nullString(r.effort),
			start, first, end, toolMs, toolCalls, provider, voiceMs, dispatched, now); err != nil {
			appLog.Warnf("message info: record %s: %v", id, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		appLog.Warnf("message info: commit: %v", err)
	}
}

// PutUserMessageReceipt records when the server received user message msgID
// and the sending device's own send time (0 = unknown). The first record
// stands: a replayed copy of the frame after a reconnect does not move it.
func (s *frameStore) PutUserMessageReceipt(convID, msgID string, deviceSentMs, serverRecvMs int64) {
	if s == nil || convID == "" || msgID == "" {
		return
	}
	var sent any
	if deviceSentMs > 0 {
		sent = deviceSentMs
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO app_message_info
		(message_id, conv_id, role, device_sent_ms, server_recv_ms, created_ms) VALUES (?, ?, 'user', ?, ?, ?)`,
		msgID, convID, sent, serverRecvMs, serverRecvMs); err != nil {
		appLog.Warnf("message info: user receipt %s: %v", msgID, err)
	}
}

// messageInfoRow reads what app_message_info holds for msgID in convID.
func (s *frameStore) messageInfoRow(convID, msgID string) (messageTurnRecord, string, bool) {
	var r messageTurnRecord
	if s == nil {
		return r, "", false
	}
	var role string
	var turn, trigger, via, effort, provider, delivery, deliveryTurn sql.NullString
	var start, first, end, toolMs, toolCalls, voiceMs, sent, recv, dispatched, delivered sql.NullInt64
	err := s.db.QueryRow(`SELECT role, turn_id, trigger, via, effort, turn_start_ms, first_output_ms,
			turn_end_ms, tool_ms, tool_calls, voice_provider, voice_ms, device_sent_ms, server_recv_ms, created_ms,
			dispatched_ms, delivery, delivery_turn, delivered_ms
		FROM app_message_info WHERE message_id = ? AND conv_id = ?`, msgID, convID).
		Scan(&role, &turn, &trigger, &via, &effort, &start, &first, &end, &toolMs, &toolCalls,
			&provider, &voiceMs, &sent, &recv, &r.rowMs, &dispatched, &delivery, &deliveryTurn, &delivered)
	if errors.Is(err, sql.ErrNoRows) {
		return r, "", false
	}
	if err != nil {
		appLog.Warnf("message info: read %s: %v", msgID, err)
		return r, "", false
	}
	r.turnID, r.trigger, r.via, r.effort = turn.String, trigger.String, via.String, effort.String
	r.measured = start.Valid && end.Valid
	r.startMs, r.firstOutputMs, r.endMs = start.Int64, first.Int64, end.Int64
	r.toolMs, r.toolCalls = toolMs.Int64, int(toolCalls.Int64)
	r.hasVoice, r.voiceProvider, r.voiceMs = voiceMs.Valid, provider.String, voiceMs.Int64
	r.deviceSentMs, r.serverRecvMs = sent.Int64, recv.Int64
	r.dispatchedMs = dispatched.Int64
	r.delivery, r.deliveryTurn, r.deliveredMs = delivery.String, deliveryTurn.String, delivered.Int64
	return r, role, true
}

// turnMessageCount is how many messages of convID turn turnID delivered.
func (s *frameStore) turnMessageCount(convID, turnID string) int {
	if s == nil || turnID == "" {
		return 0
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM app_message_info WHERE conv_id = ? AND turn_id = ?`,
		convID, turnID).Scan(&n); err != nil {
		appLog.Warnf("message info: count %s: %v", turnID, err)
	}
	return n
}

// messageConsumedMs is when the server sent the message.consumed frame for
// user message msgID (the backend took it in), or 0.
func (s *frameStore) messageConsumedMs(convID, msgID string) int64 {
	if s == nil {
		return 0
	}
	var ms sql.NullInt64
	err := s.db.QueryRow(`SELECT MIN(sent_ms) FROM app_frames
		WHERE conv_id = ? AND instr(wire, '"t":"message.consumed"') > 0 AND instr(wire, ?) > 0`,
		convID, `"messageId":"`+msgID+`"`).Scan(&ms)
	if err != nil {
		appLog.Warnf("message info: consumed %s: %v", msgID, err)
	}
	return ms.Int64
}

// envelopeMillis is an envelope ts (RFC 3339) in unix milliseconds, or 0.
func envelopeMillis(ts string) int64 {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// handleMessageInfoGet answers one message.info.get on the asking socket.
func (h *Hub) handleMessageInfoGet(client *wsClient, f fap.MessageInfoGet) {
	if f.RequestID == "" || f.ConversationID == "" || f.MessageID == "" {
		return
	}
	h.mu.RLock()
	b := h.convs[f.ConversationID]
	h.mu.RUnlock()
	var sessionKey string
	if b != nil {
		sessionKey = b.key()
	}
	client.sendRaw(buildMessageInfo(h.frames, accounting.Live(), f, sessionKey))
}

// buildMessageInfo assembles the answer from the frame store and the ledger
// (either may be nil). sessionKey is the conversation's session, "" if the
// conversation is not bound.
func buildMessageInfo(store *frameStore, ledger *accounting.Ledger, f fap.MessageInfoGet, sessionKey string) fap.MessageInfo {
	out := fap.MessageInfo{RequestID: f.RequestID, ConversationID: f.ConversationID, MessageID: f.MessageID, SessionKey: sessionKey}
	if p, ok := store.MessagePreviews(f.ConversationID, map[string]struct{}{f.MessageID: {}})[f.MessageID]; ok {
		out.ServerSentMs = p.SentMs
	}
	row, role, ok := store.messageInfoRow(f.ConversationID, f.MessageID)
	if !ok {
		out.Note = "The server has no record of how this message was made: it predates message info, came from elsewhere, or its record has expired."
		return out
	}
	if role == "user" {
		out.User = userMessageInfo(store, f, row)
		return out
	}
	if row.hasVoice {
		out.Voice = &fap.MessageVoiceInfo{Provider: row.voiceProvider, SynthesisMs: row.voiceMs}
	}
	out.Turn = agentTurnInfo(store, ledger, f.ConversationID, row)
	return out
}

func agentTurnInfo(store *frameStore, ledger *accounting.Ledger, convID string, row messageTurnRecord) *fap.MessageTurnInfo {
	t := &fap.MessageTurnInfo{TurnID: row.turnID, Trigger: row.trigger, Via: row.via, Effort: row.effort,
		Messages: store.turnMessageCount(convID, row.turnID)}
	if row.measured {
		t.StartedMs, t.EndedMs = row.startMs, row.endMs
		toolCalls, toolMs := int64(row.toolCalls), row.toolMs
		t.ToolCalls, t.ToolMs = &toolCalls, &toolMs
		// Model time starts when the input went to the model, after the
		// lock or the backend write, never at the sink's TurnStart: a turn
		// with no recorded dispatch has no first-output or generation time.
		if row.dispatchedMs > 0 {
			wait := max(row.dispatchedMs-row.startMs, 0)
			gen := max(row.endMs-row.dispatchedMs-row.toolMs, 0)
			t.WaitMs, t.GenerationMs = &wait, &gen
			if row.firstOutputMs > 0 {
				first := max(row.firstOutputMs-row.dispatchedMs, 0)
				t.FirstOutputMs = &first
			}
		}
	}
	d, err := ledger.TurnDetail(row.turnID)
	if err != nil {
		appLog.Warnf("message info: %v", err)
	}
	if d == nil {
		return t
	}
	if !row.measured {
		t.StartedMs = d.StartedAt.UnixMilli()
		if !d.EndedAt.IsZero() {
			t.EndedMs = d.EndedAt.UnixMilli()
		}
	}
	t.Running = d.Running
	t.Backend, t.Model, t.StopReason = d.Backend, d.FinalModel, d.StopReason
	t.Tokens, t.SubagentTokens = classCounts(d.Tokens), classCounts(d.SubagentTokens)
	calls, subCalls, comps := int64(d.Calls), int64(d.SubagentCalls), int64(d.Compactions)
	cost, subCost := d.CostUSD, d.SubagentCostUSD
	t.Calls, t.SubagentCalls, t.Compactions = &calls, &subCalls, &comps
	t.CostUsd, t.SubagentCostUsd, t.UnpricedCalls = &cost, &subCost, int64(d.UnpricedCalls)
	if d.CompactionCostUSD > 0 {
		cc := d.CompactionCostUSD
		t.CompactionCostUsd = &cc
	}
	if d.ContextFill > 0 {
		fill := int64(d.ContextFill)
		t.ContextTokens = &fill
	}
	spawns, err := ledger.TurnSpawns(row.turnID)
	if err != nil {
		appLog.Warnf("message info: %v", err)
	}
	// A turn with a row here ran on a build that records its spawns.
	t.SpawnsRecorded = err == nil
	for _, sp := range spawns {
		info := fap.MessageSpawnInfo{SpawnID: sp.SpawnID, Name: sp.Name, Mode: sp.Mode, SessionKey: sp.ChildSession,
			StartedMs: sp.StartedAt.UnixMilli(), InTurnCost: sp.InParentTurn}
		if sp.Priced {
			c := sp.CostUSD
			info.CostUsd, info.UnpricedCalls = &c, int64(sp.UnpricedCalls)
		}
		t.Spawns = append(t.Spawns, info)
	}
	return t
}

func classCounts[K ~string](m map[K]int) map[string]int64 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[string(k)] = int64(v)
	}
	return out
}

// userMessageInfo is a user message's delivery: its receipt, the outcome the
// agent recorded when it decided how the message reached it, and the
// backend's consumption ack (message.consumed) when one came.
func userMessageInfo(store *frameStore, f fap.MessageInfoGet, row messageTurnRecord) *fap.MessageUserInfo {
	return &fap.MessageUserInfo{
		DeviceSentMs: row.deviceSentMs, ServerReceivedMs: row.serverRecvMs,
		Delivery: row.delivery, TurnID: row.deliveryTurn, DeliveredMs: row.deliveredMs,
		AgentReceivedMs: store.messageConsumedMs(f.ConversationID, f.MessageID),
	}
}

// PutUserDelivery records how user message msgID reached the agent, as the
// agent decided it (agent.Delivery*): outcome, the turn it started, joined or
// was steered into, and when. A message that waited (queued) and then began a
// turn stays "queued", now naming that turn: the wait is the story. Any other
// outcome replaces what was there. A message with no receipt row (it did not
// come through the app) is left alone.
func (s *frameStore) PutUserDelivery(convID, msgID, outcome, turnID string, at time.Time) {
	if s == nil || msgID == "" || outcome == "" {
		return
	}
	if _, err := s.db.Exec(`UPDATE app_message_info SET
			delivery = CASE WHEN delivery = 'queued' AND ? = 'started' THEN 'queued' ELSE ? END,
			delivery_turn = COALESCE(?, delivery_turn),
			delivered_ms = CASE WHEN ? = 'queued' THEN delivered_ms ELSE ? END
		WHERE message_id = ? AND conv_id = ? AND role = 'user'`,
		outcome, outcome, nullString(turnID), outcome, at.UnixMilli(), msgID, convID); err != nil {
		appLog.Warnf("message info: delivery %s: %v", msgID, err)
	}
}

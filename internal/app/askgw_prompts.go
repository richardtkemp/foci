package app

import (
	"strings"
	"sync"

	"foci/internal/fap"
	"foci/internal/platform"
)

// Ask-gateway prompts (#2707). A question an external App asks through
// foci's askgw (e.g. an aisudo sudo approval) belongs to no agent, so on the
// app it is not posted into any conversation: the hub holds the pending set
// and mirrors it WHOLE to every socket as fap.AskgwPrompts — on each change
// and after each hello. The app shows the first as a blocking dialog over
// whatever is on screen. The first answer from any device wins; the next
// snapshot closes the dialog on the others.

// askgwPrompt is one pending prompt: the frame the app renders, plus the
// callback that resolves the askgw ask when a choice is tapped.
type askgwPrompt struct {
	frame    fap.AskgwPrompt
	onChoice func(data string)
}

// featureAskgwPrompts is the ClientHello feature a client advertises when it
// renders askgw.prompts (#2707). Only such sockets are sent the frame: an
// older client would raise its "update needed" warning on every hello.
const featureAskgwPrompts = "askgwPrompts"

// askgwPromptSet is the hub's pending askgw prompts, in arrival order.
type askgwPromptSet struct {
	mu    sync.Mutex
	order []string
	byID  map[string]*askgwPrompt

	// sendMu orders the snapshots on the wire: held from taking a snapshot
	// until it is queued on every socket, so a slower older snapshot can never
	// land after a newer one and resurrect a closed dialog.
	sendMu sync.Mutex
}

// put adds or replaces the prompt id. A replaced prompt keeps its place.
func (s *askgwPromptSet) put(id string, p *askgwPrompt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID == nil {
		s.byID = make(map[string]*askgwPrompt)
	}
	if _, ok := s.byID[id]; !ok {
		s.order = append(s.order, id)
	}
	s.byID[id] = p
}

// take removes and returns the prompt id, or nil when it is not pending. Only
// the caller that gets it non-nil may act on it (claim-under-lock).
func (s *askgwPromptSet) take(id string) *askgwPrompt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removeLocked(id)
}

// takeIfChoice is take, but only when data is one of the prompt's choices. A
// tap carrying any other data must not consume the prompt: the askgw ask
// could not resolve it and would hang until its timeout. known reports
// whether id was pending at all.
func (s *askgwPromptSet) takeIfChoice(id, data string) (p *askgwPrompt, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	for _, c := range cur.frame.Choices {
		if c.Data == data {
			return s.removeLocked(id), true
		}
	}
	return nil, true
}

// removeLocked deletes id and returns it (nil when absent). s.mu must be held.
func (s *askgwPromptSet) removeLocked(id string) *askgwPrompt {
	p, ok := s.byID[id]
	if !ok {
		return nil
	}
	delete(s.byID, id)
	for i, o := range s.order {
		if o == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return p
}

// snapshot is the frame listing every pending prompt, oldest first. Prompts
// is never nil: an empty list is the "close any dialog" signal.
func (s *askgwPromptSet) snapshot() fap.AskgwPrompts {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]fap.AskgwPrompt, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.byID[id].frame)
	}
	return fap.AskgwPrompts{Prompts: out}
}

// presentAskgw makes id a pending prompt on every device and wakes the
// offline ones. onChoice receives the tapped choice's Data, once. It refuses
// (false, nothing registered) when no device could show the prompt — see
// askgwReachable — so the caller posts it to the agent chat instead: an askgw
// question (a sudo approval) must never be invisible.
//
// host is the host the ask came from (#2800), which the dialog shows; empty
// when unknown.
func (h *Hub) presentAskgw(id, header, text, host string, buttons []platform.ButtonChoice, onChoice func(data string)) bool {
	if !h.askgwReachable() {
		if h.tokens.askgwSeenSinceStart() {
			appLog.Infof("askgw: no device can show prompt %s (none connected or push-wakeable with %s), falling back to the agent chat", id, featureAskgwPrompts)
		} else {
			appLog.Infof("askgw: no device can show prompt %s (none has advertised %s since this gateway started: the list is in memory, so a restart empties it until a device reconnects), falling back to the agent chat", id, featureAskgwPrompts)
		}
		return false
	}
	choices := make([]fap.Choice, 0, len(buttons))
	for _, b := range buttons {
		if b.Toggle != nil {
			continue // a non-terminal toggle has no meaning in a one-shot dialog
		}
		choices = append(choices, fap.Choice{Label: b.Label, Data: b.Data, Row: b.Row, Description: b.Description})
	}
	h.askgw.put(id, &askgwPrompt{
		frame:    fap.AskgwPrompt{PromptID: id, Header: header, Text: text, Host: host, Choices: choices},
		onChoice: onChoice,
	})
	h.broadcastAskgw()
	if h.pusher != nil {
		h.pusher.notifyAskgw(id, askgwPreview(header, text), h.connectedDeviceIDs())
	}
	return true
}

// askgwReachable reports whether at least one device can show an askgw prompt
// now or when woken: a live socket whose hello advertised featureAskgwPrompts,
// or (push configured) an offline device with a push token whose latest hello
// advertised it. Only old-build devices, or none at all, means false.
func (h *Hub) askgwReachable() bool {
	for _, c := range h.snapshotClients() {
		if askgwCapable(c) {
			return true
		}
	}
	return h.pusher != nil && len(h.tokens.askgwTargetsExcluding(h.connectedDeviceIDs())) > 0
}

// closeAskgw withdraws id (cancelled, timed out). Reports whether it was
// pending; only then does every device hear the new set.
func (h *Hub) closeAskgw(id string) bool {
	if h.askgw.take(id) == nil {
		return false
	}
	h.broadcastAskgw()
	return true
}

// handleAskgwResponse resolves the prompt a device answered. An answer for a
// prompt no longer pending (answered on another device, cancelled, timed out)
// or carrying an unknown choice is dropped, and the sender gets the current
// set so its stale dialog closes.
func (h *Hub) handleAskgwResponse(client *wsClient, f fap.AskgwResponse) {
	p, known := h.askgw.takeIfChoice(f.PromptID, f.Data)
	if p == nil {
		if known {
			appLog.Warnf("askgw: response for prompt %s carries unknown choice %q, ignored", f.PromptID, f.Data)
		} else {
			appLog.Infof("askgw: response for prompt %s no longer pending, ignored", f.PromptID)
		}
		h.pushAskgw(client)
		return
	}
	// The callback may present the ask's next question (a new prompt id)
	// synchronously; the broadcast after it carries both changes at once.
	if p.onChoice != nil {
		p.onChoice(f.Data)
	}
	h.broadcastAskgw()
}

// broadcastAskgw sends the current pending set to every capable socket.
func (h *Hub) broadcastAskgw() {
	h.askgw.sendMu.Lock()
	defer h.askgw.sendMu.Unlock()
	snap := h.askgw.snapshot()
	for _, c := range h.snapshotClients() {
		if askgwCapable(c) {
			c.sendRaw(snap)
		}
	}
}

// pushAskgw sends the pending set to one socket: a just-connected one, or one
// whose answer was stale. Sent even when empty, so a device that missed a
// close while offline drops its dialog.
func (h *Hub) pushAskgw(client *wsClient) {
	if !askgwCapable(client) {
		return
	}
	h.askgw.sendMu.Lock()
	defer h.askgw.sendMu.Unlock()
	client.sendRaw(h.askgw.snapshot())
}

// askgwCapable reports whether c's hello advertised featureAskgwPrompts.
func askgwCapable(c *wsClient) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.features[featureAskgwPrompts]
	return ok
}

// askgwPreviewMax bounds the wake push's preview, in runes.
const askgwPreviewMax = 140

// askgwPreview is the one-line notification text for a prompt: its header
// and the body's whitespace-collapsed text, truncated.
func askgwPreview(header, text string) string {
	s := strings.Join(strings.Fields(text), " ")
	if header != "" {
		s = header + ": " + s
	}
	if r := []rune(s); len(r) > askgwPreviewMax {
		s = string(r[:askgwPreviewMax-1]) + "…"
	}
	return s
}

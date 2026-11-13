package app

import (
	"fmt"
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
// callback that resolves it when a choice is tapped. value is the text typed
// in the prompt's ValueInput step (#3090), empty for every other answer.
type askgwPrompt struct {
	frame    fap.AskgwPrompt
	onAnswer func(data, value string)
}

// featureAskgwPrompts is the ClientHello feature a client advertises when it
// renders askgw.prompts (#2707). Only such sockets are sent the frame: an
// older client would raise its "update needed" warning on every hello.
const featureAskgwPrompts = "askgwPrompts"

// featureAskgwValueInput is the ClientHello feature a client advertises when
// its askgw dialog renders a prompt's ValueInput step (#3090). A prompt with
// one is sent only to such sockets: an older dialog would show its choices
// and answer without the value.
const featureAskgwValueInput = "askgwValueInput"

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

// takeIfValid is take, but only when the answer fits the prompt: data is one
// of its choices (or its ValueInput's cancel), and a value comes exactly with
// the ValueInput choice. An answer carrying anything else must not consume the
// prompt: its owner could not resolve it and would hang until its timeout.
// known reports whether id was pending at all.
func (s *askgwPromptSet) takeIfValid(id, data, value string) (p *askgwPrompt, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	if validAnswer(cur.frame, data, value) {
		return s.removeLocked(id), true
	}
	return nil, true
}

// validAnswer reports whether data/value is an answer f can take.
func validAnswer(f fap.AskgwPrompt, data, value string) bool {
	if vi := f.ValueInput; vi != nil {
		switch data {
		case vi.Choice:
			return value != ""
		case vi.CancelData:
			return value == ""
		}
	}
	if value != "" {
		return false
	}
	for _, c := range f.Choices {
		if c.Data == data {
			return true
		}
	}
	return false
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
// is never nil: an empty list is the "close any dialog" signal. Without
// withValue, prompts carrying a ValueInput are left out (a client that
// cannot render the value step).
func (s *askgwPromptSet) snapshot(withValue bool) fap.AskgwPrompts {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]fap.AskgwPrompt, 0, len(s.order))
	for _, id := range s.order {
		f := s.byID[id].frame
		if f.ValueInput != nil && !withValue {
			continue
		}
		out = append(out, f)
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
	h.putAskgw(fap.AskgwPrompt{PromptID: id, Header: header, Text: text, Host: host, Choices: askgwChoices(buttons)},
		func(data, _ string) {
			if onChoice != nil {
				onChoice(data)
			}
		})
	return true
}

// askgwChoices turns buttons into dialog choices.
func askgwChoices(buttons []platform.ButtonChoice) []fap.Choice {
	choices := make([]fap.Choice, 0, len(buttons))
	for _, b := range buttons {
		if b.Toggle != nil {
			continue // a non-terminal toggle has no meaning in a one-shot dialog
		}
		choices = append(choices, fap.Choice{Label: b.Label, Data: b.Data, Row: b.Row, Description: b.Description})
	}
	return choices
}

// putAskgw registers f as pending, sends the new set and wakes the offline
// devices that can show it.
func (h *Hub) putAskgw(f fap.AskgwPrompt, onAnswer func(data, value string)) {
	h.askgw.put(f.PromptID, &askgwPrompt{frame: f, onAnswer: onAnswer})
	h.broadcastAskgw()
	if h.pusher != nil {
		h.pusher.notifyAskgw(f.PromptID, askgwPreview(f.Header, f.Text), h.connectedDeviceIDs(), f.ValueInput != nil)
	}
}

// presentAppPrompt shows an app-only prompt (#3090: an agent's secret
// request) in the same app-wide dialog as askgw prompts. Unlike presentAskgw
// there is no chat fallback: it returns an error naming why no device can
// show it, and registers nothing. value, when non-nil, adds the typed-value
// step; then only devices advertising featureAskgwValueInput count. warning,
// when set, is shown large and red above the text (#3092).
func (h *Hub) presentAppPrompt(id, header, text, warning string, buttons []platform.ButtonChoice, value *fap.ValueInput, onAnswer func(data, value string)) error {
	needValue := value != nil
	if !h.appPromptReachable(needValue) {
		feature := featureAskgwPrompts
		if needValue {
			feature = featureAskgwValueInput
		}
		return fmt.Errorf("no foci app device can show it: none is connected or push-wakeable with an app build that advertises %s (update the app, or open it if it has not connected since the gateway started)", feature)
	}
	h.putAskgw(fap.AskgwPrompt{PromptID: id, Header: header, Text: text, Warning: warning, Choices: askgwChoices(buttons), ValueInput: value}, onAnswer)
	return nil
}

// appPromptReachable is askgwReachable for a prompt that may need the value
// step: with needValue, a device must also advertise featureAskgwValueInput.
func (h *Hub) appPromptReachable(needValue bool) bool {
	if !needValue {
		return h.askgwReachable()
	}
	for _, c := range h.snapshotClients() {
		if askgwCapable(c) && askgwValueCapable(c) {
			return true
		}
	}
	return h.pusher != nil && len(h.tokens.askgwValueTargetsExcluding(h.connectedDeviceIDs())) > 0
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
	p, known := h.askgw.takeIfValid(f.PromptID, f.Data, f.Value)
	if p == nil {
		if known {
			// Never log f.Value: it is a secret typed in a value step.
			appLog.Warnf("askgw: response for prompt %s carries an answer it cannot take (choice %q, value given: %t), ignored", f.PromptID, f.Data, f.Value != "")
		} else {
			appLog.Infof("askgw: response for prompt %s no longer pending, ignored", f.PromptID)
		}
		h.pushAskgw(client)
		return
	}
	// The callback may present the ask's next question (a new prompt id)
	// synchronously; the broadcast after it carries both changes at once.
	if p.onAnswer != nil {
		p.onAnswer(f.Data, f.Value)
	}
	h.broadcastAskgw()
}

// broadcastAskgw sends the current pending set to every capable socket.
func (h *Hub) broadcastAskgw() {
	h.askgw.sendMu.Lock()
	defer h.askgw.sendMu.Unlock()
	full, plain := h.askgw.snapshot(true), h.askgw.snapshot(false)
	for _, c := range h.snapshotClients() {
		if !askgwCapable(c) {
			continue
		}
		if askgwValueCapable(c) {
			c.sendRaw(full)
		} else {
			c.sendRaw(plain)
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
	client.sendRaw(h.askgw.snapshot(askgwValueCapable(client)))
}

// askgwCapable reports whether c's hello advertised featureAskgwPrompts.
func askgwCapable(c *wsClient) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.features[featureAskgwPrompts]
	return ok
}

// askgwValueCapable reports whether c's hello advertised featureAskgwValueInput.
func askgwValueCapable(c *wsClient) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.features[featureAskgwValueInput]
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

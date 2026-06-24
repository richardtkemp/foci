package ccstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
)

// ---------------------------------------------------------------------------
// Elicitation state
// ---------------------------------------------------------------------------

// pendingElicitation tracks an unresolved elicitation request from CC.
// The lifecycle mirrors pendingPermission but is kept in a separate map:
// elicitations aren't tied to tool_use_ids, and mixing them with permissions
// would force a discriminator through otherwise-clean code paths.
//
// For form mode, fieldOrder drives a sequential walk over schema.Order. Each
// user response advances currentField and accumulates into answers; once all
// fields are satisfied, answers is marshalled into the content object of the
// control_response.
//
// For url mode, fieldOrder is empty and the flow resolves on a single button
// click (Done/Decline/Cancel) or on an inbound elicitation_complete system
// message matching elicitationID.
type pendingElicitation struct {
	requestID     string
	serverName    string
	message       string
	mode          string // "form"|"url" (empty treated as "form")
	url           string
	elicitationID string
	schema        *elicSchema // nil for url mode or unparsable form schemas

	// Form-mode walk state
	fieldOrder   []string               // property names in declaration order
	currentField int                    // next field to collect
	answers      map[string]interface{} // accumulated answers, coerced to schema type

	createdAt time.Time
}

// elicSchema is the minimal subset of JSON Schema we understand for form
// rendering. We support a top-level object with string/number/integer/boolean
// properties, plus enum values (rendered as button choices). Anything more
// exotic (nested objects, arrays, oneOf, etc.) triggers the fallback path
// where the user can only accept-empty / decline / cancel.
type elicSchema struct {
	Properties map[string]elicProperty
	Required   map[string]bool
	Order      []string // declaration order, preserved during parse
}

// elicProperty describes a single schema property.
type elicProperty struct {
	Type        string // "string"|"number"|"integer"|"boolean"
	Description string
	Title       string
	Enum        []string // if non-empty, rendered as buttons
}

// ---------------------------------------------------------------------------
// Schema parser
// ---------------------------------------------------------------------------

// parseElicSchema extracts the supported subset of a JSON-Schema object.
// Returns nil on any input we can't render (missing properties, non-object
// root, unsupported property types). The caller treats nil as "fall back to
// decline/cancel-only UI" — we never block CC waiting for a form we can't
// actually collect.
func parseElicSchema(raw json.RawMessage) *elicSchema {
	if len(raw) == 0 {
		return nil
	}

	// First pass with the standard decoder to get properties+required.
	var top struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil
	}
	if top.Type != "" && top.Type != "object" {
		return nil
	}
	if len(top.Properties) == 0 {
		return nil
	}

	// Second pass: a streaming decoder walks the "properties" object to
	// capture declaration order. JSON Schema is order-sensitive for UX —
	// users expect fields in the order the server declared them.
	order, err := propertyOrder(raw)
	if err != nil || len(order) == 0 {
		return nil
	}

	schema := &elicSchema{
		Properties: make(map[string]elicProperty, len(top.Properties)),
		Required:   make(map[string]bool, len(top.Required)),
		Order:      order,
	}
	for _, req := range top.Required {
		schema.Required[req] = true
	}
	for _, name := range order {
		propRaw, ok := top.Properties[name]
		if !ok {
			continue
		}
		prop, ok := parseElicProperty(propRaw)
		if !ok {
			return nil
		}
		schema.Properties[name] = prop
	}
	if len(schema.Properties) == 0 {
		return nil
	}
	return schema
}

// propertyOrder walks the top-level "properties" object of a schema and
// returns the property names in the order they appear. json.Decoder is used
// (not encoding/json's map unmarshal) because map iteration is unordered.
func propertyOrder(raw json.RawMessage) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Expect top-level object.
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("schema root is not an object")
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("non-string schema key")
		}
		if key != "properties" {
			// Skip this value by decoding it into a discard target.
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				return nil, err
			}
			continue
		}
		// Found "properties" — walk its keys.
		if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
			return nil, fmt.Errorf("properties is not an object")
		}
		var order []string
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			propName, ok := keyTok.(string)
			if !ok {
				return nil, fmt.Errorf("non-string property key")
			}
			order = append(order, propName)
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				return nil, err
			}
		}
		return order, nil
	}
	return nil, nil
}

// parseElicProperty decodes a single property definition, returning ok=false
// if the property's type isn't one we know how to render.
func parseElicProperty(raw json.RawMessage) (elicProperty, bool) {
	var p struct {
		Type        string   `json:"type"`
		Description string   `json:"description"`
		Title       string   `json:"title"`
		Enum        []string `json:"enum"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return elicProperty{}, false
	}
	switch p.Type {
	case "string", "number", "integer", "boolean":
		// Supported.
	default:
		return elicProperty{}, false
	}
	return elicProperty{
		Type:        p.Type,
		Description: p.Description,
		Title:       p.Title,
		Enum:        p.Enum,
	}, true
}

// ---------------------------------------------------------------------------
// Handler: request dispatch from the reader goroutine
// ---------------------------------------------------------------------------

// OnElicitationRequest handles an elicitation control_request from CC.
// It builds pending state, stores it, and presents the first prompt to the
// user via permPromptFn — reusing the same platform pipeline as permissions.
func (b *Backend) OnElicitationRequest(msg *ElicitationRequest) {
	b.touchActivity()
	b.handleElicitation(msg)
}

func (b *Backend) handleElicitation(msg *ElicitationRequest) {
	log.Debugf("ccstream/elic", "handleElicitation: req_id=%s server=%s mode=%s",
		msg.RequestID, msg.Request.McpServerName, msg.Request.Mode)

	pe := &pendingElicitation{
		requestID:     msg.RequestID,
		serverName:    msg.Request.McpServerName,
		message:       msg.Request.Message,
		mode:          msg.Request.Mode,
		url:           msg.Request.URL,
		elicitationID: msg.Request.ElicitationID,
		createdAt:     time.Now(),
		answers:       map[string]interface{}{},
	}

	if msg.Request.Mode == "url" {
		b.storePendingElicit(pe)
		b.outstanding.Register(msg.RequestID, OutstandingElicitation)
		b.presentElicitationURL(pe)
		return
	}

	// Form mode (default when mode is empty).
	schema := parseElicSchema(msg.Request.RequestedSchema)
	if schema != nil && len(schema.Order) > 0 {
		pe.schema = schema
		pe.fieldOrder = schema.Order
	}

	b.storePendingElicit(pe)
	b.outstanding.Register(msg.RequestID, OutstandingElicitation)

	if pe.schema == nil {
		b.presentElicitationFallback(pe)
		return
	}
	b.presentElicitationField(pe, true)
}

// ---------------------------------------------------------------------------
// Prompt presentation
// ---------------------------------------------------------------------------

// presentElicitationURL renders a URL-mode prompt with a Done/Decline/Cancel
// button set. The Done button sends action=accept with no content; the MCP
// server is expected to read the state from its own backchannel.
func (b *Backend) presentElicitationURL(pe *pendingElicitation) {
	var body strings.Builder
	body.WriteString("**Elicitation Required**\n\n")
	if pe.serverName != "" {
		body.WriteString(fmt.Sprintf("MCP server **%s** needs your input:\n\n", pe.serverName))
	}
	if pe.message != "" {
		body.WriteString(pe.message)
		body.WriteString("\n\n")
	}
	if pe.url != "" {
		body.WriteString(pe.url)
	}

	choices := []delegator.PromptChoice{
		{Label: "Done", Data: "elic:accept"},
		{Label: "Decline", Data: "elic:decline"},
		{Label: "Cancel", Data: "elic:cancel"},
	}

	b.callPrompt(pe.requestID, body.String(), pe.serverName, choices)
}

// presentElicitationFallback is used when the schema is missing or
// unrenderable. The user can only decline or cancel — we never fabricate
// field values when we don't know what the server is asking for.
func (b *Backend) presentElicitationFallback(pe *pendingElicitation) {
	var body strings.Builder
	body.WriteString("**Elicitation Required**\n\n")
	if pe.serverName != "" {
		body.WriteString(fmt.Sprintf("MCP server **%s** needs structured input, but foci could not render the form schema.\n\n",
			pe.serverName))
	}
	if pe.message != "" {
		body.WriteString(pe.message)
		body.WriteString("\n\n")
	}
	body.WriteString("_Only decline/cancel are available for this request._")

	choices := []delegator.PromptChoice{
		{Label: "Decline", Data: "elic:decline"},
		{Label: "Cancel", Data: "elic:cancel"},
	}
	b.callPrompt(pe.requestID, body.String(), pe.serverName, choices)
}

// presentElicitationField renders the current field of a form-mode
// elicitation. When first==true, the message header is included so the user
// sees the server's intent alongside the first prompt.
func (b *Backend) presentElicitationField(pe *pendingElicitation, first bool) {
	if pe.currentField >= len(pe.fieldOrder) {
		return
	}
	name := pe.fieldOrder[pe.currentField]
	prop := pe.schema.Properties[name]
	total := len(pe.fieldOrder)

	var body strings.Builder
	if first {
		if pe.serverName != "" {
			body.WriteString(fmt.Sprintf("**Elicitation — %s**\n\n", pe.serverName))
		} else {
			body.WriteString("**Elicitation Required**\n\n")
		}
		if pe.message != "" {
			body.WriteString(pe.message)
			body.WriteString("\n\n")
		}
	}

	label := prop.Title
	if label == "" {
		label = name
	}
	body.WriteString(fmt.Sprintf("**Field %d/%d: %s**", pe.currentField+1, total, label))
	if pe.schema.Required[name] {
		body.WriteString(" *(required)*")
	}
	body.WriteString("\n")
	body.WriteString(fmt.Sprintf("_Type: %s_", prop.Type))
	if prop.Description != "" {
		body.WriteString("\n\n")
		body.WriteString(prop.Description)
	}
	if len(prop.Enum) > 0 {
		body.WriteString("\n\nSelect one:")
		for i, v := range prop.Enum {
			body.WriteString(fmt.Sprintf("\n%d. %s", i+1, v))
		}
	} else if prop.Type == "boolean" {
		body.WriteString("\n\nChoose Yes or No.")
	} else {
		body.WriteString("\n\n_Reply with your answer, or use the buttons below._")
	}

	choices := elicFieldChoices(prop)
	summary := fmt.Sprintf("%s: %s", pe.serverName, label)
	b.callPrompt(pe.requestID, body.String(), summary, choices)
}

// elicFieldChoices builds the button set for the current field. Enum →
// one button per value; boolean → Yes/No; free-text fields get only a
// Cancel/Decline row (the user replies with text).
func elicFieldChoices(prop elicProperty) []delegator.PromptChoice {
	choices := make([]delegator.PromptChoice, 0, len(prop.Enum)+3)
	switch {
	case len(prop.Enum) > 0:
		for i, v := range prop.Enum {
			_ = v
			choices = append(choices, delegator.PromptChoice{
				Label: prop.Enum[i],
				Data:  "elic:enum:" + strconv.Itoa(i),
			})
		}
	case prop.Type == "boolean":
		choices = append(choices,
			delegator.PromptChoice{Label: "Yes", Data: "elic:bool:true"},
			delegator.PromptChoice{Label: "No", Data: "elic:bool:false"},
		)
	}
	choices = append(choices,
		delegator.PromptChoice{Label: "Decline", Data: "elic:decline"},
		delegator.PromptChoice{Label: "Cancel", Data: "elic:cancel"},
	)
	return choices
}

// callPrompt forwards to permPromptFn, logging a warning when the callback
// is nil so the prompt isn't silently dropped. Matches permissions.go's
// warning style for parity.
func (b *Backend) callPrompt(requestID, text, summary string, choices []delegator.PromptChoice) {
	if b.permPromptFn != nil {
		b.permPromptFn(requestID, text, summary, "", choices)
		return
	}
	log.Warnf("ccstream/elic", "permPromptFn nil for elicitation req_id=%s, prompt not displayed", requestID)
}

// ---------------------------------------------------------------------------
// Response path
// ---------------------------------------------------------------------------

// RespondToElicitation processes one user action on a pending elicitation.
// choice is one of:
//   - "elic:accept"          URL-mode Done button
//   - "elic:decline"         user declines
//   - "elic:cancel"          user cancels
//   - "elic:enum:<i>"        enum button click for current form field
//   - "elic:bool:true|false" boolean button for current form field
//   - any other string       free-text answer for current form field
//
// For form mode, the method advances through fieldOrder on each call, only
// sending the control_response after all fields have been satisfied (or
// when the user declines/cancels mid-walk).
func (b *Backend) RespondToElicitation(requestID, choice string) error {
	pe := b.getPendingElicit(requestID)
	if pe == nil {
		return fmt.Errorf("ccstream: no pending elicitation with request ID %q", requestID)
	}

	switch choice {
	case "elic:decline":
		return b.finishElicitation(pe, "decline")
	case "elic:cancel":
		return b.finishElicitation(pe, "cancel")
	case "elic:accept":
		// Only meaningful in URL mode (or in form mode with no schema, where
		// "accept" means accept-empty). For a walking form we'd never reach
		// this branch because the user is answering per-field.
		return b.finishElicitation(pe, "accept")
	}

	// Form-mode field responses must have a schema and an in-bounds cursor.
	if pe.schema == nil || pe.currentField >= len(pe.fieldOrder) {
		return fmt.Errorf("ccstream: elicitation %q has no field to answer", requestID)
	}

	name := pe.fieldOrder[pe.currentField]
	prop := pe.schema.Properties[name]

	value, err := coerceElicAnswer(prop, choice)
	if err != nil {
		return fmt.Errorf("ccstream: elicitation field %q: %w", name, err)
	}
	pe.answers[name] = value
	pe.currentField++

	log.Debugf("ccstream/elic", "field %d/%d answered for req_id=%s: %s=%v",
		pe.currentField, len(pe.fieldOrder), requestID, name, value)

	if pe.currentField < len(pe.fieldOrder) {
		b.presentElicitationField(pe, false)
		return nil
	}
	return b.finishElicitation(pe, "accept")
}

// coerceElicAnswer converts a user-supplied choice string into a value of the
// property's declared type. Enum and boolean inputs come from button data
// ("elic:enum:<i>", "elic:bool:true|false"); string/number/integer values
// come from free-text and must parse cleanly.
func coerceElicAnswer(prop elicProperty, choice string) (interface{}, error) {
	// Enum buttons — map index back to the string value.
	if strings.HasPrefix(choice, "elic:enum:") {
		idxStr := strings.TrimPrefix(choice, "elic:enum:")
		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx < 0 || idx >= len(prop.Enum) {
			return nil, fmt.Errorf("invalid enum index %q", idxStr)
		}
		return prop.Enum[idx], nil
	}
	// Boolean buttons.
	if strings.HasPrefix(choice, "elic:bool:") {
		switch strings.TrimPrefix(choice, "elic:bool:") {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return nil, fmt.Errorf("invalid bool value %q", choice)
		}
	}

	// Free text — coerce to the declared type.
	trimmed := strings.TrimSpace(choice)
	switch prop.Type {
	case "string":
		return trimmed, nil
	case "boolean":
		switch strings.ToLower(trimmed) {
		case "true", "yes", "y", "1":
			return true, nil
		case "false", "no", "n", "0":
			return false, nil
		default:
			return nil, fmt.Errorf("cannot parse %q as boolean", trimmed)
		}
	case "integer":
		n, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cannot parse %q as integer", trimmed)
		}
		return n, nil
	case "number":
		f, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return nil, fmt.Errorf("cannot parse %q as number", trimmed)
		}
		return f, nil
	}
	return nil, fmt.Errorf("unsupported property type %q", prop.Type)
}

// finishElicitation sends the final control_response for an elicitation and
// removes the pending state. accept carries the accumulated answers as a
// content object; decline/cancel send no content.
func (b *Backend) finishElicitation(pe *pendingElicitation, action string) error {
	resp := &ElicitationResponsePayload{Action: action}
	if action == "accept" && len(pe.answers) > 0 {
		raw, err := json.Marshal(pe.answers)
		if err != nil {
			return fmt.Errorf("ccstream: marshal elicitation content: %w", err)
		}
		resp.Content = raw
	}

	b.removePendingElicit(pe.requestID)

	if err := b.writer.SendControlResponse(pe.requestID, resp); err != nil {
		return err
	}

	b.outstanding.Resolve(pe.requestID)
	return nil
}

// ---------------------------------------------------------------------------
// elicitation_complete (URL-mode external completion)
// ---------------------------------------------------------------------------

// OnElicitationComplete is called from the system-message dispatcher when CC
// emits a `system/elicitation_complete` notification. It matches the
// notification to an in-flight URL-mode elicitation by elicitationID and
// auto-resolves it as accept. No-op if no match is found (the user may have
// already clicked Done, or the notification belongs to a previous session).
func (b *Backend) OnElicitationComplete(msg *ElicitationCompleteMessage) {
	b.touchActivity()
	pe := b.findPendingElicitByCompletionID(msg.McpServerName, msg.ElicitationID)
	if pe == nil {
		log.Debugf("ccstream/elic", "elicitation_complete for unknown id=%s server=%s (already resolved?)",
			msg.ElicitationID, msg.McpServerName)
		return
	}
	log.Debugf("ccstream/elic", "elicitation_complete auto-resolving req_id=%s id=%s",
		pe.requestID, msg.ElicitationID)
	if err := b.finishElicitation(pe, "accept"); err != nil {
		log.Warnf("ccstream/elic", "auto-accept on elicitation_complete failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Text intercept (platform path)
// ---------------------------------------------------------------------------

// HasPendingElicitation returns the request ID of an in-flight elicitation
// currently awaiting a free-text field answer, or "" if none. Used by the
// agent layer to intercept typed messages as form input instead of routing
// them to CC as a new turn — mirrors HasPendingQuestion.
func (b *Backend) HasPendingElicitation() string {
	b.elicMu.Lock()
	defer b.elicMu.Unlock()
	for _, pe := range b.pendingElicits {
		if pe.schema == nil {
			continue
		}
		if pe.currentField >= len(pe.fieldOrder) {
			continue
		}
		name := pe.fieldOrder[pe.currentField]
		prop := pe.schema.Properties[name]
		// Only free-text fields should intercept typed input; enum/boolean
		// fields are answered by buttons.
		if len(prop.Enum) > 0 || prop.Type == "boolean" {
			continue
		}
		return pe.requestID
	}
	return ""
}

// ---------------------------------------------------------------------------
// State helpers (all under elicMu)
// ---------------------------------------------------------------------------

func (b *Backend) storePendingElicit(pe *pendingElicitation) {
	b.elicMu.Lock()
	b.pendingElicits[pe.requestID] = pe
	b.elicMu.Unlock()
}

func (b *Backend) getPendingElicit(requestID string) *pendingElicitation {
	b.elicMu.Lock()
	pe := b.pendingElicits[requestID]
	b.elicMu.Unlock()
	return pe
}

// removePendingElicit removes and returns a pending elicitation. The
// "all-clear" signal is fired by OutstandingRegistry's onEmpty hook, not
// inferred locally — both perms and elicitations live in one registry, so
// the registry's view of "empty" is the only correct one.
func (b *Backend) removePendingElicit(requestID string) (pe *pendingElicitation, found bool) {
	b.elicMu.Lock()
	pe, found = b.pendingElicits[requestID]
	delete(b.pendingElicits, requestID)
	b.elicMu.Unlock()
	return
}

// findPendingElicitByCompletionID locates a URL-mode elicitation matching
// both server name and elicitation_id. Returns nil if none is found.
func (b *Backend) findPendingElicitByCompletionID(serverName, elicitationID string) *pendingElicitation {
	b.elicMu.Lock()
	defer b.elicMu.Unlock()
	for _, pe := range b.pendingElicits {
		if pe.mode != "url" {
			continue
		}
		if pe.elicitationID == elicitationID && pe.serverName == serverName {
			return pe
		}
	}
	return nil
}

// PendingElicitations returns the count of in-flight elicitations.
// Exposed for diagnostics and tests.
func (b *Backend) PendingElicitations() int {
	b.elicMu.Lock()
	defer b.elicMu.Unlock()
	return len(b.pendingElicits)
}

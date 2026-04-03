// Package ccstream implements a client for Claude Code's stream-json NDJSON
// protocol (--input-format stream-json --output-format stream-json).
//
// This file defines the typed Go structs for every message that flows over
// stdin (foci → CC) and stdout (CC → foci). Each line on the wire is a
// single JSON object; the "type" field (and optionally "subtype") discriminates
// the message kind.
package ccstream

import "encoding/json"

// ---------------------------------------------------------------------------
// Message priority levels
// ---------------------------------------------------------------------------

// Priority levels for user messages. CC processes messages in priority order:
// PriorityNow interrupts the current operation (aborts tool execution),
// PriorityNext queues after the current turn (default for user messages),
// PriorityLater defers (used for task notifications / low-priority system messages).
const (
	PriorityNow   = "now"
	PriorityNext  = "next"
	PriorityLater = "later"
)

// ---------------------------------------------------------------------------
// Shared / reusable types
// ---------------------------------------------------------------------------

// ContentBlock represents a block inside an assistant message's content array.
// The same struct covers text, thinking, tool_use, and tool_result blocks;
// unused fields are omitted from JSON.
type ContentBlock struct {
	Type     string          `json:"type"`               // "text"|"thinking"|"tool_use"|"tool_result"
	Text     string          `json:"text,omitempty"`      // text block content
	Thinking string          `json:"thinking,omitempty"`  // thinking block content
	ID       string          `json:"id,omitempty"`        // tool_use id
	Name     string          `json:"name,omitempty"`      // tool_use name
	Input    json.RawMessage `json:"input,omitempty"`     // tool_use input (arbitrary JSON)
	Content  json.RawMessage `json:"content,omitempty"`   // tool_result content
	IsError  *bool           `json:"is_error,omitempty"`  // tool_result error flag
	ToolID   string          `json:"tool_use_id,omitempty"` // tool_result back-reference
}

// TokenUsage holds token counts for a single API call or accumulated turn.
type TokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// ModelUsage holds per-model token and cost accounting in a ResultMessage.
type ModelUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
	ContextWindow            int     `json:"contextWindow"`
	MaxOutputTokens          int     `json:"maxOutputTokens"`
}

// PermSuggestion is a permission rule suggestion attached to a permission
// request or included in a permission allow response.
type PermSuggestion struct {
	Prefix string `json:"prefix"`
	Scope  string `json:"scope"`
}

// ---------------------------------------------------------------------------
// Stdin messages (foci → CC)
// ---------------------------------------------------------------------------

// UserMessage sends a conversational turn to Claude Code.
type UserMessage struct {
	Type            string       `json:"type"`                          // always "user"
	Message         UserPayload  `json:"message"`
	ParentToolUseID *string      `json:"parent_tool_use_id,omitempty"` // nil for top-level turns
	SessionID       string       `json:"session_id,omitempty"`
	UUID            string       `json:"uuid,omitempty"`
	IsSynthetic     *bool        `json:"isSynthetic,omitempty"`
	Priority        string       `json:"priority,omitempty"`           // "now"|"next"|"later"
	Timestamp       string       `json:"timestamp,omitempty"`
}

// UserPayload is the inner message object of a UserMessage.
// Content can be a plain string or an array of ContentBlocks; use
// ContentString for the simple case and ContentBlocks for multi-part.
type UserPayload struct {
	Role          string          `json:"role"` // always "user"
	ContentString string          `json:"-"`    // set for simple string content
	ContentBlocks []ContentBlock  `json:"-"`    // set for structured content
}

// MarshalJSON encodes UserPayload, emitting content as either a string or
// an array depending on which field is populated.
func (p UserPayload) MarshalJSON() ([]byte, error) {
	type alias struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	a := alias{Role: p.Role}
	if len(p.ContentBlocks) > 0 {
		a.Content = p.ContentBlocks
	} else {
		a.Content = p.ContentString
	}
	return json.Marshal(a)
}

// UnmarshalJSON decodes UserPayload, accepting content as either a string
// or an array of ContentBlocks.
func (p *UserPayload) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.Role = raw.Role
	if len(raw.Content) > 0 && raw.Content[0] == '"' {
		return json.Unmarshal(raw.Content, &p.ContentString)
	}
	return json.Unmarshal(raw.Content, &p.ContentBlocks)
}

// ControlRequest sends a control command to Claude Code (foci-initiated).
type ControlRequest struct {
	Type      string          `json:"type"`       // always "control_request"
	RequestID string          `json:"request_id"` // UUID
	Request   json.RawMessage `json:"request"`    // subtype-specific payload
}

// ControlResponse answers a control request that originated from CC
// (e.g. responding to a permission prompt).
type ControlResponse struct {
	Type     string                 `json:"type"`     // always "control_response"
	Response ControlResponsePayload `json:"response"`
}

// ControlResponsePayload is the inner object of a ControlResponse.
type ControlResponsePayload struct {
	Subtype   string `json:"subtype"`    // always "success"
	RequestID string `json:"request_id"`
	Response  any    `json:"response"`
}

// controlResponseInbound is the envelope for control_response messages
// received FROM CC (responses to our get_context_usage, initialize, etc.).
type controlResponseInbound struct {
	Type     string `json:"type"` // "control_response"
	Response struct {
		Subtype   string          `json:"subtype"`    // "success" or "error"
		RequestID string          `json:"request_id"`
		Response  json.RawMessage `json:"response"`   // subtype-specific payload
	} `json:"response"`
}

// contextUsagePayload is the inner response from a get_context_usage
// control request. We parse the fields foci cares about — CC also
// returns gridRows and other TUI data that we ignore.
type contextUsagePayload struct {
	TotalTokens          int                       `json:"totalTokens"`
	MaxTokens            int                       `json:"maxTokens"`
	Percentage           int                       `json:"percentage"`
	AutoCompactThreshold int                       `json:"autoCompactThreshold"`
	Model                string                    `json:"model"`
	Categories           []contextUsageCategoryRaw `json:"categories"`
}

type contextUsageCategoryRaw struct {
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
}

// ControlCancelRequest cancels a pending CC-originated control request.
type ControlCancelRequest struct {
	Type      string `json:"type"`       // always "control_cancel_request"
	RequestID string `json:"request_id"`
}

// KeepAlive prevents idle timeout on the stream.
type KeepAlive struct {
	Type string `json:"type"` // always "keep_alive"
}

// UpdateEnvironmentVariables injects environment variables into the CC process
// at runtime.
type UpdateEnvironmentVariables struct {
	Type      string            `json:"type"`      // always "update_environment_variables"
	Variables map[string]string `json:"variables"`
}

// ---------------------------------------------------------------------------
// Control request subtypes (payloads for ControlRequest.Request)
// ---------------------------------------------------------------------------

// InitializeRequest asks CC to (re-)initialize with a system prompt.
type InitializeRequest struct {
	Subtype             string `json:"subtype"`                        // always "initialize"
	SystemPrompt        string `json:"systemPrompt,omitempty"`
	AppendSystemPrompt  string `json:"appendSystemPrompt,omitempty"`
}

// GetContextUsageRequest asks CC for current context window usage.
type GetContextUsageRequest struct {
	Subtype string `json:"subtype"` // always "get_context_usage"
}

// InterruptRequest asks CC to interrupt the current turn.
type InterruptRequest struct {
	Subtype string `json:"subtype"` // always "interrupt"
}

// SetModelRequest asks CC to switch the active model.
type SetModelRequest struct {
	Subtype string `json:"subtype"` // always "set_model"
	Model   string `json:"model"`
}

// ---------------------------------------------------------------------------
// Permission response payloads (used inside ControlResponse.Response)
// ---------------------------------------------------------------------------

// PermissionAllow is the response payload that grants a tool permission.
type PermissionAllow struct {
	Behavior               string          `json:"behavior"`                         // always "allow"
	UpdatedInput           json.RawMessage `json:"updatedInput"`                     // {} for no change
	UpdatedPermissions     []PermSuggestion `json:"updatedPermissions,omitempty"`
	ToolUseID              string          `json:"toolUseID,omitempty"`
	DecisionClassification string          `json:"decisionClassification"`           // "user_temporary"|"user_permanent"|"user_reject"
}

// PermissionDeny is the response payload that denies a tool permission.
type PermissionDeny struct {
	Behavior               string `json:"behavior"`                // always "deny"
	Message                string `json:"message"`
	Interrupt              bool   `json:"interrupt"`
	ToolUseID              string `json:"toolUseID,omitempty"`
	DecisionClassification string `json:"decisionClassification"` // "user_temporary"|"user_permanent"|"user_reject"
}

// ---------------------------------------------------------------------------
// Stdout messages (CC → foci)
// ---------------------------------------------------------------------------

// StdoutEnvelope carries just the discriminator fields, used for initial
// deserialization before dispatching to the concrete type.
type StdoutEnvelope struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
}

// AssistantMessage is a model response from Claude Code.
type AssistantMessage struct {
	Type            string       `json:"type"`                          // always "assistant"
	Message         BetaMessage  `json:"message"`
	ParentToolUseID *string      `json:"parent_tool_use_id,omitempty"`
	Error           *string      `json:"error,omitempty"`               // "rate_limit", "authentication_failed", etc.
	UUID            string       `json:"uuid,omitempty"`
	SessionID       string       `json:"session_id,omitempty"`
}

// BetaMessage is the Anthropic API message object embedded in an
// AssistantMessage.
type BetaMessage struct {
	ID         string         `json:"id"`
	Role       string         `json:"role"`       // "assistant"
	Content    []ContentBlock `json:"content"`
	Model      string         `json:"model"`
	StopReason *string        `json:"stop_reason,omitempty"`
	Usage      TokenUsage     `json:"usage"`
}

// ResultMessage signals turn completion and carries accumulated metrics.
type ResultMessage struct {
	Type                string                  `json:"type"`          // always "result"
	Subtype             string                  `json:"subtype"`       // "success"|"error_during_execution"|"error_max_turns"|"error_max_budget_usd"|"error_max_structured_output_retries"
	IsError             bool                    `json:"is_error"`
	DurationMS          int                     `json:"duration_ms"`
	DurationAPIMS       int                     `json:"duration_api_ms"`
	NumTurns            int                     `json:"num_turns"`
	Result              string                  `json:"result"`        // final text output
	StopReason          *string                 `json:"stop_reason,omitempty"`
	TotalCostUSD        float64                 `json:"total_cost_usd"`
	Usage               TokenUsage              `json:"usage"`
	ModelUsage          map[string]ModelUsage   `json:"modelUsage,omitempty"`
	Errors              []string                `json:"errors,omitempty"`
	PermissionDenials   []json.RawMessage       `json:"permission_denials,omitempty"`
	UUID                string                  `json:"uuid,omitempty"`
	SessionID           string                  `json:"session_id,omitempty"`
}

// InitMessage is the first message CC emits after startup (system/init).
type InitMessage struct {
	Type             string   `json:"type"`              // "system"
	Subtype          string   `json:"subtype"`           // "init"
	ClaudeCodeVersion string  `json:"claude_code_version"`
	CWD              string   `json:"cwd"`
	Model            string   `json:"model"`
	PermissionMode   string   `json:"permissionMode"`
	Tools            []string `json:"tools"`
	SessionID        string   `json:"session_id,omitempty"`
	UUID             string   `json:"uuid,omitempty"`
}

// StatusMessage is a system/status heartbeat (e.g. compaction in progress).
type StatusMessage struct {
	Type    string  `json:"type"`              // "system"
	Subtype string  `json:"subtype"`           // "status"
	Status  *string `json:"status,omitempty"`  // "compacting" or null
}

// CompactBoundaryMessage marks the boundary of a compaction event.
type CompactBoundaryMessage struct {
	Type            string          `json:"type"`             // "system"
	Subtype         string          `json:"subtype"`          // "compact_boundary"
	CompactMetadata CompactMetadata `json:"compact_metadata"`
}

// CompactMetadata carries details about a compaction event.
type CompactMetadata struct {
	Trigger          string            `json:"trigger"`
	PreTokens        int               `json:"pre_tokens"`
	PreservedSegment *PreservedSegment `json:"preserved_segment,omitempty"`
}

// PreservedSegment identifies the message range preserved through compaction.
type PreservedSegment struct {
	HeadUUID   string `json:"head_uuid"`
	AnchorUUID string `json:"anchor_uuid"`
	TailUUID   string `json:"tail_uuid"`
}

// SessionStateMessage signals a change in the agent's operational state.
type SessionStateMessage struct {
	Type    string `json:"type"`    // "system"
	Subtype string `json:"subtype"` // "session_state_changed"
	State   string `json:"state"`   // "idle"|"running"|"requires_action"
}

// APIRetryMessage indicates an API call is being retried after a failure.
type APIRetryMessage struct {
	Type         string `json:"type"`           // "system"
	Subtype      string `json:"subtype"`        // "api_retry"
	Attempt      int    `json:"attempt"`
	MaxRetries   int    `json:"max_retries"`
	RetryDelayMS int    `json:"retry_delay_ms"`
	ErrorStatus  int    `json:"error_status"`
	Error        string `json:"error"`
}

// PermissionRequest is a control_request from CC asking foci to approve a
// tool invocation.
type PermissionRequest struct {
	Type      string                   `json:"type"`       // "control_request"
	RequestID string                   `json:"request_id"`
	Request   PermissionRequestPayload `json:"request"`
}

// PermissionRequestPayload is the inner request object of a PermissionRequest.
type PermissionRequestPayload struct {
	Subtype               string          `json:"subtype"`                          // "can_use_tool"
	ToolName              string          `json:"tool_name"`
	Input                 json.RawMessage `json:"input"`
	ToolUseID             string          `json:"tool_use_id"`
	PermissionSuggestions []PermSuggestion `json:"permission_suggestions,omitempty"`
	DecisionReason        string          `json:"decision_reason,omitempty"`
	AgentID               *string         `json:"agent_id,omitempty"`
	Title                 string          `json:"title,omitempty"`
	DisplayName           string          `json:"display_name,omitempty"`
	Description           string          `json:"description,omitempty"`
}

// ToolProgressMessage is a heartbeat emitted during long-running tool
// execution.
type ToolProgressMessage struct {
	Type               string `json:"type"`                 // "tool_progress"
	ToolUseID          string `json:"tool_use_id"`
	ToolName           string `json:"tool_name"`
	ElapsedTimeSeconds int    `json:"elapsed_time_seconds"`
}

// TaskEvent tracks agent/background task lifecycle (system/task_*).
type TaskEvent struct {
	Type        string `json:"type"`                   // "system"
	Subtype     string `json:"subtype"`                // "task_started"|"task_progress"|"task_notification"
	TaskID      string `json:"task_id,omitempty"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status,omitempty"`
	Summary     string `json:"summary,omitempty"`
}

// StreamEvent wraps a raw Anthropic streaming event, emitted when CC is
// started with --include-partial-messages.
type StreamEvent struct {
	Type            string          `json:"type"`                          // "stream_event"
	Event           json.RawMessage `json:"event"`
	ParentToolUseID *string         `json:"parent_tool_use_id,omitempty"`
}

// ---------------------------------------------------------------------------
// Helper constructors
// ---------------------------------------------------------------------------

// NewUserMessage creates a simple text UserMessage (top-level, no parent).
func NewUserMessage(content string) *UserMessage {
	return &UserMessage{
		Type: "user",
		Message: UserPayload{
			Role:          "user",
			ContentString: content,
		},
	}
}

// NewUserMessageBlocks creates a UserMessage with structured content blocks.
func NewUserMessageBlocks(blocks []ContentBlock) *UserMessage {
	return &UserMessage{
		Type: "user",
		Message: UserPayload{
			Role:          "user",
			ContentBlocks: blocks,
		},
	}
}

// NewControlResponse creates a ControlResponse for the given request ID.
func NewControlResponse(reqID string, response any) *ControlResponse {
	return &ControlResponse{
		Type: "control_response",
		Response: ControlResponsePayload{
			Subtype:   "success",
			RequestID: reqID,
			Response:  response,
		},
	}
}

// NewControlCancelRequest creates a ControlCancelRequest for the given
// request ID.
func NewControlCancelRequest(reqID string) *ControlCancelRequest {
	return &ControlCancelRequest{
		Type:      "control_cancel_request",
		RequestID: reqID,
	}
}

// NewUpdateEnvironmentVariables creates an UpdateEnvironmentVariables message.
func NewUpdateEnvironmentVariables(vars map[string]string) *UpdateEnvironmentVariables {
	return &UpdateEnvironmentVariables{
		Type:      "update_environment_variables",
		Variables: vars,
	}
}


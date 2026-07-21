package codex

import (
	"encoding/json"
	"os"
)

// JSON-RPC 2.0 message types (without the "jsonrpc":"2.0" header, as per
// the Codex app-server wire format — one JSON object per line over stdio).

// rpcRequest is a client-to-server request.
type rpcRequest struct {
	Method string      `json:"method"`
	ID     int64       `json:"id"`
	Params interface{} `json:"params,omitempty"`
}

// rpcNotification is a message with no ID (either direction).
type rpcNotification struct {
	Method string      `json:"method"`
	Params interface{} `json:"params,omitempty"`
}

// rpcResponse is a server-to-client response to a prior request.
type rpcResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcReply is what sendAndWait receives on a pending-RPC channel: the result
// payload on success, or a non-nil err carrying the server's JSON-RPC error
// (previously discarded, so every server-side error masqueraded as "process
// exited").
type rpcReply struct {
	result json.RawMessage
	err    error
}

// --- Wire envelope (for parsing incoming lines) ---

// wireEnvelope is used to discriminate incoming messages: requests have
// method + id, notifications have method only, responses have id only.
type wireEnvelope struct {
	Method string `json:"method,omitempty"`
	ID     *int64 `json:"id,omitempty"`
}

// --- Initialize ---

type initializeParams struct {
	ClientInfo clientInfo `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

// --- Thread ---

type threadStartParams struct {
	Model            string         `json:"model,omitempty"`
	Cwd              string         `json:"cwd,omitempty"`
	Sandbox          string         `json:"sandbox,omitempty"`
	BaseInstructions string         `json:"baseInstructions,omitempty"`
	Config           map[string]any `json:"config,omitempty"`
}

type threadResumeParams struct {
	ThreadID string `json:"threadId"`
}

type threadResult struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
	Model string `json:"model,omitempty"`
}

// --- Models ---

type modelListParams struct {
	Cursor        string `json:"cursor,omitempty"`
	IncludeHidden bool   `json:"includeHidden"`
}

type modelListResponse struct {
	Data       []codexModel `json:"data"`
	NextCursor *string      `json:"nextCursor"`
}

type codexModel struct {
	ID                        string                  `json:"id"`
	Model                     string                  `json:"model"`
	SupportedReasoningEfforts []reasoningEffortOption `json:"supportedReasoningEfforts"`
}

type reasoningEffortOption struct {
	ReasoningEffort string `json:"reasoningEffort"`
}

// --- Turn ---

type turnStartParams struct {
	ThreadID       string         `json:"threadId"`
	Input          []turnInput    `json:"input"`
	Cwd            string         `json:"cwd,omitempty"`
	Model          string         `json:"model,omitempty"`
	Effort         string         `json:"effort,omitempty"`
	ApprovalPolicy string         `json:"approvalPolicy,omitempty"`
	SandboxPolicy  *sandboxPolicy `json:"sandboxPolicy,omitempty"`
}

type turnInput struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// turnSteerParams is the turn/steer request payload. expectedTurnId is a
// REQUIRED precondition (verified live against codex app-server 0.144.5 via
// --strict-config/generate-json-schema and a live app-server probe): the
// request is rejected with "no active turn to steer" if it doesn't match
// the thread's currently active turn (e.g. the turn already completed
// before the steer landed). Omitting the field entirely (the prior
// implementation) is rejected outright: "Invalid request: missing field
// `expectedTurnId`" — so unqualified turn/steer calls never succeeded.
type turnSteerParams struct {
	ThreadID       string      `json:"threadId"`
	ExpectedTurnID string      `json:"expectedTurnId"`
	Input          []turnInput `json:"input"`
}

type sandboxPolicy struct {
	Type          string   `json:"type"` // "workspace-write", "read-only", "danger-full-access"
	WritableRoots []string `json:"writableRoots,omitempty"`
	NetworkAccess bool     `json:"networkAccess,omitempty"`
}

// --- Notifications (server → client) ---

// turnStartedParams carries the turn ID at turn start.
type turnStartedParams struct {
	Turn struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"turn"`
}

// turnCompletedParams carries the final turn status.
type turnCompletedParams struct {
	ThreadID string `json:"threadId"`
	Turn     struct {
		ID     string `json:"id"`
		Status string `json:"status"` // "completed", "interrupted", "failed"
		Error  *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	} `json:"turn"`
}

// itemStartedParams signals an item (command, message, etc.) began.
type itemStartedParams struct {
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId,omitempty"`
	Item     json.RawMessage `json:"item"`
}

// itemCompletedParams signals an item finished.
type itemCompletedParams struct {
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId,omitempty"`
	Item     json.RawMessage `json:"item"`
}

// agentMessageDeltaParams carries a streaming text delta.
type agentMessageDeltaParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId,omitempty"`
	Delta    string `json:"delta"`
}

// tokenUsageParams carries token usage updates for the active thread.
// Emitted as thread/tokenUsage/updated.
type tokenUsageParams struct {
	ThreadID   string `json:"threadId"`
	TurnID     string `json:"turnId,omitempty"`
	TokenUsage struct {
		Last struct {
			InputTokens           int `json:"inputTokens"`
			OutputTokens          int `json:"outputTokens"`
			CachedInputTokens     int `json:"cachedInputTokens"`
			ReasoningOutputTokens int `json:"reasoningOutputTokens"`
			TotalTokens           int `json:"totalTokens"`
		} `json:"last"`
		ModelContextWindow int `json:"modelContextWindow,omitempty"`
	} `json:"tokenUsage"`
}

// serverRequestResolvedParams confirms a pending approval was answered.
// Emitted as serverRequest/resolved.
type serverRequestResolvedParams struct {
	ThreadID  string `json:"threadId"`
	RequestID any    `json:"requestId"`
}

// reasoningDeltaParams carries a streaming reasoning text delta.
// Emitted as item/reasoning/textDelta.
type reasoningDeltaParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId,omitempty"`
	ItemID   string `json:"itemId,omitempty"`
	Delta    string `json:"delta"`
}

// reasoningSummaryDeltaParams carries a streaming reasoning summary delta.
// Emitted as item/reasoning/summaryTextDelta.
type reasoningSummaryDeltaParams struct {
	ThreadID     string `json:"threadId"`
	TurnID       string `json:"turnId,omitempty"`
	ItemID       string `json:"itemId,omitempty"`
	Delta        string `json:"delta"`
	SummaryIndex int    `json:"summaryIndex,omitempty"`
}

// threadNameUpdatedParams carries an auto-generated thread name.
// Emitted as thread/name/updated after Codex generates a summary title.
type threadNameUpdatedParams struct {
	ThreadID   string  `json:"threadId"`
	ThreadName *string `json:"threadName"`
}

// compactStartParams is the params for thread/compact/start.
type compactStartParams struct {
	ThreadID string `json:"threadId"`
}

// configWarningParams carries a recoverable configuration or initialization
// problem. Emitted as the configWarning notification.
type configWarningParams struct {
	Summary string `json:"summary"`
	Details string `json:"details,omitempty"`
	Path    string `json:"path,omitempty"`
}

// runtimeWarningParams carries a non-fatal runtime warning.
// Emitted as the warning notification.
type runtimeWarningParams struct {
	ThreadID string `json:"threadId,omitempty"`
	Message  string `json:"message"`
}

// --- Approval requests (server-initiated) ---

// commandApprovalParams is the payload of item/commandExecution/requestApproval.
type commandApprovalParams struct {
	ItemID   string `json:"itemId"`
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Reason   string `json:"reason,omitempty"`
	Command  string `json:"command,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
}

// fileChangeApprovalParams is the payload of item/fileChange/requestApproval.
type fileChangeApprovalParams struct {
	ItemID   string `json:"itemId"`
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Reason   string `json:"reason,omitempty"`
}

// --- Approval response (client → server) ---

type approvalResponse struct {
	Decision string `json:"decision"` // "accept", "decline", "acceptForSession", "cancel"
}

// --- Item type discrimination ---

type itemEnvelope struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status string `json:"status,omitempty"`
	Text   string `json:"text,omitempty"`
	// commandExecution fields
	Command string `json:"command,omitempty"`
	// fileChange fields
	Changes []fileChangeEntry `json:"changes,omitempty"`
	// mcpToolCall / dynamicToolCall fields
	Tool      string          `json:"tool,omitempty"`
	Namespace string          `json:"namespace,omitempty"`
	Server    string          `json:"server,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// webSearch fields
	Query string `json:"query,omitempty"`
	// subAgentActivity fields
	Kind          string `json:"kind,omitempty"`
	AgentPath     string `json:"agentPath,omitempty"`
	AgentThreadID string `json:"agentThreadId,omitempty"`
	// collabAgentToolCall fields
	Prompt       string                 `json:"prompt,omitempty"`
	AgentsStates map[string]collabState `json:"agentsStates,omitempty"`
	// agentMessage fields. Phase distinguishes mid-turn narration from the
	// terminal answer (live-verified against codex app-server 0.144.5's
	// generate-json-schema output AND a live turn: "commentary" precedes a
	// tool call, "final_answer" is the turn's actual response). The server's
	// own schema doc notes it isn't emitted by every model/provider, so a
	// missing/empty phase must be treated as "unknown" and accumulated as
	// before (backward compat), never dropped.
	Phase string `json:"phase,omitempty"`
}

// fileChangeEntry is one file change in a fileChange item's changes array.
type fileChangeEntry struct {
	Path string `json:"path,omitempty"`
	Kind string `json:"kind,omitempty"` // "create", "modify", "delete"
}

// collabState is one entry in a collabAgentToolCall's agentsStates map.
type collabState struct {
	Status  string `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}

// userHomeDir wraps os.UserHomeDir for testability.
func userHomeDir() (string, error) {
	return os.UserHomeDir()
}

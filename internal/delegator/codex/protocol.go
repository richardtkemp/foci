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
	Model             string         `json:"model,omitempty"`
	Cwd               string         `json:"cwd,omitempty"`
	Sandbox           string         `json:"sandbox,omitempty"`
	BaseInstructions  string         `json:"baseInstructions,omitempty"`
	Config            map[string]any `json:"config,omitempty"`
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

// --- Turn ---

type turnStartParams struct {
	ThreadID       string         `json:"threadId"`
	Input          []turnInput    `json:"input"`
	Cwd            string         `json:"cwd,omitempty"`
	Model          string         `json:"model,omitempty"`
	ApprovalPolicy string         `json:"approvalPolicy,omitempty"`
	SandboxPolicy  *sandboxPolicy `json:"sandboxPolicy,omitempty"`
}

type turnInput struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
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
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId,omitempty"`
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
	ItemID            string `json:"itemId"`
	ThreadID          string `json:"threadId"`
	TurnID            string `json:"turnId"`
	Reason            string `json:"reason,omitempty"`
	Command           string `json:"command,omitempty"`
	Cwd               string `json:"cwd,omitempty"`
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
	Changes json.RawMessage `json:"changes,omitempty"`
}

// userHomeDir wraps os.UserHomeDir for testability.
func userHomeDir() (string, error) {
	return os.UserHomeDir()
}

package delegator

import "context"

// ControlRequest is a marker interface for backend-agnostic control intents.
// The Agent layer constructs these; each backend translates them to its own
// wire format in SendControl. The unexported method prevents arbitrary types
// from satisfying the interface.
type ControlRequest interface {
	controlRequest() // marker — compile-time safety
}

// SetModelRequest asks the backend to switch the active model.
// The Model field is the raw user input (e.g. "opus", "sonnet") — backends
// translate to their own format as needed.
type SetModelRequest struct {
	Model string
}

func (*SetModelRequest) controlRequest() {}

// ModelResolution is a backend's canonical interpretation of a user-facing
// model name or alias. BackendModel is sent over the backend protocol; Model
// is the developer-qualified id foci persists and displays.
type ModelResolution struct {
	BackendModel string
	Model        string
}

// ModelResolver is optionally implemented by catalogue-backed delegators that
// can resolve aliases before a SetModelRequest is sent. Other backends keep
// receiving the raw model string as before.
type ModelResolver interface {
	ResolveModel(ctx context.Context, model string) (ModelResolution, error)
}

// ApplyFlagSettingsRequest asks the backend to merge settings into its runtime
// flag-settings layer mid-session. For ccstream this maps to CC's
// apply_flag_settings control; e.g. {"effortLevel": "max"} changes the effort
// applied to the next turn with no session bounce. Fire-and-forget — backends
// MUST NOT block on a control_response. CC does NOT validate the values, so the
// command layer must reject invalid settings before constructing this.
type ApplyFlagSettingsRequest struct {
	Settings map[string]any
}

func (*ApplyFlagSettingsRequest) controlRequest() {}

// SetPermissionModeRequest asks the backend to switch its permission mode
// mid-session. Mode is the backend-native value (for ccstream:
// "default" | "acceptEdits" | "plan" | "auto" | "bypassPermissions" |
// "dontAsk"). The command layer translates user-facing aliases ("normal",
// "accept") before constructing this. Fire-and-forget — backends MUST NOT
// block on a control_response.
type SetPermissionModeRequest struct {
	Mode string
}

func (*SetPermissionModeRequest) controlRequest() {}

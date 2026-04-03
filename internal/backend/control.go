package backend

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

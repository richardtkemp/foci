package ccstream

import (
	"context"
	"fmt"

	"foci/internal/backend"
)

// SendControl translates a backend-agnostic ControlRequest into the
// ccstream wire format and sends it to the CC subprocess.
func (b *Backend) SendControl(ctx context.Context, req backend.ControlRequest) error {
	switch r := req.(type) {
	case *backend.SetModelRequest:
		return b.writer.SendControl(newRequestID(), &SetModelRequest{
			Subtype: "set_model",
			Model:   r.Model,
		})
	default:
		return fmt.Errorf("ccstream: unsupported control request type %T", req)
	}
}

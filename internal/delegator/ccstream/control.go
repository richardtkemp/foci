package ccstream

import (
	"context"
	"fmt"

	"foci/internal/delegator"
)

// SendControl translates a backend-agnostic ControlRequest into the
// ccstream wire format and sends it to the CC subprocess.
func (b *Backend) SendControl(ctx context.Context, req delegator.ControlRequest) error {
	switch r := req.(type) {
	case *delegator.SetModelRequest:
		return b.writer.SendControl(newRequestID(), &SetModelRequest{
			Subtype: "set_model",
			Model:   r.Model,
		})
	case *delegator.SetPermissionModeRequest:
		return b.writer.SendControl(newRequestID(), &SetPermissionModeRequest{
			Subtype: "set_permission_mode",
			Mode:    r.Mode,
		})
	default:
		return fmt.Errorf("ccstream: unsupported control request type %T", req)
	}
}

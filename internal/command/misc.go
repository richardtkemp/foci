package command

import (
	"context"
	"fmt"
	"strings"
)

// MiscCommand groups miscellaneous operational toggles under /misc.
// Currently exposes pprof live-toggle as /misc pprof.
func MiscCommand() *Command {
	return &Command{
		Name:        "misc",
		Description: "Miscellaneous operations",
		Category:    "operations",
		DefaultExecute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
			return Response{Text: "Usage: /misc pprof [on|off]"}, nil
		},
		Subcommands: []Subcommand{
			{
				Name:        "pprof",
				Description: "Toggle or set the pprof profiling gate",
				Execute: func(_ context.Context, req Request, cc CommandContext) (Response, error) {
					if cc.PprofControl == nil {
						return Response{Text: "pprof is not available on this deployment."}, nil
					}
					arg := strings.ToLower(strings.TrimSpace(req.Args))
					var action, label string
					switch arg {
					case "", "toggle":
						action = "toggle"
						label = "toggled"
					case "on", "enable", "start":
						action = "on"
					case "off", "disable", "stop":
						action = "off"
					default:
						return Response{Text: "Usage: /misc pprof [on|off]"}, nil
					}
					enabled := cc.PprofControl(action)
					if label == "toggled" {
						return Response{Text: fmt.Sprintf("pprof %s — now %v", label, enabledState(enabled))}, nil
					}
					return Response{Text: fmt.Sprintf("pprof %s", enabledState(enabled))}, nil
				},
			},
		},
	}
}

func enabledState(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

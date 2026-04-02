package dispatch

import (
	"fmt"

	"foci/internal/command"
	"foci/internal/platform"
)

// CmdButtons converts command keyboard options to platform.ButtonChoice with
// callback data formatted as "/cmdName optData". Used by both telegram and
// discord for command keyboard rendering.
func CmdButtons(cmdName string, opts []command.KeyboardOption) []platform.ButtonChoice {
	btns := make([]platform.ButtonChoice, len(opts))
	for i, opt := range opts {
		btns[i] = platform.ButtonChoice{
			Label: opt.Label,
			Data:  fmt.Sprintf("/%s %s", cmdName, opt.Data),
			Row:   opt.Row,
		}
	}
	return btns
}

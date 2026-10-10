package dispatch

import (
	"fmt"

	"foci/internal/command"
	"foci/internal/platform"
	"foci/internal/question"
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

// WizardButtons builds the button set for a wizard step's structured question:
// one button per option (label = option label, data "<token>:<index>"),
// followed by a Cancel button ("<token>:cancel"). Platforms render them under
// the "wz:" callback prefix; the token comes from command.WizardStepToken, so
// the full callback data ("wz:" + 16 hex chars + ":cancel" — 26 bytes at
// most) stays well inside Telegram's 64-byte callback_data limit.
func WizardButtons(q *question.Question) []platform.ButtonChoice {
	token := command.WizardStepToken(q)
	btns := make([]platform.ButtonChoice, 0, len(q.Options)+1)
	for i, opt := range q.Options {
		btns = append(btns, platform.ButtonChoice{
			Label: opt.Label,
			Data:  fmt.Sprintf("%s:%d", token, i),
		})
	}
	btns = append(btns, platform.ButtonChoice{Label: "Cancel", Data: token + ":" + wizardCancelChoice})
	return btns
}

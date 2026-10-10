package discord

// Channel-side rendering of structured wizard steps (#861): when the active
// wizard's current step is structured (WizardStepProvider with options), the
// wizard's prompt goes out with one button per option plus Cancel, under the
// "wz:" callback prefix. A press resolves through Registry.HandleWizardChoice,
// which validates the step token under the registry's lock — so a stale
// button (double press, press after the step was answered by typing, press
// after the wizard ended) never feeds the wizard.

import (
	"foci/internal/dispatch"
	"foci/internal/platform"
	"foci/internal/question"

	"github.com/bwmarrin/discordgo"
)

// sendWizardStep sends a wizard reply to chatID's channel: with wz: buttons
// (one per option, then Cancel) when step is a structured step, plain text
// otherwise. If the button send fails (e.g. Discord's 25-button limit on a
// long option list), the plain text is sent instead so the prompt is never
// lost.
func (b *Bot) sendWizardStep(chatID int64, text string, step *question.Question) {
	if step != nil && len(step.Options) > 0 {
		_, err := b.SendTextWithButtonsToChat(chatID, text, dispatch.WizardButtons(step), "wz:")
		if err == nil {
			return
		}
		b.logger().Warnf("wizard step buttons failed, sending plain text: %s", b.sanitizeError(err))
	}
	_ = b.SendTextToChat(chatID, text)
}

// handleWizardCallback resolves a wz: button press on a wizard step message.
// The pressed message's buttons always come off first. A valid press sends the
// wizard's reply with the same rules as any step (buttons when the NEXT step
// is structured, doc after); anything else — no wizard machinery wired, stale
// token, bad index, malformed payload — answers "This step is no longer
// active." and nothing reaches the wizard or the agent.
func (b *Bot) handleWizardCallback(channelID, msgID, data string, chatID int64) {
	var resp, docPath string
	handled := false
	var step *question.Question
	if b.dispatcher != nil && b.commands != nil {
		scope := b.dispatcher.SessionKeyForChat(chatID)
		if token, choice, ok := dispatch.ParseWizardCallback(data); ok {
			resp, docPath, handled = b.commands.HandleWizardChoice(scope, token, choice)
			if handled {
				step = b.commands.WizardPendingStep(scope)
			}
		}
	}

	b.stripMessageButtons(channelID, msgID)
	if !handled {
		_ = b.SendTextToChat(chatID, "This step is no longer active.")
		return
	}
	b.sendWizardStep(chatID, resp, step)
	_ = platform.SendDocAndRemove(b, chatID, docPath, "")
}

// stripMessageButtons removes the buttons from a message, leaving its text
// untouched (an edit with empty components — Discord has no dedicated
// "remove components" call).
func (b *Bot) stripMessageButtons(channelID, msgID string) {
	noComponents := []discordgo.MessageComponent{}
	_, _ = b.api.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    channelID,
		ID:         msgID,
		Components: &noComponents,
	})
}

package dispatch

import (
	"foci/internal/command"
	"foci/internal/question"
)

// CommandOutcome describes the result of a full command dispatch pipeline
// (normalize → keyboard lookup → chain keyboard → dispatch). Exactly one
// field is set. The platform renders the outcome using platform-native sends.
type CommandOutcome struct {
	// Keyboard: bare command had keyboard options — show buttons.
	Keyboard *KeyboardOutcome
	// Chain: command needs a chained keyboard — show follow-up buttons.
	Chain *ChainOutcome
	// Response: command executed — render text/parts/keyboard/document.
	Response *ResponseOutcome
	// NotHandled: text was not a command (or dot-command with no match).
	NotHandled bool
}

// KeyboardOutcome is set when a bare command has inline keyboard options.
type KeyboardOutcome struct {
	CommandName string
	Header      string
	Options     []command.KeyboardOption
}

// ChainOutcome is set when a command callback needs a follow-up keyboard.
type ChainOutcome struct {
	CommandName string
	Label       string // text shown above the keyboard
	Options     []command.KeyboardOption
}

// ResponseOutcome is set when a command executed and produced a response.
type ResponseOutcome struct {
	Result Result
	// LookupText is the normalized text used for keyboard lookups (with dot
	// commands converted to slash form). Used by the platform to extract the
	// command name for keyboard rendering.
	LookupText string
	// WizardStep is the structured step of the wizard THIS command activated
	// (the scope's wizard generation changed across the dispatch and a wizard
	// is still active), when that step has at least one option. Platforms
	// render the response with wizard buttons (WizardButtons, "wz:"
	// callbacks); nil renders plain text. A command run while an OLDER wizard
	// was already active does not get that wizard's buttons.
	WizardStep *question.Question
}

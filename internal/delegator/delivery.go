package delegator

import "time"

// InputRef names the platform message an input carries: the conversation it
// was sent in and the sender-side id of the message. A backend that confirms
// consumption (DeliveryTracker) reports these back, which is what lets the app
// show "the agent has read this" (✓✓) on the user's own bubble (#2050).
type InputRef struct {
	ConversationID string `json:"conversationId,omitempty"`
	MessageID      string `json:"messageId"`
}

// PendingInput is one user-role write to a backend that the backend has not yet
// confirmed consuming. It carries everything needed to write it again — into a
// respawned process, or into the next process after a gateway restart — so it
// is also the unit foci persists (#2050).
type PendingInput struct {
	// ID is the backend-level id of the write (ccstream: the stdin message's
	// uuid, echoed back in user_message_uuids). A re-write reuses it, so one
	// message keeps one identity through every redelivery.
	ID          string       `json:"id"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Refs        []InputRef   `json:"refs,omitempty"`
	// Source is the InjectSource name the write was made under ("user",
	// "steer", "system", "nudge", ...). Informational: redelivery always runs
	// as a fresh turn.
	Source string `json:"source,omitempty"`
	// WrittenAt is when this copy was written.
	WrittenAt time.Time `json:"writtenAt"`
	// SessionID and WorkDir locate the backend transcript the write went to,
	// so a later process can check whether it was folded in after all.
	SessionID string `json:"sessionId,omitempty"`
	WorkDir   string `json:"workDir,omitempty"`
	// Redeliveries counts how many times foci has re-sent this input. Capped
	// (MaxRedeliveries) so a message no backend will ever acknowledge cannot
	// loop.
	Redeliveries int `json:"redeliveries,omitempty"`
}

// MaxRedeliveries bounds how many times one input is re-sent. A message still
// unconfirmed after this many fresh processes is dropped with an error log
// rather than replayed forever.
const MaxRedeliveries = 3

// DeliveryHooks are the callbacks a DeliveryTracker fires. Each may be nil.
// They run on backend goroutines (the writer's caller, the stdout reader, the
// process waiter) and must not block on the backend.
type DeliveryHooks struct {
	// OnPending fires just before an input is written. The caller persists it
	// so the input survives a gateway restart.
	OnPending func(p PendingInput)
	// OnConsumed fires once the backend confirms the input reached the model's
	// conversation — an explicit ack, or its presence in the backend's
	// transcript after the process died.
	OnConsumed func(p PendingInput)
	// OnUndelivered fires after the process has gone, with every input it
	// never consumed, in write order. The caller re-sends them as fresh turns.
	OnUndelivered func(ps []PendingInput)
	// OnDropped fires when an input leaves tracking unconsumed and is NOT
	// handed back: its write failed and the error went to the caller (whose
	// own fallback now owns the message), or it hit MaxRedeliveries.
	OnDropped func(p PendingInput)
}

// DeliveryTracker is the optional capability of a backend that can confirm it
// consumed each input (#2050). A backend implementing it:
//
//   - reports every user-role write through DeliveryHooks.OnPending,
//   - fires OnConsumed when the input demonstrably reached the model,
//   - and, when its process dies or is closed, hands back the inputs it never
//     consumed through OnUndelivered instead of silently losing them.
//
// Backends without it (opencode, codex, cctmux, the API loop) never report
// consumption: their inputs are fire-and-forget, and a client showing delivery
// ticks must stay at "server received" for them.
type DeliveryTracker interface {
	SetDeliveryHooks(h DeliveryHooks)
}

// TranscriptChecker reports whether the backend transcript for sessionID in
// workDir already holds the input with id — i.e. a process folded it in before
// it died. Used to decide, after a gateway restart, whether a persisted
// PendingInput still needs re-sending. found=false with a nil error means the
// transcript was read and the input is not in it.
type TranscriptChecker func(workDir, sessionID, id string) (found bool, err error)

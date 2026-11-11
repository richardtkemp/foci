package app

import (
	"context"
	"time"

	"foci/internal/voice"
)

// Spoken block summaries (#3050). In voice mode a bubble's mermaid diagrams,
// tables and code blocks are spoken as one short sentence each, in place of
// their source (voice.SpeakBlocks). The sentence comes from the fast-summary
// path the pin label uses (Dick, 2026-11-11: "We already have a path for
// summaries (like message pins), use that"): the same model, endpoint and key
// (useShared), and so the High tier on a local endpoint's queue (#3019; Dick:
// "These summaries to be high priority"). When that path is off or
// misconfigured, or a call fails or misses voiceBlockSummaryBound, the block's
// marker ("Table omitted.") is spoken: the source is never read.

const (
	// voiceBlockSummaryBound is how long a bubble waits for its block
	// summaries (all made at once) before speaking the markers instead. The
	// bubble's text is delivered after its clip is made, so this also bounds
	// how much later the text appears. clutch's default: the default model
	// (Llama 3.1 8B on Groq) answers in well under it.
	voiceBlockSummaryBound = 5 * time.Second
	// voiceBlockSummaryWords is the sentence length the prompt asks for (Dick:
	// "Very short description ... max one sentence").
	voiceBlockSummaryWords = 15
	// voiceBlockSummaryMaxTokens is ample for fifteen words.
	voiceBlockSummaryMaxTokens = 60
	// voiceBlockSummaryMaxInputRunes caps the block source sent: a long
	// table or diagram is summarised from its start.
	voiceBlockSummaryMaxInputRunes = 4000

	voiceBlockSummaryWhat = "voice block summary"
)

// voiceBlockPrompt is the system prompt for a block of one kind. Dick's ruling:
// "Should capture the vague spirit of the contents rather than trying to fully
// describe them."
func voiceBlockPrompt(k voice.BlockKind) string {
	what := "a code block"
	switch k {
	case voice.BlockDiagram:
		what = "a diagram, written as mermaid source"
	case voice.BlockTable:
		what = "a table, written in markdown"
	}
	return "A chat message is being read aloud to the user. In place of " + what + " in it, the reader says one short sentence. " +
		"The user sends the " + string(k) + ". Write that sentence: the rough gist of what it is about, not a description of its contents. " +
		"Do not list or read out its items, names, numbers or code. AT MOST FIFTEEN WORDS, one sentence, plain words a voice can say, " +
		"for example \"A table comparing three phone plans by price.\" Reply with the sentence only: no quotes, no preamble, no markdown."
}

func voiceBlockAsk(k voice.BlockKind) summaryAsk {
	return summaryAsk{
		what:          voiceBlockSummaryWhat,
		use:           useShared,
		system:        voiceBlockPrompt(k),
		maxInputRunes: voiceBlockSummaryMaxInputRunes,
		maxTokens:     voiceBlockSummaryMaxTokens,
		maxWords:      voiceBlockSummaryWords,
	}
}

// voiceSpeechText is text as a voice-mode clip speaks it, before
// voice.NormalizeForSpeech: each diagram, table and code block replaced by its
// summary or marker. session and agentID name where the model calls are booked.
func (h *Hub) voiceSpeechText(ctx context.Context, text, session, agentID string) string {
	var summarise voice.BlockSummariser
	if h != nil && h.pinSum.usable(useShared) {
		sum := h.pinSum
		summarise = func(ctx context.Context, b voice.Block) (string, error) {
			return sum.ask(ctx, voiceBlockAsk(b.Kind), b.Source, session, agentID)
		}
	}
	return voice.SpeakBlocks(ctx, text, summarise, voiceBlockSummaryBound)
}

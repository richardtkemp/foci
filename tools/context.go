package tools

import "context"

// voiceReplyFuncKey is the context key for VoiceReplyFunc.
type voiceReplyFuncKey struct{}

// WithVoiceReplyFunc attaches a VoiceReplyFunc to a context.
// Called by the agent loop to inject the delivery channel for the TTS tool.
func WithVoiceReplyFunc(ctx context.Context, fn VoiceReplyFunc) context.Context {
	return context.WithValue(ctx, voiceReplyFuncKey{}, fn)
}

// VoiceReplyFuncFromContext extracts the VoiceReplyFunc from context (nil if absent).
func VoiceReplyFuncFromContext(ctx context.Context) VoiceReplyFunc {
	fn, _ := ctx.Value(voiceReplyFuncKey{}).(VoiceReplyFunc)
	return fn
}

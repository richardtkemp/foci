package tools

import "context"

// An API character spawn's own identity (#2676). Its one-shot tool loop runs
// on its spawner's context, so the context's session key is the spawner's.
// A delegated character spawn instead runs as a batch session of its own
// (<root>/b<ts>), and that is what its whoami reports and its send_to_session
// is sent from. Dick 2026-10-30, on that parity gap: "give API the same". So
// the API one-shot gets a key minted the same way (session.NewBatchKey), and
// the two tools that state who is calling read it from here:
//   - whoami reports it, with the model the spawn runs on;
//   - send_to_session sends from it, as a one-shot caller, so the target's
//     reply goes to the target's own chat and not back to the spawner's session.
//
// Every other tool still sees the spawner's key, as before: the minted key
// addresses no session, so a tool that delivers to its caller's session later
// (a background HTTP result, a reminder) must not be handed it.
type spawnSelf struct {
	key   string // the spawn's own session key
	model string // the model it runs on
}

type spawnSelfKey struct{}

func withSpawnSelf(ctx context.Context, key, model string) context.Context {
	return context.WithValue(ctx, spawnSelfKey{}, spawnSelf{key: key, model: model})
}

// spawnSelfFrom is the API character spawn's own identity on ctx; ok is false
// for any other context.
func spawnSelfFrom(ctx context.Context) (spawnSelf, bool) {
	s, ok := ctx.Value(spawnSelfKey{}).(spawnSelf)
	return s, ok
}

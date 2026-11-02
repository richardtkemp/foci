package agent

import (
	"context"
	"encoding/json"

	"foci/internal/session"
)

// A background-work (todo) turn across a usage hold (#2491; Dick 2026-11-02:
// "the background work on to-dos, that must stop and be resumed", and
// "Accept resuming them from cold").
//
// RunBackgroundResumable runs the turn; a hold start interrupts it, it pauses
// (its session in the paused set, so keepalive warms it), and at lift it goes
// on with the hidden resume line, cold cache or not. While it is paused, its
// session carries a PausedBackground record (session.MetaKeyUsageHoldPaused),
// so a foci restart during the pause resumes it too: RestorePausedBackground
// hands each record to the gateway, which runs ResumePausedBackground.

// PausedBackground is a paused background-work turn as stored: the session
// it runs on and how the gateway runs a turn there (Strategy: "in_place",
// "fork" or "independent").
type PausedBackground struct {
	Key        string `json:"key"`
	Strategy   string `json:"strategy"`
	BranchType string `json:"branch_type"`
}

// RunBackgroundResumable runs first on pb.Key through run, resuming it across
// usage holds. It returns run's last error, or ctx's when ctx ends during a
// pause (the record stays, so the next start resumes it).
func (a *Agent) RunBackgroundResumable(ctx context.Context, pb PausedBackground, first string, run func(context.Context, string) error) error {
	text := first
	for {
		tctx, done := a.UsageHoldTurn(ctx, pb.Key, "background")
		err := run(tctx, text)
		if !done() {
			a.forgetPausedBackground(pb.Key)
			return err
		}
		a.logger().Infof("usage hold: background work on %s paused; it resumes at lift", pb.Key)
		a.savePausedBackground(pb)
		if !a.waitPausedBackground(ctx, pb.Key) {
			return ctx.Err()
		}
		a.logger().Infof("usage hold: resuming background work on %s", pb.Key)
		text = UsageResumeLine
	}
}

// ResumePausedBackground resumes a background-work turn a restart found
// paused: it waits for the lift (paused, so keepalive warms it), then goes on
// with the resume line as RunBackgroundResumable.
func (a *Agent) ResumePausedBackground(ctx context.Context, pb PausedBackground, run func(context.Context, string) error) error {
	if !a.waitPausedBackground(ctx, pb.Key) {
		return ctx.Err()
	}
	a.logger().Infof("usage hold: resuming background work on %s (paused before the restart)", pb.Key)
	return a.RunBackgroundResumable(ctx, pb, UsageResumeLine, run)
}

// RestorePausedBackground hands each paused background-work turn of this
// agent to start, with the agent's lifetime context. Call once at startup,
// after the inboxes are up.
func (a *Agent) RestorePausedBackground(start func(ctx context.Context, pb PausedBackground)) {
	if a.SessionIndex == nil {
		return
	}
	keys, err := a.SessionIndex.SessionKeysWithMetadata(session.MetaKeyUsageHoldPaused)
	if err != nil {
		a.logger().Warnf("usage hold: restore paused background work: %v", err)
		return
	}
	ctx := a.inboxCtx
	if ctx == nil {
		ctx = context.Background()
	}
	for _, sk := range keys {
		if parsed, err := session.ParseSessionKey(sk); err != nil || parsed.AgentID != a.AgentID {
			continue
		}
		raw, err := a.SessionIndex.GetSessionMetadata(sk, session.MetaKeyUsageHoldPaused)
		var pb PausedBackground
		if err != nil || json.Unmarshal([]byte(raw), &pb) != nil || pb.Key == "" {
			a.logger().Warnf("usage hold: paused background record on %s unreadable — dropped", sk)
			a.forgetPausedBackground(sk)
			continue
		}
		a.logger().Infof("usage hold: restoring paused background work on %s (%s)", pb.Key, pb.Strategy)
		start(ctx, pb)
	}
}

// waitPausedBackground holds key in the paused set until the hold lifts.
func (a *Agent) waitPausedBackground(ctx context.Context, key string) bool {
	a.UsageHoldPause(key, "background", true)
	defer a.UsageHoldPause(key, "background", false)
	return a.WaitUsageLift(ctx)
}

func (a *Agent) savePausedBackground(pb PausedBackground) {
	if a.SessionIndex == nil {
		return
	}
	raw, _ := json.Marshal(pb)
	if err := a.SessionIndex.SetSessionMetadata(pb.Key, session.MetaKeyUsageHoldPaused, string(raw)); err != nil {
		a.logger().Warnf("usage hold: persist paused background work on %s: %v", pb.Key, err)
	}
}

func (a *Agent) forgetPausedBackground(key string) {
	if a.SessionIndex == nil {
		return
	}
	if v, _ := a.SessionIndex.GetSessionMetadata(key, session.MetaKeyUsageHoldPaused); v == "" {
		return
	}
	if err := a.SessionIndex.DeleteSessionMetadata(key, session.MetaKeyUsageHoldPaused); err != nil {
		a.logger().Warnf("usage hold: clear paused background work on %s: %v", key, err)
	}
}

// PausedBackgroundRecorded reports whether key holds a paused background-work
// record: its session must then keep its resume id for the next start.
func (a *Agent) PausedBackgroundRecorded(key string) bool {
	if a.SessionIndex == nil {
		return false
	}
	v, _ := a.SessionIndex.GetSessionMetadata(key, session.MetaKeyUsageHoldPaused)
	return v != ""
}

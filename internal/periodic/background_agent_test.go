package periodic

import "context"

// fakeBackgroundAgent is a configurable test double for BackgroundAgent. Its
// fields mirror the closures the Runner formerly took directly, so tests inject
// per-gate behaviour exactly as before — just nested under `agent:` in the
// Runner literal. Unset fields use benign defaults that reproduce the old
// nil-closure degradation (Branch=false, CanFire=allowed, SessionKey="", etc.).
type fakeBackgroundAgent struct {
	branchFn         func(branchType, parentKey, promptText string, noCompact bool) bool
	hasActiveWorkFn  func() int
	isTurnInFlightFn func(parentBase string) bool
	sessionKeyFn     func() string
	canFireFn        func(ctx context.Context, sessionKey string) (bool, string)
	runOnceFn        func(ctx context.Context, prompt, systemPrompt string) (string, error)
	resetFn          func(ctx context.Context, sessionKey string) error
}

func (f *fakeBackgroundAgent) Branch(branchType, parentKey, promptText string, noCompact bool) bool {
	if f.branchFn != nil {
		return f.branchFn(branchType, parentKey, promptText, noCompact)
	}
	return false
}

func (f *fakeBackgroundAgent) HasActiveWork() int {
	if f.hasActiveWorkFn != nil {
		return f.hasActiveWorkFn()
	}
	return 0
}

func (f *fakeBackgroundAgent) DrainRateLimitQueue(context.Context) {}

func (f *fakeBackgroundAgent) IsTurnInFlight(parentBase string) bool {
	if f.isTurnInFlightFn != nil {
		return f.isTurnInFlightFn(parentBase)
	}
	return false
}

func (f *fakeBackgroundAgent) SessionKey() string {
	if f.sessionKeyFn != nil {
		return f.sessionKeyFn()
	}
	return ""
}

func (f *fakeBackgroundAgent) CanFire(ctx context.Context, sessionKey string) (bool, string) {
	if f.canFireFn != nil {
		return f.canFireFn(ctx, sessionKey)
	}
	return true, ""
}

func (f *fakeBackgroundAgent) RunOnce(ctx context.Context, prompt, systemPrompt string) (string, error) {
	if f.runOnceFn != nil {
		return f.runOnceFn(ctx, prompt, systemPrompt)
	}
	return "", nil
}

func (f *fakeBackgroundAgent) ResetSession(ctx context.Context, sessionKey string) error {
	if f.resetFn != nil {
		return f.resetFn(ctx, sessionKey)
	}
	return nil
}

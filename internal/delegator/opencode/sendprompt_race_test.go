package opencode

import (
	"context"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// #2845 (1): SendControl writes b.model under b.mu on a control goroutine while
// a prompt goes out on another. Only meaningful under the race detector:
//
//	make local-test-one CGO_ENABLED=1 GOFLAGS=-race PKG=./internal/delegator/opencode/ RUN=TestSendPrompt_ModelRead
//
// Use local-test-one: test-one runs on a build host and does not forward GOFLAGS,
// so there it passes without the detector.
func TestSendPrompt_ModelReadVsSetModel(t *testing.T) {
	rec := &recordingHandler{}
	b := newReadyBackend(t, rec)
	b.resolveModelFn = func(_ context.Context, _, _, model string) (string, error) {
		return model, nil
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := b.SendControl(context.Background(), &delegator.SetModelRequest{Model: "p/m"}); err != nil {
				t.Errorf("SendControl: %v", err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := b.sendPrompt(context.Background(), "x", nil, ""); err != nil {
				t.Errorf("sendPrompt: %v", err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := b.sendPromptWithAgent(context.Background(), "x", "plan"); err != nil {
				t.Errorf("sendPromptWithAgent: %v", err)
			}
		}
	}()
	wg.Wait()
}

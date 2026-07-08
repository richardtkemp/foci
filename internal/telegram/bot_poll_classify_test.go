package telegram

import (
	"sync/atomic"
	"testing"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

func TestClassifyRecovery(t *testing.T) {
	cases := []struct {
		name              string
		consecutiveErrors int
		active            bool
		wantLog           bool
		wantWarn          bool
	}{
		{"below threshold — silent", episodeLogThreshold - 1, false, false, false},
		{"below threshold even if active — silent", episodeLogThreshold - 1, true, false, false},
		{"idle-window run — INFO", episodeLogThreshold, false, true, false},
		{"active run — WARN", episodeLogThreshold, true, true, true},
		{"long idle run — INFO", episodeLogThreshold + 20, false, true, false},
		{"long active run — WARN", episodeLogThreshold + 20, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotLog, gotWarn := classifyRecovery(tc.consecutiveErrors, tc.active)
			if gotLog != tc.wantLog || gotWarn != tc.wantWarn {
				t.Fatalf("classifyRecovery(%d, %v) = (log=%v, warn=%v), want (log=%v, warn=%v)",
					tc.consecutiveErrors, tc.active, gotLog, gotWarn, tc.wantLog, tc.wantWarn)
			}
		})
	}
}

// sendStub satisfies botClient (embedded, nil for unused methods) so we can
// exercise activityClient's send-stamping override in isolation.
type sendStub struct{ botClient }

func (sendStub) SendMessage(int64, string, *gotgbot.SendMessageOpts) (*gotgbot.Message, error) {
	return &gotgbot.Message{}, nil
}

var _ botClient = activityClient{}

func TestActivityClient_StampsOnSend(t *testing.T) {
	last := &atomic.Int64{}
	c := activityClient{botClient: sendStub{}, lastSendAt: last}
	if last.Load() != 0 {
		t.Fatal("precondition: lastSendAt should start zero")
	}
	if _, err := c.SendMessage(1, "hi", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if last.Load() == 0 {
		t.Fatal("SendMessage did not stamp lastSendAt")
	}
}

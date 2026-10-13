package app

import (
	"testing"
)

// TestClientMessage_SteerWireField_MapsToEnvelope is a CHARACTERISATION test
// (#1318): it pins that the app's existing wire field ("steer"/"queue"/"")
// still maps to the same envelope preference across the mapping's move to
// platform.SteerPreferenceFromWire — the app path must not drift from the
// HTTP /send and chat-prefix paths that now share the one mapping.
//
// Deliberately asserts INTEGER literals (SteerDefault=0, SteerAlways=1,
// SteerNever=2) instead of naming the constants, so this file compiles and
// passes on the tree the move was written against and keeps guarding the
// wire contract afterwards.
func TestClientMessage_SteerWireField_MapsToEnvelope(t *testing.T) {
	tests := []struct {
		name string
		wire string // "", "steer", "queue", "bogus"; "" omits the key entirely
		want int
	}{
		{"empty follows the config default", ``, 0},
		{"steer folds", `"steer"`, 1},
		{"queue waits", `"queue"`, 2},
		{"unknown follows the config default", `"bogus"`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHub()
			fa := registerFakeAgent(h, "ag")
			c := fakeClient()
			c.hub = h
			c.deviceID = "dev-1"

			steerField := ""
			if tc.wire != "" {
				steerField = `,"steer":` + tc.wire
			}
			h.dispatchInbound(c, []byte(`{"t":"message","id":"m1","seq":1,"d":{"conversationId":"conv-1","agentId":"ag","text":"hi"`+steerField+`}}`))

			if fa.env == nil {
				t.Fatal("no envelope enqueued — frame did not route as a user turn")
			}
			if got := int(fa.env.Steer); got != tc.want {
				t.Errorf("Envelope.Steer = %d, want %d for wire value %s", got, tc.want, tc.wire)
			}
		})
	}
}

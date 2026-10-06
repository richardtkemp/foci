package app

import (
	"reflect"
	"testing"
	"time"

	"foci/internal/fap"
)

// viewingSyncs returns the ConversationIDs of every ViewingSync queued for c, in order.
func viewingSyncs(t *testing.T, c *wsClient) [][]string {
	t.Helper()
	var out [][]string
	for _, f := range drain(t, c) {
		if f.t != fap.TypeViewingSync {
			continue
		}
		ids := []string{}
		raw, _ := f.d["conversationIds"].([]any)
		for _, r := range raw {
			ids = append(ids, r.(string))
		}
		out = append(out, ids)
	}
	return out
}

func deviceClient(h *Hub, device string) *wsClient {
	c := fakeClientFor(h)
	c.deviceID = device
	h.clients[c] = struct{}{}
	return c
}

// A device's viewing report reaches the user's OTHER devices, not itself (#2202).
func TestViewingPut_SyncsToOtherDevices(t *testing.T) {
	h := newTestHub()
	mac, phone := deviceClient(h, "mac"), deviceClient(h, "phone")

	h.handleViewingPut(mac, fap.ViewingPut{ConversationID: "c1"})

	if got := viewingSyncs(t, phone); !reflect.DeepEqual(got, [][]string{{"c1"}}) {
		t.Errorf("phone syncs = %v, want [[c1]]", got)
	}
	if got := viewingSyncs(t, mac); !reflect.DeepEqual(got, [][]string{{}}) {
		t.Errorf("mac syncs = %v, want [[]] (its own view is not 'elsewhere')", got)
	}
}

// Moving off the chat, and closing the socket, both clear it for the others.
func TestViewing_ClearedByEmptyPutAndBySocketClose(t *testing.T) {
	h := newTestHub()
	mac, phone := deviceClient(h, "mac"), deviceClient(h, "phone")

	h.handleViewingPut(mac, fap.ViewingPut{ConversationID: "c1"})
	h.handleViewingPut(mac, fap.ViewingPut{})
	if got := viewingSyncs(t, phone); !reflect.DeepEqual(got, [][]string{{"c1"}, {}}) {
		t.Errorf("phone syncs after empty put = %v, want [[c1] []]", got)
	}

	h.handleViewingPut(mac, fap.ViewingPut{ConversationID: "c2"})
	drain(t, phone)
	h.removeClient(mac)
	if got := viewingSyncs(t, phone); !reflect.DeepEqual(got, [][]string{{}}) {
		t.Errorf("phone syncs after mac closed = %v, want [[]]", got)
	}
}

// An unchanged report sends nothing; a just-hello'd device learns the current set.
func TestViewing_RepeatIsQuietAndHelloCatchesUp(t *testing.T) {
	h := newTestHub()
	mac := deviceClient(h, "mac")
	h.handleViewingPut(mac, fap.ViewingPut{ConversationID: "c1"})
	phone := deviceClient(h, "phone")
	drain(t, mac)

	h.handleViewingPut(mac, fap.ViewingPut{ConversationID: "c1"})
	if got := viewingSyncs(t, phone); len(got) != 0 {
		t.Errorf("unchanged report fanned out %v", got)
	}

	h.pushViewing(phone)
	if got := viewingSyncs(t, phone); !reflect.DeepEqual(got, [][]string{{"c1"}}) {
		t.Errorf("hello catch-up = %v, want [[c1]]", got)
	}
}

// The offline wake push is skipped for a conversation a connected device is
// viewing, and still sent for any other conversation (#2202).
func TestPushNotify_SkipsConversationViewedElsewhere(t *testing.T) {
	h := newTestHub()
	tk := newPushTokens()
	tk.set("phone", "tok-phone")
	p, rec := newRecordingPusher(t, tk)
	h.pusher = p
	mac := deviceClient(h, "mac")
	h.handleViewingPut(mac, fap.ViewingPut{ConversationID: "c1"})

	h.pushNotify(pushPayload{ConvID: "c1", Preview: "read on the mac"})
	h.pushNotify(pushPayload{ConvID: "c2", Preview: "nobody is looking"})
	rec.waitFor(t, "tok-phone", 1)
	time.Sleep(50 * time.Millisecond)
	pushes := rec.pushes()
	if len(pushes) != 1 || pushes[0].Data["conversationId"] != "c2" {
		t.Errorf("pushes = %+v, want exactly one, for c2", pushes)
	}
}

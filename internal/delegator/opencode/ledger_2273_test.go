package opencode

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"foci/internal/delegator/accounting"
)

// #2273: opencode's §8 checks as ledger alarms — a completed message with no
// counts, a tokens.total that is not the classes' sum, a tokens key foci does
// not know, and the per-message cost divergence.

func alarmsOf(alarms []accounting.Alarm, inv string) []accounting.Alarm {
	var out []accounting.Alarm
	for _, a := range alarms {
		if a.Invariant == inv {
			out = append(out, a)
		}
	}
	return out
}

// TestCompletedMessageWithoutTokensAlarms: a message opencode marks completed
// with no tokens object cannot be booked; that is an alarm, not a silent skip.
func TestCompletedMessageWithoutTokensAlarms(t *testing.T) {
	alarms, path := liveLedger(t)
	b := ledgerBackend(t)
	_, msg := recordedMessage(t)
	msg.Tokens = nil
	b.onMessageUpdated(msg)

	got := alarmsOf(*alarms, accounting.InvCompletedHasTokens)
	if len(got) != 1 || got[0].Backend != accounting.BackendOpencode || !strings.Contains(got[0].Detail, msg.ID) {
		t.Fatalf("invCompletedHasTokens = %+v, want one naming message %s", got, msg.ID)
	}
	if calls := readBooked(t, path); len(calls) != 0 {
		t.Errorf("booked %+v from a message with no counts", calls)
	}

	// A streaming update (not completed) with no tokens yet is normal.
	*alarms = nil
	msg.ID, msg.Time.Completed = "msg_streaming", 0
	b.onMessageUpdated(msg)
	if len(*alarms) != 0 {
		t.Errorf("alarms on a streaming update: %+v", *alarms)
	}
}

// TestTokenTotalNotTheSumAlarms: tokens.total is input + output + reasoning +
// cache.read + cache.write; a message where it is not alarms, and is still
// booked from the classes.
func TestTokenTotalNotTheSumAlarms(t *testing.T) {
	alarms, path := liveLedger(t)
	b := ledgerBackend(t)
	_, msg := recordedMessage(t)
	off := *msg.Tokens.Total + 1000
	msg.Tokens.Total = &off
	b.onMessageUpdated(msg)

	got := alarmsOf(*alarms, accounting.InvTokenTotal)
	if len(got) != 1 || !strings.Contains(got[0].Detail, "50528") || !strings.Contains(got[0].Detail, "49528") {
		t.Fatalf("invTokenTotal = %+v, want one naming total 50528 and sum 49528", got)
	}
	if calls := readBooked(t, path); len(calls) != 1 || calls[0].tokens["input"] != 2248 {
		t.Errorf("calls = %+v, want the message booked from its classes", calls)
	}
}

// TestRecordedMessageTotalIsTheSum: the real capture's total is its classes'
// sum, so the check is silent on it (premise of the check, and of the
// recorded fixture staying silent).
func TestRecordedMessageTotalIsTheSum(t *testing.T) {
	alarms, _ := liveLedger(t)
	b := ledgerBackend(t)
	ev, _ := recordedMessage(t)
	b.handleEvent(ev)
	for _, inv := range []string{accounting.InvTokenTotal, accounting.InvUnseenField, accounting.InvCompletedHasTokens} {
		if got := alarmsOf(*alarms, inv); len(got) != 0 {
			t.Errorf("%s on the recorded message: %+v", inv, got)
		}
	}
}

// recordedEventWith is the recorded event with its raw bytes edited.
func recordedEventWith(t *testing.T, edits ...string) rawEvent {
	t.Helper()
	data, err := os.ReadFile("testdata/message_updated_reasoning.json")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(edits); i += 2 {
		if !bytes.Contains(data, []byte(edits[i])) {
			t.Fatalf("premise: fixture lacks %q", edits[i])
		}
		data = bytes.Replace(data, []byte(edits[i]), []byte(edits[i+1]), 1)
	}
	var ev rawEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

// TestUnseenTokenKeyAlarmsOnce: a tokens key opencode adds alarms once, with
// its path and value; the next message carrying it does not alarm again.
func TestUnseenTokenKeyAlarmsOnce(t *testing.T) {
	alarms, path := liveLedger(t)
	b := ledgerBackend(t)
	b.handleEvent(recordedEventWith(t, `"reasoning":305`, `"reasoning":305,"audio":{"input":5}`))
	b.handleEvent(recordedEventWith(t, `"reasoning":305`, `"reasoning":305,"audio":{"input":6}`,
		`"msg_0e9070c34001mu747je03YsCxz"`, `"msg_second"`))

	got := alarmsOf(*alarms, accounting.InvUnseenField)
	if len(got) != 1 {
		t.Fatalf("invUnseenField raised %d times, want once: %+v", len(got), got)
	}
	if d := got[0].Detail; !strings.Contains(d, "opencode") || !strings.Contains(d, `audio={"input":5}`) {
		t.Errorf("alarm must name the adapter, key and value: %s", d)
	}
	if calls := readBooked(t, path); len(calls) != 2 {
		t.Errorf("booked %d calls, want 2: an unseen key does not stop the booking", len(calls))
	}
}

// TestCostDivergenceIsALedgerAlarm: opencode's per-message cost off foci's
// price by more than the tolerance raises invCostDivergence through the ledger
// (an ERROR through the expectation guard, like CC's), with the per-class
// breakdown.
func TestCostDivergenceIsALedgerAlarm(t *testing.T) {
	alarms, _ := liveLedger(t)
	b := ledgerBackend(t)
	_, msg := recordedMessage(t)
	msg.ModelID, msg.ProviderID = "claude-opus-5", "anthropic"
	msg.Cost = 100 // far above foci's price of these counts
	b.onMessageUpdated(msg)

	got := alarmsOf(*alarms, accounting.InvCostDivergence)
	if len(got) != 1 {
		t.Fatalf("invCostDivergence = %+v, want one", *alarms)
	}
	a := got[0]
	if a.Backend != accounting.BackendOpencode || a.Warn {
		t.Errorf("alarm = %+v, want an ERROR on backend opencode", a)
	}
	for _, want := range []string{"claude-opus-5", "100.000000", "reasoning=305", msg.ID} {
		if !strings.Contains(a.Detail, want) {
			t.Errorf("alarm missing %q: %s", want, a.Detail)
		}
	}
}

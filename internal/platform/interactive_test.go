package platform

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSendInteractiveMessageWithButtonSender verifies the happy path: buttons
// are stored, callback data is formatted correctly, and the ButtonSender
// receives the right arguments.
func TestSendInteractiveMessageWithButtonSender(t *testing.T) {
	clearIMStore(t)

	bs := &mockButtonSender{msgID: "42"}
	buttons := []ButtonChoice{
		{Label: "Yes", Data: "yes"},
		{Label: "No", Data: "no"},
	}

	var cbCalled bool
	_, err := SendInteractiveMessageWithID(staticResolver(bs), "test-id", "Choose:", buttons, func(choice ButtonChoice) string {
		cbCalled = true
		return "You chose: " + choice.Label
	}, nil)
	if err != nil {
		t.Fatalf("SendInteractiveMessage: %v", err)
	}

	// ButtonSender should have been called.
	if bs.text != "Choose:" {
		t.Errorf("sent text = %q, want %q", bs.text, "Choose:")
	}
	if bs.prefix != "im:" {
		t.Errorf("prefix = %q, want %q", bs.prefix, "im:")
	}
	if len(bs.buttons) != 2 {
		t.Fatalf("sent %d buttons, want 2", len(bs.buttons))
	}

	// Each button's Data should be "<promptID>:<index>".
	for i, b := range bs.buttons {
		parts := strings.SplitN(b.Data, ":", 2)
		if len(parts) != 2 {
			t.Errorf("button %d data = %q, want format '<promptID>:<index>'", i, b.Data)
			continue
		}
		if parts[1] != strconv.Itoa(i) {
			t.Errorf("button %d index = %q, want %q", i, parts[1], strconv.Itoa(i))
		}
	}

	// Trigger the callback for the first button.
	promptID := strings.SplitN(bs.buttons[0].Data, ":", 2)[0]
	editText, choiceData, ok := HandleInteractiveCallback(promptID + ":0")
	if !ok {
		t.Fatal("HandleInteractiveCallback returned ok=false")
	}
	if !cbCalled {
		t.Error("callback was not invoked")
	}
	if editText != "You chose: Yes" {
		t.Errorf("editText = %q, want %q", editText, "You chose: Yes")
	}
	if choiceData != "yes" {
		t.Errorf("choiceData = %q, want %q", choiceData, "yes")
	}
}

// TestSendInteractiveMessageFallback verifies that when the connection does
// not implement ButtonSender, a plain text fallback with numbered choices is
// sent via SendText.
func TestSendInteractiveMessageFallback(t *testing.T) {
	clearIMStore(t)

	mc := &mockConnectionCapture{}
	buttons := []ButtonChoice{
		{Label: "Apple"},
		{Label: "Banana"},
		{Label: "Cherry"},
	}

	_, err := SendInteractiveMessageWithID(staticResolver(mc), "test-id", "Pick a fruit:", buttons, nil, nil)
	if err != nil {
		t.Fatalf("SendInteractiveMessage: %v", err)
	}

	if !strings.Contains(mc.lastText, "Pick a fruit:") {
		t.Errorf("fallback text missing prompt: %q", mc.lastText)
	}
	if !strings.Contains(mc.lastText, "1. Apple") {
		t.Errorf("fallback text missing '1. Apple': %q", mc.lastText)
	}
	if !strings.Contains(mc.lastText, "2. Banana") {
		t.Errorf("fallback text missing '2. Banana': %q", mc.lastText)
	}
	if !strings.Contains(mc.lastText, "3. Cherry") {
		t.Errorf("fallback text missing '3. Cherry': %q", mc.lastText)
	}
	if !strings.Contains(mc.lastText, "Reply with your choice") {
		t.Errorf("fallback text missing 'Reply with your choice': %q", mc.lastText)
	}
}

// TestSendInteractiveMessageButtonSenderError verifies that when
// SendTextWithButtons fails, the callback is cleaned up and the error
// is propagated.
func TestSendInteractiveMessageButtonSenderError(t *testing.T) {
	clearIMStore(t)

	storeSizeBefore := imStoreSize()

	bs := &mockButtonSender{err: fmt.Errorf("send failed")}
	buttons := []ButtonChoice{{Label: "A", Data: "a"}}

	_, err := SendInteractiveMessageWithID(staticResolver(bs), "test-id", "text", buttons, func(ButtonChoice) string { return "" }, nil)
	if err == nil {
		t.Fatal("expected error from SendInteractiveMessage")
	}
	if !strings.Contains(err.Error(), "send failed") {
		t.Errorf("error = %q, want mention of 'send failed'", err)
	}

	// The callback should have been cleaned up.
	if imStoreSize() != storeSizeBefore {
		t.Errorf("store grew from %d to %d after failed send", storeSizeBefore, imStoreSize())
	}
}

// TestHandleInteractiveCallbackOneShot verifies that callbacks are one-shot:
// the first call returns ok=true, and a second call with the same data
// returns ok=false.
func TestHandleInteractiveCallbackOneShot(t *testing.T) {
	clearIMStore(t)

	bs := &mockButtonSender{msgID: "99"}
	buttons := []ButtonChoice{{Label: "OK", Data: "ok"}}
	_, _ = SendInteractiveMessageWithID(staticResolver(bs), "test-id", "text", buttons, func(ButtonChoice) string { return "done" }, nil)

	data := bs.buttons[0].Data
	_, _, ok := HandleInteractiveCallback(data)
	if !ok {
		t.Fatal("first callback should succeed")
	}

	_, _, ok = HandleInteractiveCallback(data)
	if ok {
		t.Fatal("second callback should fail (one-shot)")
	}
}

// TestHandleInteractiveCallbackToggle verifies a non-terminal toggle button:
// pressing it re-renders the prompt in place (revealing/hiding ExtraBody and
// flipping the label) via EditMessageWithButtons, keeps the prompt live, and
// never fires the resolving callback — while the terminal buttons still resolve.
func TestHandleInteractiveCallbackToggle(t *testing.T) {
	clearIMStore(t)

	bs := &mockButtonSender{msgID: "m1"}
	buttons := []ButtonChoice{
		{Label: "Allow", Data: "allow"},
		{Label: "Deny", Data: "deny"},
		{Label: "Show diff", Data: "showdiff", Toggle: &ButtonToggle{
			ExtraBody: "THE DIFF", ShowLabel: "Show diff", HideLabel: "Hide diff",
		}},
	}

	var resolved bool
	_, err := SendInteractiveMessageWithID(staticResolver(bs), "req-t", "Permit?", buttons, func(ButtonChoice) string {
		resolved = true
		return "✅ done"
	}, nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// Reveal.
	edit, _, ok := HandleInteractiveCallback("req-t:2")
	if !ok || edit != "" {
		t.Fatalf("toggle reveal: ok=%v edit=%q, want ok=true edit=\"\"", ok, edit)
	}
	if resolved {
		t.Fatal("toggle must not fire the resolving callback")
	}
	if bs.ewbCalls != 1 {
		t.Fatalf("EditMessageWithButtons calls = %d, want 1", bs.ewbCalls)
	}
	if bs.ewbText != "Permit?\n\nTHE DIFF" {
		t.Errorf("revealed body = %q, want %q", bs.ewbText, "Permit?\n\nTHE DIFF")
	}
	if bs.ewbButtons[2].Label != "Hide diff" {
		t.Errorf("toggle label = %q, want 'Hide diff'", bs.ewbButtons[2].Label)
	}

	// Hide again.
	if _, _, ok := HandleInteractiveCallback("req-t:2"); !ok {
		t.Fatal("second toggle should succeed")
	}
	if bs.ewbText != "Permit?" {
		t.Errorf("hidden body = %q, want %q", bs.ewbText, "Permit?")
	}
	if bs.ewbButtons[2].Label != "Show diff" {
		t.Errorf("toggle label after hide = %q, want 'Show diff'", bs.ewbButtons[2].Label)
	}

	// Prompt is still live: the terminal Allow button resolves.
	edit, _, ok = HandleInteractiveCallback("req-t:0")
	if !ok || !resolved || edit != "✅ done" {
		t.Fatalf("terminal after toggle: ok=%v resolved=%v edit=%q", ok, resolved, edit)
	}
	if _, _, ok := HandleInteractiveCallback("req-t:0"); ok {
		t.Fatal("prompt should be gone after terminal resolve")
	}
}

// TestHandleInteractiveCallbackInvalidFormat verifies that malformed callback
// data returns ok=false without panicking.
func TestHandleInteractiveCallbackInvalidFormat(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "empty string", data: ""},
		{name: "no colon", data: "abc"},
		{name: "non-numeric index", data: "abc:xyz"},
		{name: "negative index", data: "abc:-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, ok := HandleInteractiveCallback(tt.data)
			if ok {
				t.Errorf("HandleInteractiveCallback(%q) = ok, want false", tt.data)
			}
		})
	}
}

// TestHandleInteractiveCallbackOutOfBoundsIndex verifies that a valid
// promptID with an out-of-bounds button index returns ok=false.
func TestHandleInteractiveCallbackOutOfBoundsIndex(t *testing.T) {
	clearIMStore(t)

	bs := &mockButtonSender{msgID: "1"}
	buttons := []ButtonChoice{{Label: "Only", Data: "only"}}
	_, _ = SendInteractiveMessageWithID(staticResolver(bs), "test-id", "text", buttons, func(ButtonChoice) string { return "" }, nil)

	promptID := strings.SplitN(bs.buttons[0].Data, ":", 2)[0]

	// Index 1 is out of bounds (only index 0 exists).
	_, _, ok := HandleInteractiveCallback(promptID + ":1")
	if ok {
		t.Fatal("out-of-bounds index should return false")
	}
}

// TestHandleInteractiveCallbackNilCallback verifies that a nil callback
// produces an empty edit string without panicking.
func TestHandleInteractiveCallbackNilCallback(t *testing.T) {
	clearIMStore(t)

	bs := &mockButtonSender{msgID: "1"}
	buttons := []ButtonChoice{{Label: "Go", Data: "go"}}
	_, _ = SendInteractiveMessageWithID(staticResolver(bs), "test-id", "text", buttons, nil, nil)

	data := bs.buttons[0].Data
	editText, _, ok := HandleInteractiveCallback(data)
	if !ok {
		t.Fatal("HandleInteractiveCallback returned false")
	}
	if editText != "" {
		t.Errorf("editText = %q, want empty for nil callback", editText)
	}
}

// TestCleanupExpiredInteractive verifies that expired entries (older than 24h)
// are removed, while recent entries are preserved.
func TestCleanupExpiredInteractive(t *testing.T) {
	clearIMStore(t)

	// Manually insert an expired and a fresh entry.
	imMu.Lock()
	imStore["old"] = &interactiveMsg{
		buttons:  []ButtonChoice{{Label: "A"}},
		callback: nil,
		created:  time.Now().Add(-25 * time.Hour),
	}
	imStore["new"] = &interactiveMsg{
		buttons:  []ButtonChoice{{Label: "B"}},
		callback: nil,
		created:  time.Now(),
	}
	imMu.Unlock()

	CleanupExpiredInteractive(24 * time.Hour)

	imMu.Lock()
	_, hasOld := imStore["old"]
	_, hasNew := imStore["new"]
	imMu.Unlock()

	if hasOld {
		t.Error("expired entry 'old' should have been removed")
	}
	if !hasNew {
		t.Error("fresh entry 'new' should have been preserved")
	}
}

// TestCleanupExpiredInteractiveResolves verifies that expiring a prompt invokes
// its onExpire resolver (so an upstream waiter is denied/cancelled rather than
// orphaned) and edits the message to the expired notice.
func TestCleanupExpiredInteractiveResolves(t *testing.T) {
	clearIMStore(t)

	bs := &mockButtonSender{msgID: "m1"}
	resolved := false
	imMu.Lock()
	imStore["old"] = &interactiveMsg{
		resolve:  staticResolver(bs),
		msgID:    "m1",
		buttons:  []ButtonChoice{{Label: "Allow", Data: "allow"}, {Label: "Deny", Data: "deny"}},
		onExpire: func() { resolved = true },
		created:  time.Now().Add(-25 * time.Hour),
	}
	imMu.Unlock()

	CleanupExpiredInteractive(24 * time.Hour)

	if !resolved {
		t.Error("onExpire should have been invoked to resolve the orphaned prompt")
	}
	if bs.editedMsgID != "m1" || bs.editedText != expiredInteractiveText {
		t.Errorf("edited msgID=%q text=%q, want m1 / %q", bs.editedMsgID, bs.editedText, expiredInteractiveText)
	}
	imMu.Lock()
	_, hasOld := imStore["old"]
	imMu.Unlock()
	if hasOld {
		t.Error("expired entry should have been removed from the store")
	}
}

// TestCleanupExpiredInteractiveEmpty verifies that cleanup on an empty store
// does not panic.
func TestCleanupExpiredInteractiveEmpty(t *testing.T) {
	clearIMStore(t)
	CleanupExpiredInteractive(24 * time.Hour) // should not panic
}

// TestConcurrentSendAndHandle verifies that concurrent goroutines calling
// SendInteractiveMessage and HandleInteractiveCallback do not race or panic.
func TestConcurrentSendAndHandle(t *testing.T) {
	clearIMStore(t)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	// Each goroutine sends a message, then handles its own callback.
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()

			bs := &mockButtonSender{msgID: strconv.Itoa(n)}
			buttons := []ButtonChoice{{Label: fmt.Sprintf("btn-%d", n), Data: fmt.Sprintf("d-%d", n)}}
			id := fmt.Sprintf("conc-%d", n)
			_, err := SendInteractiveMessageWithID(staticResolver(bs), id, "msg", buttons, func(choice ButtonChoice) string {
				return "ok-" + choice.Data
			}, nil)
			if err != nil {
				t.Errorf("goroutine %d: SendInteractiveMessage: %v", n, err)
				return
			}

			data := bs.buttons[0].Data
			editText, choiceData, ok := HandleInteractiveCallback(data)
			if !ok {
				t.Errorf("goroutine %d: callback not found", n)
				return
			}
			wantEdit := fmt.Sprintf("ok-d-%d", n)
			if editText != wantEdit {
				t.Errorf("goroutine %d: editText = %q, want %q", n, editText, wantEdit)
			}
			wantData := fmt.Sprintf("d-%d", n)
			if choiceData != wantData {
				t.Errorf("goroutine %d: choiceData = %q, want %q", n, choiceData, wantData)
			}
		}(i)
	}

	wg.Wait()
}

// --- Helpers ---

// staticResolver wraps a fixed connection as a ConnResolver, for tests where the
// connection never changes. Production resolvers re-query the connection manager.
func staticResolver(c Connection) ConnResolver {
	return func() Connection { return c }
}

// clearIMStore empties the global interactive message store so tests are
// independent. The counter is NOT reset because nextPromptID relies on
// global monotonicity.
func clearIMStore(t *testing.T) {
	t.Helper()
	imMu.Lock()
	defer imMu.Unlock()
	for k := range imStore {
		delete(imStore, k)
	}
}

func imStoreSize() int {
	imMu.Lock()
	defer imMu.Unlock()
	return len(imStore)
}

// ---------------------------------------------------------------------------
// SendInteractiveMessageWithID & CancelInteractiveMessage
// ---------------------------------------------------------------------------

// TestSendInteractiveMessageWithIDUsesGivenID verifies that the caller-supplied
// ID is used as the prompt ID — button data prefixes match it, and the entry
// is keyed by it in the store. Lets the caller round-trip its own identifier
// (e.g. a CC requestID) without an extra mapping table.
func TestSendInteractiveMessageWithIDUsesGivenID(t *testing.T) {
	clearIMStore(t)

	bs := &mockButtonSender{msgID: "42"}
	buttons := []ButtonChoice{{Label: "Allow", Data: "allow"}}

	_, err := SendInteractiveMessageWithID(staticResolver(bs), "req-abc-123", "Permit?", buttons, func(ButtonChoice) string { return "" }, nil)
	if err != nil {
		t.Fatalf("SendInteractiveMessageWithID: %v", err)
	}

	if !strings.HasPrefix(bs.buttons[0].Data, "req-abc-123:") {
		t.Errorf("button data = %q, want prefix req-abc-123:", bs.buttons[0].Data)
	}

	imMu.Lock()
	_, found := imStore["req-abc-123"]
	imMu.Unlock()
	if !found {
		t.Error("store missing entry under caller-supplied id")
	}
}

// TestCancelInteractiveMessageEditsAndRemoves verifies the happy-path cancel:
// EditMessageText is called with the stored msgID and the supplied final
// text, and the store entry is removed (so subsequent clicks become no-ops).
func TestCancelInteractiveMessageEditsAndRemoves(t *testing.T) {
	clearIMStore(t)

	bs := &mockButtonSender{msgID: "777"}
	buttons := []ButtonChoice{{Label: "Allow", Data: "allow"}}
	_, _ = SendInteractiveMessageWithID(staticResolver(bs), "req-X", "Permit?", buttons, func(ButtonChoice) string { return "" }, nil)

	if err := CancelInteractiveMessage("req-X", "❌ cancelled"); err != nil {
		t.Fatalf("CancelInteractiveMessage: %v", err)
	}

	if bs.editedMsgID != "777" {
		t.Errorf("EditMessageText msgID = %q, want 777", bs.editedMsgID)
	}
	if bs.editedText != "❌ cancelled" {
		t.Errorf("EditMessageText text = %q, want '❌ cancelled'", bs.editedText)
	}

	// Entry should be gone — subsequent click is no-op.
	imMu.Lock()
	_, found := imStore["req-X"]
	imMu.Unlock()
	if found {
		t.Error("store entry should be removed after cancel")
	}
}

// TestCancelInteractiveMessageRaceWithCallback verifies that calling Cancel
// after the callback already fired (which deletes the entry) is a benign
// no-op — no second edit, no error.
func TestCancelInteractiveMessageRaceWithCallback(t *testing.T) {
	clearIMStore(t)

	bs := &mockButtonSender{msgID: "55"}
	buttons := []ButtonChoice{{Label: "OK", Data: "ok"}}
	_, _ = SendInteractiveMessageWithID(staticResolver(bs), "req-race", "Pick?", buttons, func(ButtonChoice) string { return "done" }, nil)

	// User clicks first — HandleInteractiveCallback removes the entry.
	if _, _, ok := HandleInteractiveCallback("req-race:0"); !ok {
		t.Fatal("expected callback to succeed")
	}

	// Reset edit-capture so we can detect any second edit.
	bs.editedMsgID = ""
	bs.editedText = ""

	// Cancel arrives after click — should be a no-op.
	if err := CancelInteractiveMessage("req-race", "shouldn't appear"); err != nil {
		t.Fatalf("CancelInteractiveMessage: %v", err)
	}

	if bs.editedMsgID != "" || bs.editedText != "" {
		t.Errorf("Cancel after click triggered second edit: msgID=%q text=%q", bs.editedMsgID, bs.editedText)
	}
}

// TestCancelInteractiveMessageUnknownID verifies that cancelling an id that
// was never stored is a benign no-op (no edit, no error).
func TestCancelInteractiveMessageUnknownID(t *testing.T) {
	clearIMStore(t)

	if err := CancelInteractiveMessage("never-existed", "x"); err != nil {
		t.Errorf("CancelInteractiveMessage(unknown) = %v, want nil", err)
	}
}

// --- Mocks ---

// mockButtonSender implements Connection (via embedding mockConnection) and
// ButtonSender for testing the full interactive message flow.
type mockButtonSender struct {
	mockConnection
	msgID       string
	err         error
	text        string
	buttons     []ButtonChoice
	prefix      string
	editedMsgID string // captured by EditMessageText
	editedText  string // captured by EditMessageText
	editErr     error  // returned from EditMessageText

	ewbText    string         // captured by EditMessageWithButtons
	ewbButtons []ButtonChoice // captured by EditMessageWithButtons
	ewbCalls   int
}

func (m *mockButtonSender) SendTextWithButtons(text string, buttons []ButtonChoice, prefix string) (string, error) {
	m.text = text
	m.buttons = buttons
	m.prefix = prefix
	return m.msgID, m.err
}

func (m *mockButtonSender) EditMessageText(msgID string, text string) error {
	m.editedMsgID = msgID
	m.editedText = text
	return m.editErr
}
func (m *mockButtonSender) EditMessageWithButtons(msgID, text string, buttons []ButtonChoice, _ string) error {
	m.ewbText = text
	m.ewbButtons = buttons
	m.ewbCalls++
	return nil
}

// Compile-time verification.
var _ ButtonSender = (*mockButtonSender)(nil)

// mockConnectionCapture embeds mockConnection but captures text sent via
// SendText — used to test the fallback path where no ButtonSender exists.
type mockConnectionCapture struct {
	mockConnection
	lastText string
}

func (m *mockConnectionCapture) SendText(text string) error {
	m.lastText = text
	return nil
}

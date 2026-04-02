package platform

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNextPromptID verifies that nextPromptID generates unique, monotonically
// increasing IDs across sequential and concurrent calls.
func TestNextPromptID(t *testing.T) {
	// Record the starting counter so the test is independent of run order.
	start := atomic.LoadUint64(&imCounter)

	id1 := nextPromptID()
	id2 := nextPromptID()

	if id1 == id2 {
		t.Errorf("expected unique IDs, got %q twice", id1)
	}

	// IDs should decode to sequential values.
	n1, err1 := strconv.ParseUint(id1, 36, 64)
	n2, err2 := strconv.ParseUint(id2, 36, 64)
	if err1 != nil || err2 != nil {
		t.Fatalf("IDs should be base-36: id1=%q err=%v, id2=%q err=%v", id1, err1, id2, err2)
	}
	if n1 != start+1 || n2 != start+2 {
		t.Errorf("expected sequential values %d,%d but got %d,%d", start+1, start+2, n1, n2)
	}
}

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
	err := SendInteractiveMessage(bs, "Choose:", buttons, func(choice ButtonChoice) string {
		cbCalled = true
		return "You chose: " + choice.Label
	})
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

	err := SendInteractiveMessage(mc, "Pick a fruit:", buttons, nil)
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

	err := SendInteractiveMessage(bs, "text", buttons, func(ButtonChoice) string { return "" })
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
	_ = SendInteractiveMessage(bs, "text", buttons, func(ButtonChoice) string { return "done" })

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
	_ = SendInteractiveMessage(bs, "text", buttons, func(ButtonChoice) string { return "" })

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
	_ = SendInteractiveMessage(bs, "text", buttons, nil)

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

	CleanupExpiredInteractive()

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

// TestCleanupExpiredInteractiveEmpty verifies that cleanup on an empty store
// does not panic.
func TestCleanupExpiredInteractiveEmpty(t *testing.T) {
	clearIMStore(t)
	CleanupExpiredInteractive() // should not panic
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
			err := SendInteractiveMessage(bs, "msg", buttons, func(choice ButtonChoice) string {
				return "ok-" + choice.Data
			})
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

// --- Mocks ---

// mockButtonSender implements Connection (via embedding mockConnection) and
// ButtonSender for testing the full interactive message flow.
type mockButtonSender struct {
	mockConnection
	msgID   string
	err     error
	text    string
	buttons []ButtonChoice
	prefix  string
}

func (m *mockButtonSender) SendTextWithButtons(text string, buttons []ButtonChoice, prefix string) (string, error) {
	m.text = text
	m.buttons = buttons
	m.prefix = prefix
	return m.msgID, m.err
}

func (m *mockButtonSender) EditMessageText(string, string) error                              { return nil }
func (m *mockButtonSender) EditMessageWithButtons(string, string, []ButtonChoice, string) error { return nil }

// Compile-time verification.
var _ ButtonSender = (*mockButtonSender)(nil)

// mockConnectionCapture embeds mockConnection but captures text sent via
// RawSendText — used to test the fallback path where no ButtonSender exists.
type mockConnectionCapture struct {
	mockConnection
	lastText string
}

func (m *mockConnectionCapture) RawSendText(text string) error {
	m.lastText = text
	return nil
}

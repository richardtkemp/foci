package tools

import "testing"

func TestAsyncNotifierDelivers(t *testing.T) {
	var got string
	n := NewAsyncNotifier(func(msg string) {
		got = msg
	})
	n.Notify("hello")
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestAsyncNotifierNilReceiver(t *testing.T) {
	var n *AsyncNotifier
	n.Notify("should not panic") // must not panic
}

func TestAsyncNotifierNilFunc(t *testing.T) {
	n := &AsyncNotifier{}
	n.Notify("should not panic") // must not panic
}

package tools

import "testing"

func TestAsyncNotifierDelivers(t *testing.T) {
	var gotKey, gotMsg string
	n := NewAsyncNotifier(func(sk, msg string) {
		gotKey = sk
		gotMsg = msg
	})
	n.Notify("sess-1", "hello")
	if gotKey != "sess-1" {
		t.Errorf("key = %q, want %q", gotKey, "sess-1")
	}
	if gotMsg != "hello" {
		t.Errorf("msg = %q, want %q", gotMsg, "hello")
	}
}

func TestAsyncNotifierNilReceiver(t *testing.T) {
	var n *AsyncNotifier
	n.Notify("sess", "should not panic") // must not panic
}

func TestAsyncNotifierNilFunc(t *testing.T) {
	n := &AsyncNotifier{}
	n.Notify("sess", "should not panic") // must not panic
}

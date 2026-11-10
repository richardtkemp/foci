package voice

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/voice/fluxfake"
)

// collect reads a stream's events until it ends, with a hang guard.
func collect(t *testing.T, st STTStream) []StreamEvent {
	t.Helper()
	var out []StreamEvent
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-st.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatal("stream did not end")
		}
	}
}

// sendAudio sends n bytes of silence-shaped audio in 80 ms chunks (2560
// bytes at 16 kHz mono PCM16: the chunk size Flux's quickstart recommends).
func sendAudio(t *testing.T, st STTStream, n int) {
	t.Helper()
	chunk := make([]byte, 2560)
	for sent := 0; sent < n; sent += len(chunk) {
		if err := st.Send(chunk); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
}

func TestFluxStreamReportsTurnThenEndOfTurn(t *testing.T) {
	fake := fluxfake.New("k1")
	defer fake.Close()
	fake.Words = []string{"hello", "foca", "there"}
	fake.BytesPerWord = 2560

	f := &FluxSTT{Endpoint: fake.URL(), APIKey: "k1"}
	st, err := f.OpenStream(context.Background(), StreamOptions{EOTThreshold: 0.8, EOTTimeoutMs: 3000})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sendAudio(t, st, 3*2560)

	var got []StreamEvent
	for len(got) == 0 || got[len(got)-1].Kind != StreamEndOfTurn {
		select {
		case ev, ok := <-st.Events():
			if !ok {
				t.Fatalf("stream ended early: %v (events %v)", st.Err(), got)
			}
			got = append(got, ev)
		case <-time.After(10 * time.Second):
			t.Fatalf("no end of turn; events %v", got)
		}
	}
	want := []StreamEvent{
		{StreamPartial, "hello"},
		{StreamPartial, "hello foca"},
		{StreamPartial, "hello foca there"},
		{StreamEndOfTurn, "hello foca there"},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	collect(t, st)
	if st.Err() != nil {
		t.Errorf("Err after clean close = %v, want nil", st.Err())
	}
	if !fake.WaitConnEnd(5 * time.Second) {
		t.Fatal("upstream connection still open after Close: billing would continue")
	}
	if !fake.GotCloseStream() {
		t.Error("Close did not send CloseStream")
	}

	q := fake.Queries()[0]
	for k, v := range map[string]string{
		"model": "flux-general-en", "encoding": "linear16", "sample_rate": "16000",
		"eot_threshold": "0.8", "eot_timeout_ms": "3000",
	} {
		if q.Get(k) != v {
			t.Errorf("query %s = %q, want %q", k, q.Get(k), v)
		}
	}
}

func TestFluxStreamLeavesUnsetThresholdsToFlux(t *testing.T) {
	fake := fluxfake.New("k1")
	defer fake.Close()
	f := &FluxSTT{Endpoint: fake.URL(), APIKey: "k1", Model: "flux-general-multi"}
	st, err := f.OpenStream(context.Background(), StreamOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = st.Close()
	collect(t, st)
	q := fake.Queries()[0]
	if q.Has("eot_threshold") || q.Has("eot_timeout_ms") {
		t.Errorf("zero options sent thresholds: %v", q)
	}
	if q.Get("model") != "flux-general-multi" {
		t.Errorf("model = %q", q.Get("model"))
	}
}

func TestFluxStreamBadKeyFailsOpen(t *testing.T) {
	fake := fluxfake.New("right")
	defer fake.Close()
	f := &FluxSTT{Endpoint: fake.URL(), APIKey: "wrong"}
	_, err := f.OpenStream(context.Background(), StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("open with a bad key: err = %v, want an HTTP 401 failure", err)
	}
}

func TestFluxStreamOutOfRangeThresholdFailsOpen(t *testing.T) {
	fake := fluxfake.New("k")
	defer fake.Close()
	f := &FluxSTT{Endpoint: fake.URL(), APIKey: "k"}
	_, err := f.OpenStream(context.Background(), StreamOptions{EOTThreshold: 0.2})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("open with eot_threshold 0.2: err = %v, want an HTTP 400 failure", err)
	}
}

func TestFluxStreamFatalErrorEndsStream(t *testing.T) {
	fake := fluxfake.New("k")
	defer fake.Close()
	fake.FatalAfterBytes = 2560
	f := &FluxSTT{Endpoint: fake.URL(), APIKey: "k"}
	st, err := f.OpenStream(context.Background(), StreamOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sendAudio(t, st, 2560)
	collect(t, st)
	if st.Err() == nil || !strings.Contains(st.Err().Error(), "INTERNAL_SERVER_ERROR") {
		t.Fatalf("Err = %v, want the Flux error code", st.Err())
	}
	if err := st.Send([]byte{0, 0}); err != ErrStreamClosed {
		t.Errorf("Send after the stream ended = %v, want ErrStreamClosed", err)
	}
}

func TestFluxStreamContextEndClosesUpstream(t *testing.T) {
	fake := fluxfake.New("k")
	defer fake.Close()
	ctx, cancel := context.WithCancel(context.Background())
	f := &FluxSTT{Endpoint: fake.URL(), APIKey: "k"}
	st, err := f.OpenStream(ctx, StreamOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	cancel()
	collect(t, st)
	if !fake.WaitConnEnd(5 * time.Second) {
		t.Fatal("upstream still open after the context ended")
	}
}

func TestWrapStreamingSTTAppliesReplacements(t *testing.T) {
	fake := fluxfake.New("k")
	defer fake.Close()
	fake.Words = []string{"ask", "foca"}
	fake.BytesPerWord = 2560
	s := WrapStreamingSTT(&FluxSTT{Endpoint: fake.URL(), APIKey: "k"}, map[string]string{"foca": "foci"})
	st, err := s.OpenStream(context.Background(), StreamOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sendAudio(t, st, 2*2560)
	var last StreamEvent
	for last.Kind != StreamEndOfTurn {
		select {
		case ev, ok := <-st.Events():
			if !ok {
				t.Fatalf("ended early: %v", st.Err())
			}
			last = ev
		case <-time.After(10 * time.Second):
			t.Fatal("no end of turn")
		}
	}
	if last.Text != "ask foci" {
		t.Errorf("end of turn = %q, want replacements applied (\"ask foci\")", last.Text)
	}
	_ = st.Close()
	collect(t, st)
}

func TestNewStreamingSTT(t *testing.T) {
	if _, ok, _ := NewStreamingSTT("openai", "", "k", ""); ok {
		t.Error("openai reported as a streaming format")
	}
	if _, ok, err := NewStreamingSTT("deepgram-flux", "", "", ""); !ok || err == nil {
		t.Errorf("flux with no key: ok=%v err=%v, want ok and an error", ok, err)
	}
	s, ok, err := NewStreamingSTT("deepgram-flux", "", "k", "")
	if !ok || err != nil || s == nil {
		t.Fatalf("flux: ok=%v err=%v", ok, err)
	}
	u, _ := s.(*FluxSTT).fluxURL(StreamOptions{})
	if !strings.HasPrefix(u, "wss://api.deepgram.com/v2/listen?") {
		t.Errorf("default URL = %q", u)
	}
}

func TestFluxStreamFinishEndsTurnNow(t *testing.T) {
	fake := fluxfake.New("k")
	defer fake.Close()
	fake.Words = []string{"one", "two", "three"}
	fake.BytesPerWord = 2560
	st, err := (&FluxSTT{Endpoint: fake.URL(), APIKey: "k"}).OpenStream(context.Background(), StreamOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close(); collect(t, st) }()
	sendAudio(t, st, 2560)
	if ev := <-st.Events(); ev != (StreamEvent{StreamPartial, "one"}) {
		t.Fatalf("first event = %+v", ev)
	}
	if err := st.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	select {
	case ev := <-st.Events():
		if ev != (StreamEvent{StreamEndOfTurn, "one"}) {
			t.Fatalf("after finish = %+v, want end of turn \"one\"", ev)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no end of turn after Finish")
	}
}

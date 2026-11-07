package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foci/internal/config"
)

// sinePCM16 returns secs seconds of a mono 16-bit sine at freq Hz.
func sinePCM16(sampleRate int, freq, secs float64) []byte {
	n := int(float64(sampleRate) * secs)
	buf := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		v := int16(12000 * math.Sin(2*math.Pi*freq*float64(i)/float64(sampleRate)))
		binary.LittleEndian.PutUint16(buf[2*i:], uint16(v))
	}
	return buf
}

// groqStyleWAV wraps pcm the way Groq's speech endpoint does (seen live,
// 2026-11-07): RIFF and data sizes set to 0xFFFFFFFF (a streamed header) and a
// LIST chunk between "fmt " and "data".
func groqStyleWAV(pcm []byte, sampleRate int) []byte {
	var b bytes.Buffer
	put32 := func(v uint32) { _ = binary.Write(&b, binary.LittleEndian, v) }
	put16 := func(v uint16) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	put32(0xFFFFFFFF)
	b.WriteString("WAVEfmt ")
	put32(16)
	put16(1)
	put16(1)
	put32(uint32(sampleRate))
	put32(uint32(sampleRate * 2))
	put16(2)
	put16(16)
	b.WriteString("LIST")
	put32(26)
	b.WriteString("INFOISFT")
	put32(14)
	b.WriteString("Lavf61.7.100\x00\x00")
	b.WriteString("data")
	put32(0xFFFFFFFF)
	b.Write(pcm)
	return b.Bytes()
}

// zeroCrossingsPerSec counts sign changes in mono 16-bit pcm per second.
func zeroCrossingsPerSec(pcm []byte, sampleRate int) float64 {
	n := len(pcm) / 2
	crossings := 0
	prev := int16(binary.LittleEndian.Uint16(pcm[0:]))
	for i := 1; i < n; i++ {
		cur := int16(binary.LittleEndian.Uint16(pcm[2*i:]))
		if (prev < 0) != (cur < 0) {
			crossings++
		}
		prev = cur
	}
	return float64(crossings) / (float64(n) / float64(sampleRate))
}

func TestStretchWAV_ChangesDurationKeepsPitch(t *testing.T) {
	// Proves the local time stretch shortens (rate>1) or lengthens (rate<1) a
	// clip by the rate while the pitch stays the same: a plain resample would
	// also move the frequency by the rate.
	const sr = 24000
	in := groqStyleWAV(sinePCM16(sr, 440, 3), sr)
	inZC := zeroCrossingsPerSec(sinePCM16(sr, 440, 3), sr)
	for _, rate := range []float64{0.8, 1.3, 2.0} {
		out, err := stretchWAV(in, rate)
		if err != nil {
			t.Fatalf("rate %v: %v", rate, err)
		}
		pcm, gotSR, ch, err := parsePCM16WAV(out)
		if err != nil {
			t.Fatalf("rate %v: output not a PCM16 WAV: %v", rate, err)
		}
		if gotSR != sr || ch != 1 {
			t.Fatalf("rate %v: format changed: sr=%d ch=%d", rate, gotSR, ch)
		}
		gotSecs := float64(len(pcm)/2) / sr
		wantSecs := 3 / rate
		if math.Abs(gotSecs-wantSecs)/wantSecs > 0.03 {
			t.Errorf("rate %v: duration %.3fs, want %.3fs ±3%%", rate, gotSecs, wantSecs)
		}
		zc := zeroCrossingsPerSec(pcm, sr)
		if math.Abs(zc-inZC)/inZC > 0.03 {
			t.Errorf("rate %v: pitch moved: %.0f zero crossings/s, input %.0f", rate, zc, inZC)
		}
	}
}

func TestStretchWAV_RejectsNonPCM16(t *testing.T) {
	// Proves a body that is not 16-bit PCM WAV (e.g. an MP3) is refused, so the
	// caller can keep the original audio rather than emit noise.
	if _, err := stretchWAV([]byte("ID3\x04\x00 not a wav"), 1.3); err == nil {
		t.Fatal("expected an error for a non-WAV body")
	}
}

func TestOpenAITTS_WAVRateAppliedLocally(t *testing.T) {
	// #2937: Groq's Orpheus endpoint accepts "speed" (it validates <= 5.0) but
	// returns byte-identical audio at every value (live test, 2026-11-07). So
	// for a voice marked LocalSpeed (#2940: [[tts]] local_speed_control) foci
	// applies the rate itself and does not send speed: the clip must come back
	// shorter by the rate.
	const sr = 24000
	pcm := sinePCM16(sr, 300, 2)
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_, _ = w.Write(groqStyleWAV(pcm, sr))
	}))
	defer server.Close()

	tts := &OpenAITTS{Endpoint: server.URL, APIKey: "key", Model: "m", Voice: "troy", Speed: 1.3, LocalSpeed: true}
	audio, err := tts.Synthesize(context.Background(), "hello")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if strings.Contains(gotBody, "speed") {
		t.Errorf("speed sent to the API as well as applied locally (double rate on a provider that honours it): %s", gotBody)
	}
	out, _, _, err := parsePCM16WAV(audio)
	if err != nil {
		t.Fatalf("output not a PCM16 WAV: %v", err)
	}
	gotSecs := float64(len(out)/2) / sr
	if want := 2 / 1.3; math.Abs(gotSecs-want)/want > 0.03 {
		t.Errorf("duration %.3fs, want %.3fs (rate 1.3 not applied)", gotSecs, want)
	}
}

func TestOpenAITTS_WAVRateOneUntouched(t *testing.T) {
	// Proves a clip with no rate (Speed 0) is returned byte for byte as the
	// provider sent it: no decode, no re-encode.
	const sr = 24000
	body := groqStyleWAV(sinePCM16(sr, 300, 1), sr)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()

	tts := &OpenAITTS{Endpoint: server.URL, APIKey: "key", Model: "m"}
	audio, err := tts.Synthesize(context.Background(), "hello")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if !bytes.Equal(audio, body) {
		t.Error("audio changed with no rate set")
	}
}

func TestOpenAITTS_WAVRateUndecodableBodyKept(t *testing.T) {
	// Proves that when a WAV response cannot be decoded as PCM16, the reply
	// still plays: the provider's bytes come back unchanged, not an error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not a wav"))
	}))
	defer server.Close()

	tts := &OpenAITTS{Endpoint: server.URL, APIKey: "key", Model: "m", Speed: 1.3, LocalSpeed: true}
	audio, err := tts.Synthesize(context.Background(), "hello")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if string(audio) != "not a wav" {
		t.Errorf("audio = %q, want the provider's bytes", audio)
	}
}

func TestOpenAITTS_WAVRateSentToAPIByDefault(t *testing.T) {
	// #2940, Dick's ruling: the endpoint's own speed is the rule, the local
	// stretch the exception. A WAV voice without LocalSpeed sends speed and
	// gets the provider's bytes back unchanged.
	const sr = 24000
	body := groqStyleWAV(sinePCM16(sr, 300, 1), sr)
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	tts := &OpenAITTS{Endpoint: server.URL, APIKey: "key", Model: "m", Speed: 1.3}
	audio, err := tts.Synthesize(context.Background(), "hello")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if !strings.Contains(gotBody, `"speed":1.3`) {
		t.Errorf("speed not sent to the API: %s", gotBody)
	}
	if !bytes.Equal(audio, body) {
		t.Error("audio changed locally on a voice without local speed control")
	}
}

func TestNewTTS_LocalSpeedControlCarried(t *testing.T) {
	// #2940: the [[tts]] key reaches the provider, and survives WithRate (which
	// is how every caller applies the rate).
	for _, want := range []bool{false, true} {
		cfg := config.TTSConfig{Format: "openai", Endpoint: "http://x", LocalSpeedControl: want}
		p, err := NewTTS(cfg, "k", HTTPOpts{})
		if err != nil {
			t.Fatal(err)
		}
		o, ok := WithRate(p, 1.3).(*OpenAITTS)
		if !ok {
			t.Fatalf("WithRate returned %T, want *OpenAITTS", WithRate(p, 1.3))
		}
		if o.LocalSpeed != want || o.Speed != 1.3 {
			t.Errorf("LocalSpeed=%v Speed=%v, want %v and 1.3", o.LocalSpeed, o.Speed, want)
		}
	}
}

package voice

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withFFmpeg points ffmpegBinary at bin for one test.
func withFFmpeg(t *testing.T, bin string) {
	t.Helper()
	old := ffmpegBinary
	ffmpegBinary = bin
	t.Cleanup(func() { ffmpegBinary = old })
}

// fakeFFmpeg writes an executable script standing in for ffmpeg. It records its
// arguments in a file next to it and runs body.
func fakeFFmpeg(t *testing.T, body string) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "ffmpeg")
	argsFile = filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
	return bin, argsFile
}

// wavSecs is the duration of a PCM16 WAV.
func wavSecs(t *testing.T, wav []byte) float64 {
	t.Helper()
	pcm, sr, ch, err := parsePCM16WAV(wav)
	if err != nil {
		t.Fatalf("not a PCM16 WAV: %v", err)
	}
	return float64(len(pcm)/(2*ch)) / float64(sr)
}

func TestAtempoChain(t *testing.T) {
	for rate, want := range map[float64]string{
		1.3:  "atempo=1.3",
		0.8:  "atempo=0.8",
		2:    "atempo=2",
		3:    "atempo=2,atempo=1.5",
		0.25: "atempo=0.5,atempo=0.5",
	} {
		if got := atempoChain(rate); got != want {
			t.Errorf("atempoChain(%v) = %q, want %q", rate, got, want)
		}
	}
}

// #2943: with ffmpeg present, the stretch is ffmpeg's atempo on raw PCM. The
// fake ffmpeg passes its input through unchanged, so a clip that keeps its
// length proves the built-in stretch did not run.
func TestStretchLocal_UsesFFmpegWhenPresent(t *testing.T) {
	bin, argsFile := fakeFFmpeg(t, "exec /bin/cat")
	withFFmpeg(t, bin)
	const sr = 24000
	pcm := sinePCM16(sr, 300, 1)

	out, err := stretchLocal(context.Background(), groqStyleWAV(pcm, sr), 1.3)
	if err != nil {
		t.Fatalf("stretchLocal: %v", err)
	}
	if !bytes.Equal(out, wrapPCMInWAV(pcm, sr, 1, 16)) {
		t.Errorf("output is not ffmpeg's: %.3fs, want the 1.000s the pass-through fake returns", wavSecs(t, out))
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ffmpeg was not run: %v", err)
	}
	for _, want := range []string{"atempo=1.3", "s16le", "24000", "pipe:0", "pipe:1"} {
		if !strings.Contains(string(args), want+"\n") {
			t.Errorf("ffmpeg args lack %q:\n%s", want, args)
		}
	}
}

// #2943: no ffmpeg, or an ffmpeg that fails (no atempo filter), is degraded
// quality, not a failure: the built-in stretch applies the rate.
func TestStretchLocal_FallsBackToBuiltin(t *testing.T) {
	failing, _ := fakeFFmpeg(t, "echo \"No such filter: 'atempo'\" >&2; exit 1")
	for name, bin := range map[string]string{
		"missing": filepath.Join(t.TempDir(), "ffmpeg"),
		"fails":   failing,
		"empty":   func() string { b, _ := fakeFFmpeg(t, "exit 0"); return b }(),
	} {
		t.Run(name, func(t *testing.T) {
			withFFmpeg(t, bin)
			const sr = 24000
			out, err := stretchLocal(context.Background(), groqStyleWAV(sinePCM16(sr, 300, 2), sr), 1.3)
			if err != nil {
				t.Fatalf("stretchLocal: %v", err)
			}
			if got, want := wavSecs(t, out), 2/1.3; math.Abs(got-want)/want > 0.03 {
				t.Errorf("duration %.3fs, want %.3fs from the built-in stretch", got, want)
			}
		})
	}
}

func TestStretchLocal_NotPCM16IsAnError(t *testing.T) {
	bin, argsFile := fakeFFmpeg(t, "exec /bin/cat")
	withFFmpeg(t, bin)
	if _, err := stretchLocal(context.Background(), []byte("not a wav"), 1.3); err == nil {
		t.Fatal("a body that is not PCM16 WAV must be an error, so the caller keeps it")
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Error("ffmpeg ran on a body that is not PCM16 WAV")
	}
}

// The real ffmpeg on this host, when there is one: atempo changes the length
// by the rate. Skipped where ffmpeg is not on the Trusted PATH.
func TestStretchLocal_RealFFmpeg(t *testing.T) {
	if !FFmpegAvailable() {
		t.Skip("no ffmpeg on the Trusted PATH")
	}
	const sr = 24000
	pcm, _, _, err := parsePCM16WAV(groqStyleWAV(sinePCM16(sr, 300, 2), sr))
	if err != nil {
		t.Fatal(err)
	}
	out, err := ffmpegStretchPCM(context.Background(), pcm, sr, 1, 1.3)
	if err != nil {
		t.Fatalf("ffmpeg atempo: %v", err)
	}
	if got, want := float64(len(out)/2)/sr, 2/1.3; math.Abs(got-want)/want > 0.03 {
		t.Errorf("duration %.3fs, want %.3fs", got, want)
	}
}

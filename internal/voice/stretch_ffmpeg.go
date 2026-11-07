package voice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"foci/internal/procx"
)

// Local speed control prefers ffmpeg's atempo filter (#2943). Dick listened to
// Groq speech at rate 1.3 stretched each way and ruled atempo the default
// (2026-11-07: "Atempo is perfect"; rubberband "sounds awful" and is not
// offered). atempo is WSOLA too, but better tuned than the pure-Go stretch in
// stretch.go, which stays as the fallback when ffmpeg is missing or fails.

// ffmpegBinary is the command run for atempo, resolved against the Trusted
// population's PATH. A test seam.
var ffmpegBinary = "ffmpeg"

// atempo accepts 0.5–2.0 in every ffmpeg version (newer ones accept up to 100).
const (
	atempoMin = 0.5
	atempoMax = 2.0
)

// atempoChain renders rate as a chain of atempo filters, each within the range
// every ffmpeg accepts; their product is rate.
func atempoChain(rate float64) string {
	var parts []string
	for rate > atempoMax {
		parts = append(parts, "atempo="+strconv.FormatFloat(atempoMax, 'f', -1, 64))
		rate /= atempoMax
	}
	for rate < atempoMin {
		parts = append(parts, "atempo="+strconv.FormatFloat(atempoMin, 'f', -1, 64))
		rate /= atempoMin
	}
	parts = append(parts, "atempo="+strconv.FormatFloat(rate, 'f', -1, 64))
	return strings.Join(parts, ",")
}

// FFmpegAvailable reports whether the ffmpeg for local speed control is on the
// Trusted PATH. It does not check for the atempo filter: a stretch that fails
// for that reason falls back and warns.
func FFmpegAvailable() bool {
	_, err := procx.LookPath(procx.Trusted, ffmpegBinary)
	return err == nil
}

// ffmpegStretchPCM returns interleaved 16-bit PCM played rate times faster, at
// the same pitch, through ffmpeg's atempo filter. Raw PCM goes in and comes out
// on pipes, so no WAV header (Groq's streamed one included) reaches ffmpeg.
func ffmpegStretchPCM(ctx context.Context, pcm []byte, sampleRate, channels int, rate float64) ([]byte, error) {
	path, err := procx.LookPath(procx.Trusted, ffmpegBinary)
	if err != nil {
		return nil, fmt.Errorf("%s not on PATH", ffmpegBinary)
	}
	sr, ch := strconv.Itoa(sampleRate), strconv.Itoa(channels)
	// Trusted: foci's own TTS pipeline, like edge-tts.
	cmd := procx.Spawn(ctx, procx.Trusted, path,
		"-hide_banner", "-loglevel", "error",
		"-f", "s16le", "-ar", sr, "-ac", ch, "-i", "pipe:0",
		"-filter:a", atempoChain(rate),
		"-f", "s16le", "-acodec", "pcm_s16le", "-ar", sr, "-ac", ch, "pipe:1")
	var out, stderr bytes.Buffer
	cmd.Stdin = bytes.NewReader(pcm)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg atempo: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	frame := 2 * channels
	if out.Len() == 0 || out.Len()%frame != 0 {
		return nil, fmt.Errorf("ffmpeg atempo: %d bytes out, not whole %d-byte frames", out.Len(), frame)
	}
	return out.Bytes(), nil
}

// stretchLocal returns wav played rate times faster, at the same pitch, as a
// plain PCM16 WAV: through ffmpeg atempo when it works, else the built-in WSOLA
// stretch, with a warning each time (degraded quality, not a failure). A body
// that is not 16-bit PCM WAV is an error, so the caller can keep the original.
func stretchLocal(ctx context.Context, wav []byte, rate float64) ([]byte, error) {
	if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return nil, fmt.Errorf("invalid rate %v", rate)
	}
	pcm, sr, ch, err := parsePCM16WAV(wav)
	if err != nil {
		return nil, err
	}
	out, ferr := ffmpegStretchPCM(ctx, pcm, sr, ch, rate)
	if ferr == nil {
		return wrapPCMInWAV(out, sr, ch, 16), nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return nil, errors.Join(cerr, ferr)
	}
	voiceLog.Warnf("tts local speed control: %v; using the built-in stretch (lower quality)", ferr)
	return stretchWAV(wav, rate)
}

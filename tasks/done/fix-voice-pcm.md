# Task: Fix voice STT — wrap raw PCM in WAV header

## Problem
The FOCI Android app sends raw PCM 16-bit audio over WebSocket. The server passes it to Whisper as `voice.opus`, but Whisper can't decode raw PCM without a container format.

Error: `could not process file - is it a valid media file?`

## Fix
In `voice/ws.go`, the `processAudio` function calls:
```go
text, err := c.cfg.STT.Transcribe(ctx, audio, "voice.opus")
```

The audio bytes are raw PCM 16-bit, mono, sample rate is set by the client (check `AudioRecorder.kt` — likely 16000 or 44100 Hz).

**Option A (preferred):** Add a WAV header wrapper function. Before calling Transcribe, wrap the raw PCM in a WAV header and change the filename to `voice.wav`:
```go
wavAudio := wrapPCMInWAV(audio, sampleRate, 1, 16)
text, err := c.cfg.STT.Transcribe(ctx, wavAudio, "voice.wav")
```

WAV header is 44 bytes — simple to construct. PCM 16-bit, mono, little-endian.

**Check the Android client** at `/home/rich/git/foci_android/app/src/main/kotlin/uk/richardkemp/foci/audio/AudioRecorder.kt` to confirm the sample rate (look for `currentSampleRate`).

## Files to modify
- `voice/ws.go` — add WAV wrapper, update Transcribe call
- Add tests for the WAV wrapper function

## Important
- The WAV header format is well-documented: RIFF header + fmt chunk + data chunk = 44 bytes
- Don't change the Android app — server should handle whatever raw PCM comes in
- The sample rate might need to be negotiated or assumed (check what Android sends)
- Commit and push when done, update any relevant docs

# Task: Implement WebSocket voice endpoint for FOCI Android app

## Context
The FOCI Android app (repo: foci_android) is a voice conversation client that connects to clod via WebSocket. The app is built and ready — now clod needs the server-side WebSocket endpoint.

Read the full protocol spec at `/home/rich/git/foci_android/SPEC.md` — it defines the complete message protocol, connection flow, and audio handling.

## What to Build

A WebSocket endpoint at `/voice` that handles the full conversation loop:

### Connection Flow
1. Client connects to `wss://{server}/voice?api_key={key}`
2. Server authenticates via API key (validate against configured keys)
3. Server sends `connected` message with available agents list
4. Client sends `select_agent` with agent_id
5. Server creates/resumes a voice session, sends `session_ready`

### Message Handling
- **Client audio** — receives `audio_start`, binary audio frames, `audio_end`. Transcribes audio (use Groq Whisper API — we have a groq.api_key secret). Returns `transcription` message.
- **Agent processing** — passes transcribed text to the selected agent via `HandleMessage`. Streams response text back as `response_text` messages.
- **TTS** — converts agent response to speech. Stream TTS audio back as binary frames wrapped in `audio_start`/`audio_end`. Use the existing TTS infrastructure (check how the `tts` tool works).
- **Text input** — also handle `text` type messages as direct text input (skip transcription).
- **Ping/pong** — respond to `ping` with `pong`.

### Key Design Decisions
- Use gorilla/websocket (already a Go standard) or nhooyr.io/websocket
- Voice sessions use session keys like `agent:clutch:voice:{connection_id}`
- API key auth — add a `voice_api_keys` config section or reuse existing auth
- Audio format: expect Opus from client, send MP3 for TTS playback
- Keep it simple — this is v1, not production-grade

### Transcription (Groq Whisper)
- POST audio to `https://api.groq.com/openai/v1/audio/transcriptions`
- Model: `whisper-large-v3`
- Send as multipart form: file + model field
- Use the groq.api_key secret
- Return transcribed text

### TTS
- Check how the existing `tts` tool generates audio (look at tools/tts.go or similar)
- Reuse that infrastructure to generate speech from agent response text
- Stream the resulting audio back over WebSocket as binary frames

### Integration Points
- The gateway (clodgw) handles HTTP routing — add the `/voice` WebSocket upgrade there
- Or add it directly to the agent server if that's simpler architecturally
- Need access to: agent instances, secrets store, TTS provider

### Config
Add voice-related config:
```toml
[voice]
enabled = true
api_keys = ["key1", "key2"]  # or reference secrets
```

### Files to Create/Modify
- New: `voice/` package (handler, session, protocol types)
- Modified: gateway or main.go (route registration)
- Modified: config (voice section)
- Update: SPEC.md, docs/CONFIG.md

### Tests
- Protocol message serialization
- Connection handshake flow
- Transcription request building
- Session lifecycle

### Important
- Read `/home/rich/git/foci_android/SPEC.md` FIRST for the complete protocol spec
- Check how TTS currently works in the codebase before implementing
- Check how the gateway routes HTTP requests
- Commit and push when done

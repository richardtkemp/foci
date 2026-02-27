# Task: FOCI — Save and replay audio for message history

## Goal
The app should save audio received from the server (TTS responses) and allow replaying them by tapping on the message text in the conversation view.

## Requirements

1. **Save received audio** — when TTS audio chunks arrive for a response, concatenate them and save to local storage (app cache or internal storage)
2. **Configurable limit** — user setting for how many messages to keep audio for. Default: 50. When exceeded, delete oldest audio files.
3. **Settings UI** — add "Audio history size" setting (number picker or text field) to the existing settings screen
4. **Replay on tap** — tapping a message that has saved audio should replay it. Visual indicator (e.g. small speaker icon) on messages that have audio available.
5. **Audio format** — the server sends MP3 chunks. Save as MP3 files, play with MediaPlayer or ExoPlayer.

## Architecture suggestions

- Audio files stored in app's cache dir, named by message ID or timestamp
- Room database or SharedPreferences to track which messages have audio files
- The app already has `AudioPlayer.kt` in `app/src/main/kotlin/uk/richardkemp/foci/audio/` — extend or reuse it
- Messages already have a data model — check the existing chat history implementation (per-agent JSON in encrypted prefs, added in commit f49c86f)

## Repo
`/home/rich/git/foci_android`

## Important
- Don't break existing functionality — recording, playback, WebSocket connection
- Handle edge cases: app restart (audio files persist), clearing cache, storage pressure
- The current AudioPlayer buffers and plays in real-time — saving should happen alongside, not instead of, live playback
- Commit with descriptive messages, push when done

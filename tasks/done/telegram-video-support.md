# Task: Telegram video message support

## Context
Clod currently handles photo attachments from Telegram (downloads, saves, injects path into message text). Video messages are silently ignored. Fotini needs video support for social media content from Eleni.

## Current photo handling (telegram/bot.go)
When a Telegram message contains a photo:
1. Bot downloads the highest-resolution photo via Telegram API
2. If `image_save_dir` is configured, saves to `YYYY-MM-DDTHH-MM-SSZ_chat-CHATID.ext`
3. Injects `[Image saved to: /path/to/file]` into the message text sent to the agent
4. Agent sees the path and can reference/process the image

## Requirements

### Video reception
1. Handle `message.Video` in the Telegram update handler (same pattern as `message.Photo`)
2. Download video file via Telegram Bot API `getFile`
3. Save to the configured save directory (reuse `image_save_dir` or add a new `video_save_dir` — prefer reusing `image_save_dir` and renaming the concept to `media_save_dir`)
4. Filename format: `YYYY-MM-DDTHH-MM-SSZ_chat-CHATID_video.EXT` (preserve original extension — mp4, mov, etc.)
5. Inject `[Video saved to: /path/to/file]` into the message text
6. Handle `message.VideoNote` (circular video messages) the same way

### Video notes (voice-video messages)
7. Handle `message.VideoNote` — these are the circular video messages in Telegram
8. Save with `_videonote` suffix in filename

### File size limits
9. Telegram Bot API has a 20MB download limit for files. If video exceeds this, inject `[Video too large to download (SIZE MB)]` instead of failing silently

### Config
10. Consider renaming `image_save_dir` to `media_save_dir` with backward compatibility (if `image_save_dir` is set but `media_save_dir` isn't, use `image_save_dir`). Or just reuse `image_save_dir` for all media. Simpler is better.

### Document/file attachments
11. While you're in the video handling area, also handle `message.Document` — any file attachment. Save to the same directory with original filename. This covers PDFs, spreadsheets, etc. that users might send.
12. Inject `[Document saved to: /path/to/file]` or `[File saved to: /path/to/file]`

## Don't do
- Don't add video processing/transcoding
- Don't add video-to-text/frame extraction (that's a separate feature)
- Don't change how the agent processes the saved files — just make them available

## Tests
- Test video message handling (mock Telegram update with video)
- Test file size limit handling
- Test filename generation for video/videonote/document
- Test backward compat if renaming config key

## Docs
- Update SPEC.md (Telegram bot section, tool list if relevant)
- Update docs/CONFIG.md if config key changes
- Commit and push when done

# Task: Image persistence — save received images to disk (#89)

## Two mechanisms needed

### 1. Auto-save all images (config-driven)

New config option to automatically save every image received via Telegram to a directory.

**Config (global and per-agent):**
```toml
[telegram]
image_save_dir = ""  # default: empty = don't auto-save

[[agents]]
id = "clutch"
image_save_dir = "/home/clod/clutch/images"  # per-agent override
```

Per-agent overrides global. Empty/unset = disabled.

**Behaviour:**
- When a Telegram message contains an image, download the highest-resolution version from Telegram API (use `getFile` + file download)
- Save to the configured directory with a sensible filename (e.g. `2026-02-24T22-38-01Z_chat-5970082313.jpg`)
- This happens before the agent processes the message — the agent doesn't need to do anything
- Log the save path

### 2. Agent-accessible image path

When the agent receives a message containing an image, it currently gets the image as inline content for the API call (base64). It should ALSO know the file path on disk so it can reference, copy, or process the image later.

- If auto-save is enabled, include the saved file path in the image metadata available to the agent
- If auto-save is NOT enabled, the agent should still be able to request saving via a mechanism (could be as simple as the image being saved to a temp path always, with the path available in context)

## Implementation Notes

- Telegram images come as `PhotoSize` arrays — pick the largest one
- Use Telegram Bot API `getFile` to get the file path, then download from `https://api.telegram.org/file/bot<token>/<file_path>`
- The download happens in telegram/bot.go where images are already extracted
- Check how images are currently handled (look for `PhotoSize`, `photo`, image extraction in bot.go)

## Update docs/CONFIG.md with the new config options. Write tests, commit with descriptive message, push.

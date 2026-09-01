---
name: image-gen
description: Generate images via OpenRouter (GPT-5 Image, GPT-5 Image Mini, Gemini Pro/Flash).
owner: foci
seeded: true
homepage: https://openrouter.ai/
---

> **This `SKILL.md` is yours to customise** (seed-if-missing — override it, add your own sibling files). Changes to it survive a restart, but they live only on this install: to change the skill for every agent, edit it in the foci repo (`shared/skills/image-gen/`).

# Image Generation (OpenRouter)

Generate images using OpenRouter's image-capable models via `http_request`.

> **"Send me an image" ≠ "generate an image."** If the user asks you to *send* an image with no details at all, they mean find an existing one on disk and send it — not generate a new one. Only generate when they describe what they want, or explicitly say "generate"/"make"/"create".

> **Generation is slow (often 30s–2min).** Always pass a timeout of **at least 3 minutes** (180s) on the `http_request` call, or it will exceed the 10s foreground threshold, drop to background, and may hit `context deadline exceeded`.

## Models

| Alias | Model ID | Notes |
|-------|----------|-------|
| `gpt5` | openai/gpt-5-image | Best quality, slower. **Square 1024×1024 only** — no aspect/size control (see below) |
| `gpt5-mini` | openai/gpt-5-image-mini | **Default.** Fast + cheap. **Square 1024×1024 only** — no aspect/size control |
| `gemini-pro` | google/gemini-3-pro-image-preview | Supports resolution/aspect config |
| `gemini-flash` | google/gemini-2.5-flash-image | Cheapest, fast. Supports resolution/aspect config |

> **Aspect ratio / size is a Gemini-only capability, verified 2026-07-06.** The GPT-5 image
> endpoints on OpenRouter do **not** advertise `aspect_ratio`, `resolution`, `image_config`, or
> `size` in their `supported_parameters` — passing `image_config` is silently ignored and you get
> 1024×1024 back regardless. If you prompt a tall/wide layout, GPT-5 either self-squeezes it into
> the square (dropping detail/text) or overflows the frame. **Need non-square? Use a Gemini model.**
> Confirm any model's real capabilities via
> `GET https://openrouter.ai/api/v1/models/<id>/endpoints` → `supported_parameters` before asserting.
> Trade-off seen in practice: Gemini does portrait but garbles Greek/accented text; GPT-5 renders
> text cleaner but is square-only. For a card needing *both* correct text and a chosen shape,
> generate the art with the right model then **overlay** the text programmatically (PIL).

## Workflow

One `http_request` call does everything — calls the API, extracts the image from the JSON response, decodes base64, and saves to disk:

```
http_request(
  method: "POST",
  url: "https://openrouter.ai/api/v1/chat/completions",
  headers: {
    "Authorization": "Bearer {{secret:openrouter.api_key}}",
    "Content-Type": "application/json"
  },
  body: '{"model":"openai/gpt-5-image-mini","messages":[{"role":"user","content":"A cat on the moon"}],"modalities":["image","text"]}',
  save_to: "/tmp/generated-image.png",
  save_from_json_path: "choices.0.message.images.0.image_url.url"
)
```

Then send the image:
```
send_to_chat(file: "/tmp/generated-image.png", text: "Here's your image")
```

## Parameters

### Required
- **model** — one of the model IDs above
- **prompt** — image description (goes in messages[0].content)

### Optional (Gemini models only)
- **aspect_ratio** — e.g. 1:1, 16:9, 9:16, 3:4 (default: 1:1)
- **resolution** — 1K, 2K, 4K (default: 1K)

For Gemini models, add `image_config` to the request body:
```json
{
  "model": "google/gemini-3-pro-image-preview",
  "messages": [{"role": "user", "content": "prompt"}],
  "modalities": ["image", "text"],
  "image_config": {
    "aspect_ratio": "16:9",
    "image_size": "2K"
  }
}
```

## How it works

1. `http_request` POSTs to OpenRouter with the secret API key (domain-locked, never exposed)
2. `save_from_json_path` extracts `choices.0.message.images.0.image_url.url` from the JSON response
3. If the extracted value is a `data:image/png;base64,...` URI, it's decoded to binary automatically
4. If it's a regular URL, the raw content is saved
5. The decoded image is written to the `save_to` path
6. Send via `send_to_chat` with `file` (the param is `file`, NOT `file_path`; shell form: `foci_send_to_chat --file /tmp/x.png --caption "..."`)

## Notes

- No Python dependency — uses `http_request` tool directly
- Secret `openrouter.api_key` must be configured in secrets.toml with `allowed_hosts = ["openrouter.ai"]`
- Image generation is slow — often 30s to 2min depending on model. Use a `timeout` of at least 180s on the `http_request` call.
- **High-res Gemini (`image_size: "2K"`/"4K") can exceed even the 300s max timeout** and return a truncated/empty body — which surfaces as `extract ... from JSON: unexpected end of JSON input` (looks like a parse bug, is really a timeout). Verified 2026-07-18: `gemini-3-pro` at 2K failed at 240s; **1K succeeded** (~1408×768 at 16:9 — plenty sharp for a web/email banner). If you hit that JSON error, drop resolution before assuming the request is broken.
- The old Python script (`scripts/generate_image.py`) is deprecated

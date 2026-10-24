---
name: image-gen
description: Generate images via OpenRouter (Gemini image models, GPT-5 Image Mini).
owner: foci
seeded: true
homepage: https://openrouter.ai/
---

> **This `SKILL.md` is yours to customise** (seed-if-missing — override it, add your own sibling files). Changes to it survive a restart, but they live only on this install: to change the skill for every agent, edit it in the foci repo (`shared/skills/image-gen/`).

# Image Generation (OpenRouter)

> **"Send me an image" ≠ "generate an image."** With no description, the user means an existing file. Generate only when they describe what they want or say "generate"/"make"/"create".

## Models

| Model ID | Use |
|---|---|
| `google/gemini-3-pro-image-preview` | Best quality. Aspect ratio and resolution configurable. |
| `google/gemini-2.5-flash-image` | Cheap and fast. Aspect ratio and resolution configurable. |
| `openai/gpt-5-image-mini` | Cleanest rendered text. Square 1024×1024 only; size settings are ignored. |

Check a model's real parameters with `GET https://openrouter.ai/api/v1/models/<id>/endpoints` → `supported_parameters`. For correct text on a non-square image, generate the art with Gemini and overlay the text with PIL.

## Generate

One request calls the API, extracts the image and writes the file. Shell form:

```bash
cat > /tmp/req.json <<'EOF'
{"model": "google/gemini-3-pro-image-preview",
 "messages": [{"role": "user", "content": "A red apple on a white table. No text, no letters, no signature."}],
 "modalities": ["image", "text"],
 "image_config": {"aspect_ratio": "1:1", "image_size": "1K"}}
EOF
foci_http_request --method POST --url https://openrouter.ai/api/v1/chat/completions \
  --headers '{"Authorization":"Bearer {{secret:openrouter.api_key}}","Content-Type":"application/json"}' \
  --body-file /tmp/req.json --timeout 290 \
  --save-to /tmp/out.jpg --save-from-json-path choices.0.message.images.0.image_url.url
```

The `http_request` tool takes the same fields (`method`, `url`, `headers`, `body`, `save_to`, `save_from_json_path`, `timeout`).

- `image_config` is for Gemini only: `aspect_ratio` (`1:1`, `16:9`, `9:16`, `3:4`, …) and `image_size` (`1K`, `2K`, `4K`). Omit it for GPT-5 Image Mini.
- Generation takes 30s–2min. Always set `--timeout 290`. A call past 10s finishes in the background and its result arrives as a separate message.
- For several images, make one call per image in parallel (separate tool calls; not `&`).
- Gemini returns JPEG data whatever `--save-to` is named. Name the file `.jpg`, or convert when a real PNG is needed: `python3 -c "from PIL import Image; Image.open('/tmp/out.jpg').save('/tmp/out.png')"`.
- Send the result with `foci_send_to_chat "caption" --file /tmp/out.jpg` (tool param `file`).

## When it fails

- **`key "images" not found`**: re-run with `--save-to /tmp/raw.json` and no `--save-from-json-path`, then read `jq '.choices[0] | {finish_reason, refusal: .message.refusal}' /tmp/raw.json`.
  - `finish_reason: "error"` with no refusal is a provider failure. Switch model; rewording will not help.
  - A refusal is about the prompt. Rephrase.
- **`unexpected end of JSON input`**: the body was truncated by the timeout. High resolutions (`2K`/`4K`) can exceed 290s. Drop to `1K`.

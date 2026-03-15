---
name: image-gen
description: Generate images via OpenRouter (GPT-5 Image, GPT-5 Image Mini, Gemini Pro/Flash).
homepage: https://openrouter.ai/
metadata:
  foci:
    requires:
      secrets:
        - openrouter.api_key
---

# Image Generation (OpenRouter)

Generate images using OpenRouter's image-capable models via `http_request`.

## Models

| Alias | Model ID | Notes |
|-------|----------|-------|
| `gpt5` | openai/gpt-5-image | Best quality, slower |
| `gpt5-mini` | openai/gpt-5-image-mini | **Default.** Fast + cheap |
| `gemini-pro` | google/gemini-3-pro-image-preview | Supports resolution/aspect config |
| `gemini-flash` | google/gemini-2.5-flash-image | Cheapest, fast |

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

Then send the image to the user:
```
send_message_to_user(file_path: "/tmp/generated-image.png", text: "Here's your image")
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
6. Send via `send_message_to_user` with `file_path`

## Notes

- No Python dependency — uses `http_request` tool directly
- Secret `openrouter.api_key` must be configured in secrets.toml with `allowed_hosts = ["openrouter.ai"]`
- Image generation can take 10-30 seconds depending on model

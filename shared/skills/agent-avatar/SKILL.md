---
name: agent-avatar
description: Design, validate and install an avatar for a foci agent — composition for the circular crop, the small-size legibility gate, and where foci looks for the file.
owner: foci
seeded: true
script: avatar-gate.py
---

> **This `SKILL.md` is yours to customise** (seed-if-missing — override it, add your own sibling files). Changes to it survive a restart, but they live only on this install: to change the skill for every agent, edit it in the foci repo (`shared/skills/agent-avatar/`).

# Agent avatars

Image generation mechanics are in the `image-gen` skill. This skill covers what is specific to avatars.

## What holds across a good set
- **1024×1024 square.**
- **One centred subject, no text or lettering.**
- **One dominant hue** carrying the whole image.

Background (dark, or transparent) and style (photographic, line art, flat vector, screenprint) are per-agent choices. Pick the register that fits the agent's character: read its `character/SOUL.md` first, and look at 3–4 of the install's existing avatars rather than inferring a style from one.

## The avatar is displayed in a circle
- **The corners are cut.** Filling the square edge to edge wastes ~21% of the area.
- **Keep the subject inside the inscribed circle with margin**, or it is clipped mid-stroke.
- Wide horizontal elements are truncated at both ends. Check whether that looks intentional or severed.
- Corner vignettes and background gradients vanish after the crop. Spend contrast on the centre.

## Approach
1. Read the agent's `character/SOUL.md`.
2. Generate **3 distinct concepts**, not variations of one, as parallel requests per `image-gen` (Gemini, `"aspect_ratio":"1:1"`). End each prompt with "no text, no letters, no words, no signature".
3. **Run the gate before forming an opinion.**
4. Send all candidates plus the gate strip, with an honest recommendation. The user picks; it is an aesthetic call.
5. Install, and say it appears after a foci restart.

## The gate: `avatar-gate.py`

```bash
~/shared/skills/agent-avatar/avatar-gate.py a.png b.png c.png -o /tmp/gate.png
# then view /tmp/gate.png and judge from it, not from the originals
```

It masks each candidate to a circle and renders it at true display sizes (32/48/64 px) on a light and a dark ground, plus one smooth enlargement. Flags: `--sizes 32,48,64`, `--inspect 192` (`0` to skip), `-o`.

Don't substitute a one-line PIL resize. A single LANCZOS pass from 1024 aliases, un-premultiplied RGBA gives transparent images a false dark halo, and a NEAREST enlargement shows stair-stepping no real display produces.

What survives the gate: high fill, a strong silhouette, high value contrast. Fine detail, thin strokes and low fill disappear. Check the light-ground column too: a near-black avatar can read as an inkblot on a light theme. Expect the gate to change your ranking.

Clean symmetric geometry on a bright field can read as signage; an off-axis or organic element fixes it.

## Transparency
Ask for it in the prompt (fully transparent background, alpha channel, isolated subject with clean edges); models return opaque images otherwise. Check the alpha channel, not a preview — viewers composite transparency onto their own backdrop:

```bash
python3 -c "from PIL import Image; a=Image.open('f.png').convert('RGBA').getchannel('A'); print(a.getextrema())"
```

## Install
With no `avatar =` field in the agent's config, foci uses the first of:
1. `<workspace>/avatar.{png,jpg,jpeg,webp,gif}`
2. `<workspace>/.data/avatar.{png,jpg,jpeg,webp,gif}`

Prefer `.data/avatar.png`, which keeps the workspace root clean. Save a real PNG: Gemini returns JPEG data, so convert first (see `image-gen`). The avatar is read at startup only; it appears after the next foci restart.

#!/usr/bin/env python3
"""Avatar legibility gate — render candidates as foci actually displays them.

foci shows an agent avatar as a SMALL CIRCLE. Judging an uncropped 1024px square
is judging the wrong artifact. This renders each candidate circularly masked at
true display sizes, on both a light and a dark chat ground, plus a smooth
enlargement for detail inspection.

Usage:
    ./avatar-gate.py a.png b.png c.png [-o thumbs.png] [--sizes 32,48,64]

Then Read the output PNG and judge from that, not from the originals.

Why not a one-line PIL resize:
  * Downscaling 1024 -> 64 in a single LANCZOS pass aliases badly. We reduce by
    an integer box factor first (proper area-average), then LANCZOS the remainder.
  * RGBA must be premultiplied before resampling, or transparent pixels bleed
    their (usually black) colour into the subject's edges as a dark halo.
  * NEAREST enlargement of a 64px thumbnail shows chunky artifacts no real
    display produces. Inspect with a smooth filter; show true size at true size.
"""

import argparse
import os
import sys

import numpy as np
from PIL import Image, ImageDraw

LIGHT = (242, 242, 239)
DARK = (26, 26, 25)
PAD = 14


def hq_resize(im: Image.Image, size: int) -> Image.Image:
    """Alias-free downscale of an RGBA image, premultiplied so edges don't halo."""
    im = im.convert("RGBA")
    arr = np.asarray(im).astype(np.float64) / 255.0
    arr[..., :3] *= arr[..., 3:4]  # premultiply
    pre = Image.fromarray((arr * 255).round().astype(np.uint8), "RGBA")

    # Integer box-reduce as far as we can without undershooting the target.
    factor = max(1, min(pre.width, pre.height) // size)
    if factor > 1:
        pre = pre.reduce(factor)
    pre = pre.resize((size, size), Image.LANCZOS)

    out = np.asarray(pre).astype(np.float64) / 255.0
    a = out[..., 3:4]
    out[..., :3] = np.divide(out[..., :3], a, out=np.zeros_like(out[..., :3]), where=a > 1e-6)
    out = np.clip(out, 0.0, 1.0)
    return Image.fromarray((out * 255).round().astype(np.uint8), "RGBA")


def circle_mask(size: int, supersample: int = 8) -> Image.Image:
    """Antialiased circular mask (PIL's ellipse is hard-edged, hence supersampling)."""
    big = size * supersample
    m = Image.new("L", (big, big), 0)
    ImageDraw.Draw(m).ellipse((0, 0, big - 1, big - 1), fill=255)
    return m.resize((size, size), Image.LANCZOS)


def as_avatar(path: str, size: int) -> Image.Image:
    im = hq_resize(Image.open(path), size)
    mask = circle_mask(size)
    a = Image.fromarray(
        (np.asarray(im.getchannel("A")).astype(np.uint16) * np.asarray(mask) // 255).astype(np.uint8)
    )
    im.putalpha(a)
    return im


def on(bg, im: Image.Image) -> Image.Image:
    plate = Image.new("RGBA", im.size, bg + (255,))
    return Image.alpha_composite(plate, im)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("images", nargs="+", help="candidate image paths")
    ap.add_argument("-o", "--out", default="thumbs.png", help="output contact sheet (default thumbs.png)")
    ap.add_argument("--sizes", default="32,48,64", help="true display sizes, comma-separated")
    ap.add_argument("--inspect", type=int, default=192, help="smooth enlargement size (0 to skip)")
    args = ap.parse_args()

    for p in args.images:
        if not os.path.isfile(p):
            print(f"error: no such file: {p}", file=sys.stderr)
            return 1

    sizes = [int(s) for s in args.sizes.split(",") if s.strip()]
    col_w = max([args.inspect] + sizes) if args.inspect else max(sizes)
    # one column per candidate; rows = inspect, then each true size (light|dark pair)
    row_hs = ([args.inspect] if args.inspect else []) + sizes
    sheet_w = len(args.images) * col_w + (len(args.images) + 1) * PAD
    sheet_h = sum(row_hs) + (len(row_hs) + 1) * PAD
    sheet = Image.new("RGB", (sheet_w, sheet_h), (128, 128, 128))

    for ci, path in enumerate(args.images):
        cx = PAD + ci * (col_w + PAD)
        y = PAD
        if args.inspect:
            big = as_avatar(path, args.inspect)
            half = args.inspect // 2
            sheet.paste(on(LIGHT, big.crop((0, 0, half, args.inspect))), (cx, y))
            sheet.paste(
                on(DARK, big.crop((half, 0, args.inspect, args.inspect))), (cx + half, y)
            )
            y += args.inspect + PAD
        for s in sizes:
            small = as_avatar(path, s)
            x = cx + (col_w - (s * 2 + 8)) // 2
            sheet.paste(on(LIGHT, small), (x, y))
            sheet.paste(on(DARK, small), (x + s + 8, y))
            y += s + PAD

    sheet.save(args.out)
    print(f"{args.out}  {sheet.size[0]}x{sheet.size[1]}  ({len(args.images)} candidates, sizes {sizes})")
    print("Top row: enlarged for inspection (left half on light ground, right half on dark).")
    print("Below: TRUE display sizes, each shown on light and dark. Judge legibility here.")
    return 0


if __name__ == "__main__":
    sys.exit(main())

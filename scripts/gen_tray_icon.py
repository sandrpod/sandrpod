#!/usr/bin/env python3
"""Generate the sandrpod-tray icon: indigo shield + white check.

Outputs (into cmd/sandrpod-tray/assets/):
  icon.png — 64x64 RGBA, for macOS/Linux trays (PNG accepted)
  icon.ico — 16/24/32/48 BMP-format entries, for Windows
             (systray loads via LoadImage(IMAGE_ICON): ICO only,
              BMP entries for maximum LoadImage compatibility)

Drawn at 512px with supersampling, then LANCZOS-downscaled so the
16px entry stays legible.
"""
from PIL import Image, ImageDraw
import os
import sys

OUT_DIR = sys.argv[1] if len(sys.argv) > 1 else "."

S = 512  # master canvas
SHIELD = (79, 70, 229, 255)        # indigo-600 — reads on light & dark taskbars
SHIELD_EDGE = (55, 48, 163, 255)   # indigo-800 rim for 16px contrast
CHECK = (255, 255, 255, 255)


def quad(p0, p1, p2, n=24):
    """Sample a quadratic bezier."""
    pts = []
    for i in range(n + 1):
        t = i / n
        x = (1 - t) ** 2 * p0[0] + 2 * (1 - t) * t * p1[0] + t ** 2 * p2[0]
        y = (1 - t) ** 2 * p0[1] + 2 * (1 - t) * t * p1[1] + t ** 2 * p2[1]
        pts.append((x, y))
    return pts


def shield_polygon(inset=0.0):
    """Classic badge: flat top, straight upper sides, curved taper to a
    bottom point. inset > 0 shrinks the shape uniformly (for the rim)."""
    left, right, top = 72 + inset, 440 - inset, 64 + inset
    waist_y = 268
    tip = (256, 484 - inset * 1.6)
    pts = [(left, top), (right, top), (right, waist_y)]
    pts += quad((right, waist_y), (right - 24, waist_y + 116), tip)
    pts += quad(tip, (left + 24, waist_y + 116), (left, waist_y))
    pts.append((left, waist_y))
    return pts


img = Image.new("RGBA", (S, S), (0, 0, 0, 0))
d = ImageDraw.Draw(img)
d.polygon(shield_polygon(0), fill=SHIELD_EDGE)   # rim
d.polygon(shield_polygon(22), fill=SHIELD)        # body

# Check mark — two thick round-capped strokes.
w = 58
a, b, c = (162, 262), (232, 336), (356, 186)
d.line([a, b], fill=CHECK, width=w)
d.line([b, c], fill=CHECK, width=w)
for p in (a, b, c):
    d.ellipse([p[0] - w // 2, p[1] - w // 2, p[0] + w // 2, p[1] + w // 2], fill=CHECK)

os.makedirs(OUT_DIR, exist_ok=True)

png_path = os.path.join(OUT_DIR, "icon.png")
img.resize((64, 64), Image.LANCZOS).save(png_path, "PNG")

ico_path = os.path.join(OUT_DIR, "icon.ico")
img.resize((48, 48), Image.LANCZOS).save(
    ico_path, "ICO",
    sizes=[(16, 16), (24, 24), (32, 32), (48, 48)],
    bitmap_format="bmp",
)

for p in (png_path, ico_path):
    print(f"{p}: {os.path.getsize(p)} bytes")

# Sanity: re-open the ICO and verify the size table.
ico = Image.open(ico_path)
print("ico sizes:", sorted(ico.info.get("sizes", [])), "format:", ico.format)

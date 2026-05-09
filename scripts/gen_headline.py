#!/usr/bin/env python3
"""Generate a newspaper-style headline cover image for briefing-v3.

This script is invoked by internal/image/headline.go once per issue.
It renders a single PNG laid out like a front-page newspaper teaser:

  ┌────────────────────────────────────────────┐
  │ 🤖 AI 资讯日报      2026/04/11 · FRI      │  ← masthead
  │ ──────────────────────────────────────     │
  │                                            │
  │   {HEADLINE — large, bold, up to 2 lines}  │  ← main title
  │                                            │
  │   {subtitle — one line, lighter weight}    │
  │                                            │
  │ ──────────────────────────────────────     │
  │ briefing-v3 · 每日 AI 资讯        v3 · AI │  ← footer strip
  └────────────────────────────────────────────┘

Design notes:
  - Deep navy background (#0B1221) with a subtle vertical gradient so it
    doesn't look flat on Slack's light/dark themes.
  - Accent color is a cool blue (#60A5FA) used for the brand name, the
    primary divider, and the corner accent marks.
  - Main headline wraps at word-ish boundaries (we measure per-rune so
    CJK text works). If even two lines at the base size are too wide,
    we shrink the font in 4-point steps until it fits.
  - Both fonts must be CJK-capable; defaults point at the Noto Sans CJK
    files packaged with the Ubuntu `fonts-noto-cjk` package.

Failure modes:
  - Missing fonts raise IOError; the caller treats any non-zero exit as
    a fatal render failure.
  - If --output's parent directory does not exist it is created
    automatically so the Go side doesn't have to.

On success, the script prints "OK: <absolute output path>" so Go can
easily confirm completion.
"""

from __future__ import annotations

import argparse
import os
import sys
from datetime import datetime

from PIL import Image, ImageDraw, ImageFont

# ---------- palette ----------
BG_TOP = (11, 18, 33)        # #0B1221 — near-black navy
BG_BOTTOM = (15, 23, 42)     # #0F172A — slightly lighter navy
ACCENT = (96, 165, 250)      # #60A5FA — cool blue, primary accent
ACCENT_DIM = (56, 105, 186)  # darker accent for secondary strokes
TEXT_MAIN = (241, 245, 249)  # #F1F5F9 — headline text
TEXT_MUTED = (148, 163, 184) # #94A3B8 — subtitle / footer text
TEXT_SOFT = (203, 213, 225)  # #CBD5E1 — subtitle hover

WEEKDAY_ZH = ("一", "二", "三", "四", "五", "六", "日")

MAC_BOLD_FONT_CANDIDATES = [
    "/System/Library/Fonts/Hiragino Sans GB.ttc",
    "/System/Library/Fonts/STHeiti Medium.ttc",
    "/System/Library/Fonts/PingFang.ttc",
]

MAC_REGULAR_FONT_CANDIDATES = [
    "/System/Library/Fonts/Hiragino Sans GB.ttc",
    "/System/Library/Fonts/STHeiti Light.ttc",
    "/System/Library/Fonts/PingFang.ttc",
]

LINUX_BOLD_FONT_CANDIDATES = [
    "/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc",
    "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
]

LINUX_REGULAR_FONT_CANDIDATES = [
    "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
    "/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc",
]


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="Generate a newspaper-style headline image for briefing-v3.",
    )
    p.add_argument("--date", required=True, help="ISO date string, e.g. 2026-04-11")
    p.add_argument("--headline", required=True, help="Main headline text (CJK ok)")
    p.add_argument(
        "--subtitle",
        default="briefing-v3 每日 AI 资讯",
        help="Secondary line under the headline",
    )
    p.add_argument("--output", required=True, help="Absolute path of the PNG to write")
    p.add_argument("--width", type=int, default=1200, help="Canvas width in pixels")
    p.add_argument("--height", type=int, default=630, help="Canvas height in pixels")
    p.add_argument(
        "--font-bold",
        default="/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc",
        help="Path to a bold CJK-capable TTF/TTC font",
    )
    p.add_argument(
        "--font-regular",
        default="/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
        help="Path to a regular CJK-capable TTF/TTC font",
    )
    return p.parse_args()


def _find_sc_index(path: str) -> int:
    if not path.lower().endswith(".ttc"):
        return 0
    for i in range(20):
        try:
            f = ImageFont.truetype(path, size=20, index=i)
            name = f.getname()[0]
            if "SC" in name or "GB" in name:
                return i
        except Exception:
            break
    return 0


_sc_index_cache: dict[str, int] = {}


def resolve_font_path(requested_path: str, role: str) -> str:
    candidates = []
    if requested_path:
        candidates.append(requested_path)
    if role == "bold":
        candidates.extend(MAC_BOLD_FONT_CANDIDATES)
        candidates.extend(LINUX_BOLD_FONT_CANDIDATES)
    else:
        candidates.extend(MAC_REGULAR_FONT_CANDIDATES)
        candidates.extend(LINUX_REGULAR_FONT_CANDIDATES)

    seen = set()
    for raw in candidates:
        path = str(raw).strip()
        if not path or path in seen:
            continue
        seen.add(path)
        if os.path.exists(path):
            return path
    return requested_path


def load_font(path: str, size: int) -> ImageFont.FreeTypeFont:
    if path not in _sc_index_cache:
        _sc_index_cache[path] = _find_sc_index(path)
    return ImageFont.truetype(path, size=size, index=_sc_index_cache[path])


def make_gradient(width: int, height: int) -> Image.Image:
    """Return a vertical gradient from BG_TOP to BG_BOTTOM.

    We draw it by writing one horizontal line per pixel row with a
    linearly interpolated color. For 630px that's fast enough; the
    whole pass is <30ms on a modern CPU.
    """
    base = Image.new("RGB", (width, height), BG_TOP)
    draw = ImageDraw.Draw(base)
    for y in range(height):
        t = y / max(height - 1, 1)
        r = int(BG_TOP[0] + (BG_BOTTOM[0] - BG_TOP[0]) * t)
        g = int(BG_TOP[1] + (BG_BOTTOM[1] - BG_TOP[1]) * t)
        b = int(BG_TOP[2] + (BG_BOTTOM[2] - BG_TOP[2]) * t)
        draw.line([(0, y), (width, y)], fill=(r, g, b))
    return base


def draw_grid_overlay(base: Image.Image, color=(255, 255, 255, 6)) -> None:
    """Lay a very subtle grid over the background.

    Uses a separate RGBA overlay so we can composite at low alpha and
    give the background a bit of structure without stealing focus.
    """
    overlay = Image.new("RGBA", base.size, (0, 0, 0, 0))
    od = ImageDraw.Draw(overlay)
    w, h = base.size
    step = 60
    for x in range(0, w, step):
        od.line([(x, 0), (x, h)], fill=color, width=1)
    for y in range(0, h, step):
        od.line([(0, y), (w, y)], fill=color, width=1)
    base.paste(Image.alpha_composite(base.convert("RGBA"), overlay).convert("RGB"))


def measure_text(draw: ImageDraw.ImageDraw, text: str, font: ImageFont.FreeTypeFont) -> tuple[int, int]:
    """Return (width, height) in pixels for the given text + font."""
    bbox = draw.textbbox((0, 0), text, font=font)
    return bbox[2] - bbox[0], bbox[3] - bbox[1]


def wrap_headline(
    draw: ImageDraw.ImageDraw,
    text: str,
    font: ImageFont.FreeTypeFont,
    max_width: int,
    max_lines: int = 2,
) -> list[str]:
    """Greedy per-rune wrap that respects CJK-friendly soft breakpoints.

    The upstream layout prefers two or fewer lines, so callers should
    combine this with a font-shrink loop: if the returned list has
    more than max_lines elements the font size should be reduced.

    We split on whitespace for Latin text but fall back to per-character
    wrapping for CJK (which has no interword whitespace).
    """
    text = text.strip()
    if not text:
        return [""]

    # Detect if the string is CJK-heavy. If at least half the runes are
    # outside the ASCII range treat it as CJK and wrap per-rune;
    # otherwise wrap per-word. This avoids weird mid-word breaks for
    # pure English headlines.
    non_ascii = sum(1 for ch in text if ord(ch) > 127)
    cjk_mode = non_ascii >= max(1, len(text) // 2)

    lines: list[str] = []
    if cjk_mode:
        current = ""
        for ch in text:
            trial = current + ch
            w, _ = measure_text(draw, trial, font)
            if w > max_width and current:
                lines.append(current)
                current = ch
            else:
                current = trial
        if current:
            lines.append(current)
    else:
        words = text.split()
        current = ""
        for word in words:
            trial = (current + " " + word).strip() if current else word
            w, _ = measure_text(draw, trial, font)
            if w > max_width and current:
                lines.append(current)
                current = word
            else:
                current = trial
        if current:
            lines.append(current)

    return lines


def fit_headline(
    draw: ImageDraw.ImageDraw,
    text: str,
    font_path: str,
    max_width: int,
    max_lines: int = 2,
    start_size: int = 84,
    min_size: int = 42,
    step: int = 4,
) -> tuple[ImageFont.FreeTypeFont, list[str]]:
    """Iteratively shrink the headline font until it fits in max_lines.

    Returns the chosen font + the wrapped line list. If even min_size
    can't fit in max_lines, we return the last (wrapped) attempt; the
    caller can decide whether to live with extra lines being cropped or
    surface the error.
    """
    size = start_size
    last_font = load_font(font_path, size)
    last_lines = wrap_headline(draw, text, last_font, max_width, max_lines)
    while size > min_size and len(last_lines) > max_lines:
        size -= step
        last_font = load_font(font_path, size)
        last_lines = wrap_headline(draw, text, last_font, max_width, max_lines)
    # Even if still > max_lines, return what we have — the drawing
    # loop clamps to max_lines when rendering.
    return last_font, last_lines[:max_lines]


def format_date(date_str: str) -> str:
    """Turn '2026-04-11' into '2026/04/11 · 周五'."""
    try:
        dt = datetime.strptime(date_str, "%Y-%m-%d")
    except ValueError:
        return date_str
    weekday = WEEKDAY_ZH[dt.weekday()]
    return f"{dt.year}/{dt.month:02d}/{dt.day:02d} · 周{weekday}"


def draw_corner_marks(draw: ImageDraw.ImageDraw, width: int, height: int) -> None:
    """Small corner L-shapes, like a newspaper crop mark. Pure decoration."""
    length = 28
    inset = 24
    thickness = 3
    color = ACCENT

    # Top-left
    draw.line([(inset, inset), (inset + length, inset)], fill=color, width=thickness)
    draw.line([(inset, inset), (inset, inset + length)], fill=color, width=thickness)
    # Top-right
    draw.line([(width - inset - length, inset), (width - inset, inset)], fill=color, width=thickness)
    draw.line([(width - inset, inset), (width - inset, inset + length)], fill=color, width=thickness)
    # Bottom-left
    draw.line([(inset, height - inset), (inset + length, height - inset)], fill=color, width=thickness)
    draw.line([(inset, height - inset - length), (inset, height - inset)], fill=color, width=thickness)
    # Bottom-right
    draw.line(
        [(width - inset - length, height - inset), (width - inset, height - inset)],
        fill=color,
        width=thickness,
    )
    draw.line(
        [(width - inset, height - inset - length), (width - inset, height - inset)],
        fill=color,
        width=thickness,
    )


def main() -> int:
    args = parse_args()
    args.font_bold = resolve_font_path(args.font_bold, "bold")
    args.font_regular = resolve_font_path(args.font_regular, "regular")

    width, height = args.width, args.height
    padding = 72  # outer content padding

    # 1. Canvas with gradient + overlay grid.
    img = make_gradient(width, height)
    draw_grid_overlay(img)
    draw = ImageDraw.Draw(img)

    # 2. Fonts. We load the masthead + footer at fixed sizes; the
    # headline uses fit_headline for auto-shrinking.
    font_brand = load_font(args.font_bold, 40)
    font_date = load_font(args.font_regular, 26)
    font_subtitle = load_font(args.font_regular, 32)
    font_footer = load_font(args.font_regular, 22)
    font_badge = load_font(args.font_bold, 22)

    # 3. Masthead: brand on the left, date on the right, both on the
    # same horizontal baseline near the top of the canvas.
    brand_text = "AI 资讯日报"
    brand_w, brand_h = measure_text(draw, brand_text, font_brand)
    brand_y = padding - 12
    # Accent bullet bar to the left of the brand.
    bar_x = padding
    bar_y = brand_y + 4
    draw.rectangle(
        [bar_x, bar_y, bar_x + 8, bar_y + brand_h - 8],
        fill=ACCENT,
    )
    draw.text(
        (bar_x + 22, brand_y),
        brand_text,
        font=font_brand,
        fill=TEXT_MAIN,
    )

    date_text = format_date(args.date)
    date_w, date_h = measure_text(draw, date_text, font_date)
    draw.text(
        (width - padding - date_w, brand_y + (brand_h - date_h) // 2),
        date_text,
        font=font_date,
        fill=TEXT_MUTED,
    )

    # 4. Primary divider line under the masthead.
    masthead_bottom = brand_y + brand_h + 18
    draw.line(
        [(padding, masthead_bottom), (width - padding, masthead_bottom)],
        fill=ACCENT,
        width=3,
    )
    # Thin secondary line for a double-rule newspaper feel.
    draw.line(
        [(padding, masthead_bottom + 6), (width - padding, masthead_bottom + 6)],
        fill=ACCENT_DIM,
        width=1,
    )

    # 5. Main headline. Fit it inside the content area.
    headline_top = masthead_bottom + 56
    headline_max_width = width - padding * 2
    font_headline, lines = fit_headline(
        draw,
        args.headline,
        args.font_bold,
        max_width=headline_max_width,
        max_lines=2,
        start_size=88,
        min_size=46,
        step=4,
    )
    line_height = font_headline.size + 14
    for i, line in enumerate(lines):
        draw.text(
            (padding, headline_top + i * line_height),
            line,
            font=font_headline,
            fill=TEXT_MAIN,
        )

    # 6. Subtitle directly under the last headline line. Truncate with
    # ellipsis if it overflows the content width.
    subtitle_y = headline_top + len(lines) * line_height + 18
    subtitle = args.subtitle.strip() or ""
    sw, _ = measure_text(draw, subtitle, font_subtitle)
    if sw > headline_max_width:
        # Rune-wise shrink until it fits.
        trimmed = subtitle
        while trimmed and measure_text(draw, trimmed + "…", font_subtitle)[0] > headline_max_width:
            trimmed = trimmed[:-1]
        subtitle = trimmed + "…"
    draw.text(
        (padding, subtitle_y),
        subtitle,
        font=font_subtitle,
        fill=TEXT_SOFT,
    )

    # 7. Footer strip: thin rule + brand wordmark on the left + small
    # "AI · v3" badge on the right.
    footer_rule_y = height - padding + 8
    draw.line(
        [(padding, footer_rule_y), (width - padding, footer_rule_y)],
        fill=ACCENT_DIM,
        width=2,
    )
    footer_text = f"briefing-v3 · {args.date}"
    draw.text(
        (padding, footer_rule_y + 14),
        footer_text,
        font=font_footer,
        fill=TEXT_MUTED,
    )

    badge_text = "AI · v3"
    bw, bh = measure_text(draw, badge_text, font_badge)
    badge_padding_x, badge_padding_y = 14, 6
    badge_x2 = width - padding
    badge_y1 = footer_rule_y + 10
    badge_x1 = badge_x2 - bw - badge_padding_x * 2
    badge_y2 = badge_y1 + bh + badge_padding_y * 2
    draw.rounded_rectangle(
        [badge_x1, badge_y1, badge_x2, badge_y2],
        radius=8,
        fill=ACCENT,
    )
    draw.text(
        (badge_x1 + badge_padding_x, badge_y1 + badge_padding_y - 2),
        badge_text,
        font=font_badge,
        fill=(11, 18, 33),
    )

    # 8. Newspaper-style corner marks for a bit of character.
    draw_corner_marks(draw, width, height)

    # 9. Save. mkdir -p parent to survive a fresh checkout.
    out = os.path.abspath(args.output)
    os.makedirs(os.path.dirname(out), exist_ok=True)
    img.save(out, "PNG", optimize=True)

    print(f"OK: {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

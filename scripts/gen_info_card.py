#!/usr/bin/env python3
"""
gen_info_card.py — editorial info-card renderer for briefing-v3.

Produces two kinds of 1600x1600 PNG "info cards" on a newspaper-style
off-white background, inspired by the ai.hubtoday.app layout but
independently implemented:

  --mode item    one card per news item (main body of the briefing)
  --mode header  one card per issue (the "大字报" banner on top)

Input is a JSON payload on stdin describing the card. The JSON shape
is intentionally the same shape the Go infocard package produces so
the Go orchestrator can pipe straight through.

Example item JSON:

    {
      "main_title": "Anthropic Claude Sonnet 4.6",
      "subtitle": "一天连发编码 Agent + 托管基础设施",
      "intro": "Anthropic 在同一天宣布 Sonnet 4.6 ...",
      "hero_number": "4.6",
      "hero_label": "新版本号",
      "stat_numbers": [
        {"value": "$0.08/h", "label": "Agent 托管定价"},
        {"value": "3x", "label": "编码速度"}
      ],
      "key_points": [
        {"title": "编码", "desc": "智能体式编码能力"},
        {"title": "智能体", "desc": "自主执行多步任务"},
        {"title": "专业场景", "desc": "金融、法律、医疗"}
      ],
      "footer_summary": "头部公司已不止卖模型, 开始卖完整 Agent 环境",
      "brand_tag": "产品与功能更新",
      "category_tag": "MODEL"
    }

Example header JSON:

    {
      "issue_date": "2026-04-10",
      "main_headline": "Anthropic 重磅 · Claude 4.6 与 Agent 同日连发",
      "sub_headline": "OpenAI 下放安全刹车，Meta 改走闭源路线",
      "top_stories": [
        {"title": "Claude Sonnet/Opus 4.6 一天双发", "tag": "MODEL"},
        {"title": "Anthropic 托管 Agent 0.08/h", "tag": "AGENT"},
        {"title": "Meta Muse Spark 首个闭源", "tag": "STRATEGY"}
      ],
      "footer_slogan": "briefing-v3 · 每日早读"
    }
"""

import argparse
import json
import os
import sys
import textwrap
from pathlib import Path

try:
    from PIL import Image, ImageDraw, ImageFont, ImageFilter
except ImportError:
    print("ERROR: Pillow not installed", file=sys.stderr)
    sys.exit(2)


# ----- Colour + layout constants -----------------------------------------
# Newspaper-off-white background, deep navy text, warm accent red.

BG_MAIN = (246, 243, 236)    # F6F3EC warm cream
BG_PANEL = (238, 233, 221)   # EEE9DD slightly darker panel
BG_PANEL_DARK = (28, 30, 46) # 1C1E2E deep navy reversed panel
INK_MAIN = (22, 22, 22)      # near-black body text
INK_SOFT = (85, 85, 85)      # grey secondary text
INK_MUTED = (130, 130, 130)  # muted caption
ACCENT_RED = (193, 55, 42)   # editorial brand red
ACCENT_BLUE = (31, 58, 118)  # masthead blue
RULE = (55, 55, 55)          # horizontal rule colour


# ----- Font loading -------------------------------------------------------

def _find_sc_index(path):
    """Find the Simplified Chinese (SC) face index in a TTC collection.
    Returns 0 if not a TTC or SC face not found."""
    if not path.lower().endswith('.ttc'):
        return 0
    for i in range(20):
        try:
            f = ImageFont.truetype(path, size=20, index=i)
            name = f.getname()[0]
            if 'SC' in name:
                return i
        except Exception:
            break
    return 0

# Cache SC index per path to avoid repeated probing.
_sc_index_cache = {}

LINUX_BOLD_FONT_CANDIDATES = [
    "/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc",
    "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
    "/usr/share/fonts/opentype/noto/SourceHanSansSC-Bold.otf",
]

LINUX_REGULAR_FONT_CANDIDATES = [
    "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
    "/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc",
    "/usr/share/fonts/opentype/noto/SourceHanSansSC-Regular.otf",
]

MAC_BOLD_FONT_CANDIDATES = [
    "/System/Library/Fonts/STHeiti Medium.ttc",
    "/System/Library/Fonts/Hiragino Sans GB.ttc",
    "/System/Library/Fonts/PingFang.ttc",
]

MAC_REGULAR_FONT_CANDIDATES = [
    "/System/Library/Fonts/Hiragino Sans GB.ttc",
    "/System/Library/Fonts/STHeiti Light.ttc",
    "/System/Library/Fonts/PingFang.ttc",
]


def resolve_font_path(requested_path, role):
    """Return the first usable font path for this machine.

    Order:
    1. explicitly requested path
    2. role-specific macOS candidates
    3. role-specific Linux candidates
    """
    candidates = []
    if requested_path:
        candidates.append(requested_path)
    if role == "bold":
        candidates.extend(LINUX_BOLD_FONT_CANDIDATES)
        candidates.extend(MAC_BOLD_FONT_CANDIDATES)
    else:
        candidates.extend(LINUX_REGULAR_FONT_CANDIDATES)
        candidates.extend(MAC_REGULAR_FONT_CANDIDATES)

    seen = set()
    for raw in candidates:
        path = str(raw).strip()
        if not path or path in seen:
            continue
        seen.add(path)
        if os.path.exists(path):
            return path
    return requested_path

def load_font(path, size):
    """Load a TrueType font. For TTC collections, automatically selects
    the Simplified Chinese (SC) face instead of the default JP face.
    Returns PIL default font on failure so the script never crashes."""
    try:
        if path not in _sc_index_cache:
            _sc_index_cache[path] = _find_sc_index(path)
        idx = _sc_index_cache[path]
        return ImageFont.truetype(path, size=size, index=idx)
    except Exception:
        return ImageFont.load_default()


# ----- Text wrapping ------------------------------------------------------

def wrap_by_width(draw, text, font, max_width):
    """Smart wrap: ASCII alphanumeric runs are pulled as whole words
    (so "Spark" never gets split into "Spar" + "k"); CJK characters
    and punctuation still wrap per-char. If a single ASCII word is wider
    than max_width it falls back to char-level break for that word only.
    """
    if not text:
        return []
    out = []
    line = ""
    i = 0
    n = len(text)

    def measure(s):
        bbox = draw.textbbox((0, 0), s, font=font)
        return bbox[2] - bbox[0]

    while i < n:
        ch = text[i]
        if ch == "\n":
            out.append(line)
            line = ""
            i += 1
            continue
        # ASCII alnum: pull entire word as a unit so we don't split
        # "Spark" / "App" / "Anthropic" mid-word.
        if ord(ch) < 128 and ch.isalnum():
            j = i + 1
            while j < n and ord(text[j]) < 128 and (text[j].isalnum() or text[j] in "._'-"):
                j += 1
            word = text[i:j]
            i = j
            candidate = line + word
            if measure(candidate) > max_width and line.strip():
                out.append(line.rstrip())
                # If the word itself is wider than max_width, split it char-wise.
                if measure(word) > max_width:
                    chunk = ""
                    for w_ch in word:
                        test = chunk + w_ch
                        if measure(test) > max_width and chunk:
                            out.append(chunk)
                            chunk = w_ch
                        else:
                            chunk = test
                    line = chunk
                else:
                    line = word
            else:
                line = candidate
            continue
        # CJK / 标点 / 空格: 逐字符
        candidate = line + ch
        if measure(candidate) > max_width and line:
            out.append(line.rstrip() if line.endswith(" ") else line)
            line = "" if ch == " " else ch
        else:
            line = candidate
        i += 1
    if line:
        out.append(line)
    return out


def draw_wrapped(
    draw,
    xy,
    text,
    font,
    fill,
    max_width,
    line_spacing=1.25,
    max_lines=None,
    stroke_width=0,
    stroke_fill=None,
):
    """Draw wrapped text. Returns the y-coordinate just below the
    last drawn line."""
    lines = wrap_by_width(draw, text, font, max_width)
    if max_lines is not None and len(lines) > max_lines:
        lines = lines[:max_lines]
        # Append ellipsis to the last kept line if it was truncated.
        if lines:
            lines[-1] = lines[-1].rstrip() + "…"
    x, y = xy
    bbox = draw.textbbox((0, 0), "好", font=font)
    lh = int((bbox[3] - bbox[1]) * line_spacing)
    for line in lines:
        draw.text(
            (x, y),
            line,
            font=font,
            fill=fill,
            stroke_width=stroke_width,
            stroke_fill=stroke_fill or fill,
        )
        y += lh
    return y


# ----- Common chrome (masthead + footer bar) -----------------------------

def draw_masthead(draw, w, left_text, right_text, font, fg=INK_MAIN):
    """Thin top strip with left/right corner labels + horizontal rule."""
    pad_x = 56
    y = 46
    if left_text:
        draw.text((pad_x, y), left_text.upper(), font=font, fill=fg)
    if right_text:
        bbox = draw.textbbox((0, 0), right_text.upper(), font=font)
        draw.text((w - pad_x - (bbox[2] - bbox[0]), y),
                  right_text.upper(), font=font, fill=fg)
    # Rule below.
    bbox = draw.textbbox((0, 0), "A", font=font)
    rule_y = y + (bbox[3] - bbox[1]) + 18
    draw.line([(pad_x, rule_y), (w - pad_x, rule_y)], fill=RULE, width=2)


def draw_footer_bar(draw, w, h, left_text, right_text, font, fg=INK_MAIN):
    """Matching bottom strip with brand + technical label."""
    pad_x = 56
    y = h - 80
    draw.line([(pad_x, y), (w - pad_x, y)], fill=RULE, width=2)
    ty = y + 22
    if left_text:
        draw.text((pad_x, ty), left_text.upper(), font=font, fill=fg)
    if right_text:
        bbox = draw.textbbox((0, 0), right_text.upper(), font=font)
        draw.text((w - pad_x - (bbox[2] - bbox[0]), ty),
                  right_text.upper(), font=font, fill=fg)


# ----- Item card renderer -------------------------------------------------

def render_item_card(data, output_path, width, height,
                     font_bold_path, font_regular_path):
    """One 1600x1600 info card per news item."""
    img = Image.new("RGB", (width, height), BG_MAIN)
    draw = ImageDraw.Draw(img)

    # Font scale: nominal sizes at 1600 canvas; scale if caller
    # overrode width/height.
    scale = width / 1600

    f_mono = load_font(font_bold_path, int(24 * scale))
    f_title = load_font(font_bold_path, int(84 * scale))
    f_subtitle = load_font(font_bold_path, int(42 * scale))
    f_intro = load_font(font_regular_path, int(34 * scale))
    f_hero_num = load_font(font_bold_path, int(220 * scale))
    f_hero_lbl = load_font(font_regular_path, int(30 * scale))
    f_stat_num = load_font(font_bold_path, int(80 * scale))
    f_stat_lbl = load_font(font_regular_path, int(26 * scale))
    f_section_h = load_font(font_bold_path, int(40 * scale))
    f_pt_title = load_font(font_bold_path, int(34 * scale))
    f_pt_desc = load_font(font_regular_path, int(28 * scale))
    f_footer = load_font(font_regular_path, int(26 * scale))
    f_category = load_font(font_bold_path, int(28 * scale))

    # Masthead (brand tag + category tag).
    draw_masthead(
        draw, width,
        left_text=data.get("brand_tag", "BRIEFING · NEWS"),
        right_text=data.get("category_tag", ""),
        font=f_mono,
        fg=INK_MAIN,
    )

    pad_x = int(56 * scale)
    content_top = int(150 * scale)
    content_w = width - pad_x * 2

    # --- Main title (huge, deep navy) ---
    main_title = data.get("main_title") or "(missing title)"
    title_col_w = int(content_w * 0.58)
    hero_col_x = pad_x + title_col_w + int(40 * scale)

    y = content_top
    # Tag line above title (short thin rule + small accent mono)
    tag = data.get("category_tag", "").upper()
    if tag:
        draw.text((pad_x, y), tag, font=f_mono, fill=ACCENT_RED)
        draw.line(
            [(pad_x + int(120 * scale), y + int(14 * scale)),
             (pad_x + title_col_w, y + int(14 * scale))],
            fill=ACCENT_RED, width=2,
        )
        y += int(40 * scale)

    y = draw_wrapped(draw, (pad_x, y), main_title, f_title,
                     INK_MAIN, title_col_w, line_spacing=1.15, max_lines=3)
    y += int(16 * scale)

    # Subtitle (accent red, smaller)
    subtitle = data.get("subtitle", "")
    if subtitle:
        y = draw_wrapped(draw, (pad_x, y), subtitle, f_subtitle,
                         ACCENT_RED, title_col_w, line_spacing=1.2, max_lines=2)
        y += int(24 * scale)

    # Intro paragraph.
    intro = data.get("intro", "")
    if intro:
        y = draw_wrapped(draw, (pad_x, y), intro, f_intro,
                         INK_MAIN, title_col_w, line_spacing=1.5, max_lines=6)

    # --- Hero number (top-right column) ---
    hero_num = str(data.get("hero_number") or "")
    hero_lbl = data.get("hero_label", "")
    if hero_num:
        hero_col_w = width - hero_col_x - pad_x
        # Dark panel behind hero number
        panel_pad = int(26 * scale)
        panel_left = hero_col_x - panel_pad
        panel_top = content_top
        panel_bottom = content_top + int(380 * scale)
        panel_right = width - pad_x
        draw.rectangle(
            [panel_left, panel_top, panel_right, panel_bottom],
            fill=BG_PANEL_DARK,
        )
        # Number centred horizontally.
        num_font = f_hero_num
        # Autoshrink if it overflows the panel width.
        while num_font.size > int(90 * scale):
            bbox = draw.textbbox((0, 0), hero_num, font=num_font)
            if bbox[2] - bbox[0] <= panel_right - panel_left - int(48 * scale):
                break
            num_font = load_font(font_bold_path, num_font.size - 8)
        bbox = draw.textbbox((0, 0), hero_num, font=num_font)
        nx = panel_left + (panel_right - panel_left - (bbox[2] - bbox[0])) // 2
        ny = panel_top + int(50 * scale)
        draw.text((nx, ny), hero_num, font=num_font, fill=BG_MAIN)
        # Label below.
        if hero_lbl:
            bbox_lbl = draw.textbbox((0, 0), hero_lbl, font=f_hero_lbl)
            lx = panel_left + (panel_right - panel_left - (bbox_lbl[2] - bbox_lbl[0])) // 2
            ly = ny + (bbox[3] - bbox[1]) + int(30 * scale)
            draw.text((lx, ly), hero_lbl, font=f_hero_lbl, fill=(200, 200, 210))

    # --- Stat numbers row (below hero panel on the right) ---
    stats = data.get("stat_numbers") or []
    if stats and hero_num:
        stat_top = content_top + int(410 * scale)
        stat_area_x = hero_col_x - int(26 * scale)
        stat_area_w = width - stat_area_x - pad_x
        cell_w = stat_area_w // max(1, min(2, len(stats)))
        for i, s in enumerate(stats[:2]):
            cx = stat_area_x + i * cell_w + int(18 * scale)
            draw.text((cx, stat_top),
                      str(s.get("value", "")), font=f_stat_num, fill=INK_MAIN)
            label = s.get("label", "")
            if label:
                draw.text((cx, stat_top + int(90 * scale)),
                          label, font=f_stat_lbl, fill=INK_SOFT)

    # --- Key points panel (bottom half) ---
    points = data.get("key_points") or []
    if points:
        panel_top = int(height * 0.60)
        panel_bottom = int(height * 0.86)
        draw.rectangle([pad_x, panel_top, width - pad_x, panel_bottom],
                       fill=BG_PANEL)
        # Panel heading
        draw.text((pad_x + int(28 * scale), panel_top + int(28 * scale)),
                  "三大要点 · KEY POINTS", font=f_section_h, fill=INK_MAIN)
        draw.line(
            [(pad_x + int(28 * scale), panel_top + int(90 * scale)),
             (width - pad_x - int(28 * scale), panel_top + int(90 * scale))],
            fill=RULE, width=2,
        )
        n = min(3, len(points))
        if n > 0:
            col_w = (width - pad_x * 2 - int(56 * scale)) // n
            for i in range(n):
                pt = points[i]
                cx = pad_x + int(28 * scale) + i * col_w + int(20 * scale)
                cy = panel_top + int(120 * scale)
                # Divider between columns
                if i > 0:
                    draw.line(
                        [(cx - int(20 * scale), panel_top + int(110 * scale)),
                         (cx - int(20 * scale), panel_bottom - int(40 * scale))],
                        fill=(220, 215, 200), width=2,
                    )
                draw.text((cx, cy), pt.get("title", ""),
                          font=f_pt_title, fill=ACCENT_BLUE)
                cy += int(60 * scale)
                draw_wrapped(draw, (cx, cy), pt.get("desc", ""),
                             f_pt_desc, INK_MAIN,
                             col_w - int(40 * scale),
                             line_spacing=1.45, max_lines=5)

    # --- Footer summary line (just above footer bar) ---
    footer_sum = data.get("footer_summary", "")
    if footer_sum:
        y = int(height * 0.88)
        draw_wrapped(draw, (pad_x, y), footer_sum, f_footer,
                     INK_SOFT, content_w, line_spacing=1.4, max_lines=2)

    # --- Footer bar ---
    draw_footer_bar(
        draw, width, height,
        left_text="briefing-v3 · editorial",
        right_text="INFO CARD · 1600 × 1600",
        font=f_mono,
    )

    img.save(output_path, "PNG", optimize=True)


# ----- Header card (page hero) -------------------------------------------

def render_header_card(data, output_path, width, height,
                       font_bold_path, font_regular_path):
    """Hero 大字报 — newspaper-style 左右分栏 layout.

    v1.0.0 hubtoday-参考版: 学习 source.hubtoday.app 的排版精神
    1) 左右分栏 (LEFT 62% / RIGHT 38%, col_gap)
    2) 多个 named sections, 每 section 一个红色 LABEL + 内容
    3) 数字 cell 大字号 + 小标签
    4) section 之间 horizontal rule 分隔
    5) 高密度无大段空白

    布局区域 (1600x1600):
       0- 60   masthead bar
      80-150   date + edition meta line + rule
     150-720   主区   LEFT  L1 巨字 + 导语 lead
                      RIGHT [今日要闻] L2 + [次要看点] L3
     720-740   rule
     740-1100  中区   LEFT  TOP STORIES 6 entries 2×3 grid
                      RIGHT BY THE NUMBERS 4 cells 2×2 grid
    1100-1130  rule
    1130-1480  下区   LEFT  MORE STORIES 3 stories list
                      RIGHT 今日板块速览 5 sections + count
    1480-1600  footer bar
    """
    img = Image.new("RGB", (width, height), BG_MAIN)
    draw = ImageDraw.Draw(img)

    scale = width / 1600

    # ---- Fonts ----
    f_mono = load_font(font_bold_path, int(26 * scale))
    f_edition = load_font(font_bold_path, int(32 * scale))
    f_date = load_font(font_bold_path, int(40 * scale))
    f_l1 = load_font(font_bold_path, int(64 * scale))                # L1 main headline (左大栏) 64 让 1 行容纳更多字
    f_lead = load_font(font_regular_path, int(26 * scale))           # 导语
    f_l2 = load_font(font_bold_path, int(40 * scale))                # L2 right top (44→40 让 1 行容纳更多)
    f_l3 = load_font(font_bold_path, int(30 * scale))                # L3 right top (36→30 防截断)
    f_section_label = load_font(font_bold_path, int(22 * scale))     # 红色 SECTION LABEL
    f_story_tag = load_font(font_bold_path, int(20 * scale))         # 故事 tag
    f_story_title = load_font(font_bold_path, int(28 * scale))       # 中区故事 title
    f_keynum_value = load_font(font_bold_path, int(72 * scale))      # 数字 cell 大字
    f_keynum_label = load_font(font_regular_path, int(20 * scale))   # 数字 cell label
    f_more_story = load_font(font_bold_path, int(26 * scale))        # 下区 more stories
    f_section_name = load_font(font_bold_path, int(28 * scale))      # 板块名
    f_section_count = load_font(font_bold_path, int(28 * scale))     # 板块计数 34→28 跟 name 对齐

    # ---- Masthead ----
    draw_masthead(
        draw, width,
        left_text="BRIEFING-V3 · DAILY",
        right_text="AI INSIGHT DAILY",
        font=f_mono,
    )

    pad_x = int(56 * scale)
    content_w = width - pad_x * 2

    # 左右分栏几何
    col_gap = int(40 * scale)
    left_w = int(content_w * 0.62)
    right_x = pad_x + left_w + col_gap
    right_w = width - pad_x - right_x

    # ===== HEADER META: red bar | date | edition =====
    y = int(112 * scale)
    draw.rectangle(
        [pad_x, y + int(6 * scale), pad_x + int(12 * scale), y + int(46 * scale)],
        fill=ACCENT_RED,
    )
    issue_date = data.get("issue_date", "")
    draw.text((pad_x + int(28 * scale), y), issue_date.upper(),
              font=f_date, fill=ACCENT_RED)
    edition = data.get("edition", "").strip()
    if edition:
        edition_bbox = draw.textbbox((0, 0), edition, font=f_edition)
        edition_w_px = edition_bbox[2] - edition_bbox[0]
        draw.text(
            (width - pad_x - edition_w_px, y + int(4 * scale)),
            edition,
            font=f_edition,
            fill=INK_SOFT,
        )
    y += int(72 * scale)
    draw.line([(pad_x, y), (width - pad_x, y)], fill=RULE, width=3)
    y += int(28 * scale)

    # ===== MAIN ZONE: LEFT L1 + lead, RIGHT L2 + L3 =====
    main_zone_top = y

    # LEFT col: L1 + lead
    left_y = main_zone_top
    headline = data.get("main_headline") or "AI 资讯日报"
    left_y = draw_wrapped(
        draw, (pad_x, left_y), headline, f_l1, INK_MAIN,
        left_w, line_spacing=1.08, max_lines=3,
    )
    left_y += int(18 * scale)
    lead = data.get("lead_paragraph", "").strip()
    if lead:
        left_y = draw_wrapped(
            draw, (pad_x, left_y), lead, f_lead, INK_MAIN,
            left_w, line_spacing=1.40, max_lines=7,
        )

    # RIGHT col: 2 highlights with red labels
    right_y = main_zone_top
    sub_raw = data.get("sub_headline", "")
    sub_lines = [s.strip() for s in sub_raw.split("\n") if s.strip()]
    highlight_specs = [
        ("今日要闻", f_l2, INK_MAIN, 3),
        ("次要看点", f_l3, INK_MAIN, 3),
    ]
    for i, (label, font, color, ml) in enumerate(highlight_specs):
        if i >= len(sub_lines):
            break
        draw.text((right_x, right_y), label,
                  font=f_section_label, fill=ACCENT_RED)
        right_y += int(30 * scale)
        right_y = draw_wrapped(
            draw, (right_x, right_y), sub_lines[i], font, color,
            right_w, line_spacing=1.18, max_lines=ml,
        )
        right_y += int(36 * scale)  # 28→36 副标题两段间距加宽

    # 主区底部对齐 (取较深的列)
    y = max(left_y, right_y) + int(24 * scale)
    draw.line([(pad_x, y), (width - pad_x, y)], fill=RULE, width=3)
    y += int(24 * scale)

    # ===== MID ZONE: LEFT TOP STORIES 2×3 grid, RIGHT BY THE NUMBERS 2×2 grid =====
    mid_zone_top = y
    stories = data.get("top_stories") or []
    key_numbers = data.get("key_numbers") or []

    # LEFT: TOP STORIES (6 in 2 cols × 3 rows)
    left_y = mid_zone_top
    draw.text((pad_x, left_y), "TOP STORIES", font=f_section_label, fill=ACCENT_RED)
    left_y += int(34 * scale)

    # 用户反馈"截断到一半看不懂": 改回 6 (2×3) 但加大 row_h 让每 cell
    # 能容纳 3 行 wrap, title 长 60 字至少能一句话讲清楚.
    grid_stories = stories[:6]
    if grid_stories:
        col_inner_gap = int(28 * scale)
        cell_w = (left_w - col_inner_gap) // 2
        row_h = int(140 * scale)  # 96 → 140, 容纳 tag + 3 行 title
        for i, st in enumerate(grid_stories):
            row = i // 2
            col = i % 2
            cx = pad_x + col * (cell_w + col_inner_gap)
            cy = left_y + row * row_h
            tag = (st.get("tag") or "").upper()
            if tag:
                draw.text((cx, cy), tag, font=f_story_tag, fill=ACCENT_RED)
                cy += int(26 * scale)
            draw_wrapped(
                draw, (cx, cy), st.get("title", ""),
                f_story_title, INK_MAIN, cell_w,
                line_spacing=1.20, max_lines=3,
            )
        n_rows = (len(grid_stories) + 1) // 2
        left_y += n_rows * row_h

    # RIGHT: BY THE NUMBERS (4 cells in 2×2 grid)
    right_y = mid_zone_top
    draw.text((right_x, right_y), "BY THE NUMBERS", font=f_section_label, fill=ACCENT_RED)
    right_y += int(34 * scale)

    grid_nums = key_numbers[:6]  # 4 → 6 (2×3 grid 加密)
    if grid_nums:
        num_col_gap = int(20 * scale)
        num_cell_w = (right_w - num_col_gap) // 2
        num_row_h = int(142 * scale)  # 128 → 142 中文数字比英文宽, 垂直多留点
        # value 字号自适应: LLM 生成的 value 可能是长字符串 (e.g. "100 亿美元",
        # "27B@Q4"), 固定 78px 会撑出 cell 宽度跟旁边重叠. 测量实际宽度,
        # 太宽就降级到更小字号.
        value_size_steps = [48, 40, 34, 28, 24]  # 再降: 中文数字 3-4 字宽, 48 起步够醒目
        value_fonts = [load_font(font_bold_path, int(s * scale)) for s in value_size_steps]
        max_value_w = num_cell_w - int(8 * scale)
        for i, kn in enumerate(grid_nums):
            row = i // 2
            col = i % 2
            cx = right_x + col * (num_cell_w + num_col_gap)
            cy = right_y + row * num_row_h
            value = (kn.get("value") or "").strip()
            label = (kn.get("label") or "").strip()
            if value:
                # 选第一个能塞下的字号
                vfont = value_fonts[-1]
                for f in value_fonts:
                    bbox = draw.textbbox((0, 0), value, font=f)
                    if bbox[2] - bbox[0] <= max_value_w:
                        vfont = f
                        break
                draw.text((cx, cy), value, font=vfont, fill=ACCENT_BLUE)
            if label:
                draw.text((cx, cy + int(76 * scale)), label,
                          font=f_keynum_label, fill=INK_SOFT)
        n_num_rows = (len(grid_nums) + 1) // 2
        right_y += n_num_rows * num_row_h

    y = max(left_y, right_y) + int(24 * scale)
    draw.line([(pad_x, y), (width - pad_x, y)], fill=RULE, width=3)
    y += int(24 * scale)

    # ===== BOTTOM ZONE: LEFT MORE STORIES, RIGHT 今日板块速览 =====
    bot_zone_top = y

    # MORE STORIES — 占满整个 bot zone width (full content_w).
    # 板块速览已删 (用户原话: "中间那一列看不懂, 跟 MORE STORIES 不对应"),
    # 把全局板块条目数信息让给 BY THE NUMBERS 的统计 cell, MORE STORIES
    # 独占整行不再分栏.
    left_y = bot_zone_top
    draw.text((pad_x, left_y), "MORE STORIES", font=f_section_label, fill=ACCENT_RED)
    left_y += int(36 * scale)

    # MORE STORIES: 2 列 × 4 行 (左右各 4 条) 填满底部, 不留空白.
    # 自适应行距: footer 前剩余空间 / 行数, 均匀撑满.
    f_more_story_lg = load_font(font_bold_path, int(28 * scale))
    more_stories = stories[6:14]
    footer_y = int(1480 * scale)
    n_per_col = 4
    n_stories = min(8, len(more_stories))
    avail_h = footer_y - left_y - int(16 * scale)
    row_h = max(int(44 * scale), avail_h // n_per_col) if n_per_col > 0 else int(50 * scale)

    ms_col_gap = int(36 * scale)
    ms_col_w = (content_w - ms_col_gap) // 2

    def _draw_more_story(st, cx, cy, col_w):
        tag = (st.get("tag") or "").strip()
        title = (st.get("title") or "").strip()
        if tag:
            tag_text = tag + " · "
            tag_bbox = draw.textbbox((0, 0), tag_text, font=f_more_story_lg)
            tag_w = tag_bbox[2] - tag_bbox[0]
            draw.text((cx, cy), tag_text, font=f_more_story_lg, fill=ACCENT_RED)
            title_x = cx + tag_w
            avail_w = col_w - tag_w
        else:
            title_x = cx
            avail_w = col_w
        title_runes = list(title)
        fit_title = title
        for cut in range(len(title_runes), 0, -1):
            tt = "".join(title_runes[:cut])
            if cut < len(title_runes):
                tt += "…"
            tbbox = draw.textbbox((0, 0), tt, font=f_more_story_lg)
            if tbbox[2] - tbbox[0] <= avail_w:
                fit_title = tt
                break
        draw.text((title_x, cy), fit_title, font=f_more_story_lg, fill=INK_MAIN)

    # 左列: stories 0-3, 右列: stories 4-7
    for i, st in enumerate(more_stories[:n_stories]):
        col = i // n_per_col  # 0=左, 1=右
        row = i % n_per_col
        cx = pad_x + col * (ms_col_w + ms_col_gap)
        cy = left_y + row * row_h
        _draw_more_story(st, cx, cy, ms_col_w)

    # RIGHT 板块速览已删 — MORE STORIES 占满整个 bot zone, 视觉上不再
    # 出现 "row-by-row 对齐误解" 的问题. 各板块条目数信息可以用 BY THE
    # NUMBERS 的统计 cell 表达 (e.g. 1 个 cell 显示总条目数).
    pass

    # ===== Footer bar =====
    draw_footer_bar(
        draw, width, height,
        left_text="briefing-v3 · hero",
        right_text="HEADLINE · 1600 × 1600",
        font=f_mono,
    )

    img.save(output_path, "PNG", optimize=True)


# ----- CLI ---------------------------------------------------------------

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--mode", choices=["item", "header"], required=True)
    ap.add_argument("--output", required=True)
    ap.add_argument("--width", type=int, default=1600)
    ap.add_argument("--height", type=int, default=1600)
    ap.add_argument("--font-bold",
                    default="/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc")
    ap.add_argument("--font-regular",
                    default="/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc")
    ap.add_argument("--json-file",
                    help="read JSON from file instead of stdin (optional)")
    args = ap.parse_args()

    # Load JSON payload
    if args.json_file:
        with open(args.json_file, "r", encoding="utf-8") as f:
            data = json.load(f)
    else:
        data = json.load(sys.stdin)

    os.makedirs(os.path.dirname(args.output) or ".", exist_ok=True)

    if args.mode == "item":
        font_bold = resolve_font_path(args.font_bold, "bold")
        font_regular = resolve_font_path(args.font_regular, "regular")
        render_item_card(
            data, args.output, args.width, args.height,
            font_bold, font_regular,
        )
    else:
        font_bold = resolve_font_path(args.font_bold, "bold")
        font_regular = resolve_font_path(args.font_regular, "regular")
        render_header_card(
            data, args.output, args.width, args.height,
            font_bold, font_regular,
        )

    print(f"OK: {args.output}", file=sys.stdout)


if __name__ == "__main__":
    main()

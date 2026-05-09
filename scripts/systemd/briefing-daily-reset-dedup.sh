#!/bin/bash
# briefing-daily-reset-dedup.sh
#
# v1.0.1 Phase 4.6 (2026-04-16): "彻底去重" 模式 — 只去重不 trim.
# 用户决策: opensource/news 本来就该是新内容, 推过就不再重推.
# 例外: 如果同一 URL 标题变了 (如出新版本 v2.0), sent_titles 的相似度
# dedup 会放行.
#
# 文件大小估算: ~300 条/天 × 365 天 × 100 字节 = ~10MB/年, 完全可接受.

for FILE in /root/briefing-v3/data/sent_urls.txt /root/briefing-v3/data/sent_titles.txt; do
    if [[ ! -f "$FILE" ]]; then continue; fi
    BEFORE=$(wc -l < "$FILE" 2>/dev/null || echo 0)
    sort -u "$FILE" > "$FILE.tmp" && mv "$FILE.tmp" "$FILE"
    AFTER=$(wc -l < "$FILE")
    echo "[dedup-permanent] $(basename $FILE): $BEFORE → $AFTER lines (only sort -u, no trim)"
done

#!/bin/bash
# briefing-v3 数据导出工具
#
# 用法:
#   ./scripts/export_data.sh                  # 导出全部 (CSV + JSON + SQL + ZIP)
#   ./scripts/export_data.sh --csv            # 只导 CSV
#   ./scripts/export_data.sh --json           # 只导 JSON
#   ./scripts/export_data.sh --sql            # 只导 SQL dump
#   ./scripts/export_data.sh --zip            # 只导一键打包 zip
#   ./scripts/export_data.sh --date 2026-04-11  # 只导某一天
#
# 输出目录: /root/briefing-v3/data/export/YYYY-MM-DD-HHMM/

set -e

cd /root/briefing-v3
DB=data/briefing.db
TS=$(date '+%Y-%m-%d-%H%M')
OUT=data/export/$TS
mkdir -p "$OUT"

DO_CSV=true
DO_JSON=true
DO_SQL=true
DO_ZIP=true
DATE_FILTER=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --csv)  DO_CSV=true; DO_JSON=false; DO_SQL=false; DO_ZIP=false ;;
        --json) DO_CSV=false; DO_JSON=true; DO_SQL=false; DO_ZIP=false ;;
        --sql)  DO_CSV=false; DO_JSON=false; DO_SQL=true; DO_ZIP=false ;;
        --zip)  DO_CSV=false; DO_JSON=false; DO_SQL=false; DO_ZIP=true ;;
        --date) DATE_FILTER="$2"; shift ;;
        *) echo "未知参数: $1"; exit 1 ;;
    esac
    shift
done

WHERE=""
if [[ -n "$DATE_FILTER" ]]; then
    WHERE="WHERE issue_date='$DATE_FILTER'"
    echo "[export] 过滤日期: $DATE_FILTER"
fi

echo "[export] 输出目录: $OUT"

# === CSV 导出 (Excel 友好) ===
if $DO_CSV; then
    echo "[export] CSV ..."
    for tbl in issues issue_items issue_insights raw_items deliveries sources; do
        sqlite3 -header -csv "$DB" "SELECT * FROM $tbl" > "$OUT/$tbl.csv"
        rows=$(($(wc -l < "$OUT/$tbl.csv") - 1))
        printf "  %-20s %s rows\n" "$tbl.csv" "$rows"
    done
fi

# === JSON 导出 (程序友好) ===
if $DO_JSON; then
    echo "[export] JSON ..."
    python3 - <<PY
import sqlite3, json, os
con = sqlite3.connect("$DB")
con.row_factory = sqlite3.Row
out = "$OUT"
where = """$WHERE"""

def dump(table, query=None):
    q = query or f"SELECT * FROM {table}"
    rows = [dict(r) for r in con.execute(q)]
    with open(os.path.join(out, f"{table}.json"), "w", encoding="utf-8") as f:
        json.dump(rows, f, ensure_ascii=False, indent=2, default=str)
    print(f"  {table+'.json':<25} {len(rows)} rows")

if where:
    issue_id_q = f"SELECT id FROM issues {where}"
    issue_ids = [r[0] for r in con.execute(issue_id_q)]
    if not issue_ids:
        print(f"  无匹配 issue: {where}")
    else:
        ids_str = ",".join(str(i) for i in issue_ids)
        dump("issues", f"SELECT * FROM issues WHERE id IN ({ids_str})")
        dump("issue_items", f"SELECT * FROM issue_items WHERE issue_id IN ({ids_str})")
        dump("issue_insights", f"SELECT * FROM issue_insights WHERE issue_id IN ({ids_str})")
        dump("deliveries", f"SELECT * FROM deliveries WHERE issue_id IN ({ids_str})")
else:
    dump("issues")
    dump("issue_items")
    dump("issue_insights")
    dump("deliveries")
    dump("sources")
    dump("domains")
con.close()
PY
fi

# === SQL dump (完整 schema + data) ===
if $DO_SQL; then
    echo "[export] SQL dump ..."
    sqlite3 "$DB" .dump > "$OUT/full.sql"
    bytes=$(wc -c < "$OUT/full.sql")
    echo "  full.sql              $bytes bytes"
fi

# === ZIP 一键打包 (sqlite + markdown + html + 图 + json) ===
if $DO_ZIP; then
    echo "[export] 一键 zip 打包 ..."
    ZIP=$OUT/briefing-v3-snapshot-$TS.zip
    cd /root
    zip -qr "$OLDPWD/$ZIP" \
        briefing-v3/data/briefing.db \
        briefing-v3/data/sent_urls.txt \
        briefing-v3/daily/ \
        briefing-v3/data/slack-payload-*.json \
        briefing-v3/data/images/cards/ \
        ai-daily-site/content/cn/ \
        ai-daily-site/public/2026/ 2>/dev/null || true
    cd /root/briefing-v3
    if [[ -f "$ZIP" ]]; then
        bytes=$(wc -c < "$ZIP")
        mb=$((bytes / 1024 / 1024))
        echo "  $(basename $ZIP)  ${mb} MB"
    fi
fi

echo ""
echo "[export] 完成. 输出在: $OUT"
ls -la "$OUT" | tail -20

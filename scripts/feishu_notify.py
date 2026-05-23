#!/usr/bin/env python3
"""Send daily briefing card to Feishu bot webhook."""
import json, urllib.request, ssl, re, os, sys

WEBHOOK = os.environ.get("FEISHU_WEBHOOK", "https://open.feishu.cn/open-apis/bot/v2/hook/b69fabf6-5922-4460-9055-c73322301316")
SITE_URL = os.environ.get("SITE_URL", "https://zhuxinfei.github.io/ai-daily-site")
FILE = sys.argv[1] if len(sys.argv) > 1 else None
if not FILE or not os.path.exists(FILE):
    print(f"File not found: {FILE}")
    sys.exit(1)

with open(FILE) as f:
    content = f.read()

# Extract date
date_match = re.search(r'(\d{4}/\d{1,2}/\d{1,2})', content)
date_str = date_match.group(1) if date_match else ""

# Build URL
url_date = date_str.replace("/", "/").replace("2026/", "2026/2026-") if date_str else ""
detail_url = f"{SITE_URL}/{url_date.replace('/', '/2026-')}/" if date_str else SITE_URL

# Extract sections
def extract_section(text, header):
    pattern = rf'###\s+.*?{header}.*?\n(.*?)(?=###\s+|$)'
    m = re.search(pattern, text, re.DOTALL)
    return m.group(1).strip()[:1500] if m else ""

summary = extract_section(content, "今日摘要")
insight = extract_section(content, "行业洞察")
takeaways = extract_section(content, "对我们的启发")

# Clean markdown symbols
def clean_md(text):
    text = re.sub(r'\*\*', '', text)
    text = re.sub(r'\[([^\]]+)\]\([^)]+\)', r'\1', text)
    text = re.sub(r'[#*>]', '', text)
    text = text.replace('`', '')
    return text.strip()

# Build Feishu card
card = {
    "msg_type": "interactive",
    "card": {
        "config": {"wide_screen_mode": True},
        "header": {
            "title": {"tag": "plain_text", "content": f"📰 AI资讯日报 {date_str}"},
            "template": "blue"
        },
        "elements": [
            {
                "tag": "div",
                "text": {"tag": "lark_md", "content": f"**📋 今日摘要**\n{clean_md(summary)[:500]}"}
            },
            {"tag": "hr"},
            {
                "tag": "div",
                "text": {"tag": "lark_md", "content": f"**📊 行业洞察**\n{clean_md(insight)[:800]}"}
            },
            {"tag": "hr"},
            {
                "tag": "div",
                "text": {"tag": "lark_md", "content": f"**💭 对我们的启发**\n{clean_md(takeaways)[:800]}"}
            },
            {"tag": "hr"},
            {
                "tag": "note",
                "elements": [
                    {"tag": "lark_md", "content": f"🔗 [查看完整日报]({detail_url})"}
                ]
            }
        ]
    }
}

ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
req = urllib.request.Request(WEBHOOK, data=json.dumps(card).encode(),
    headers={"Content-Type": "application/json"})
resp = urllib.request.urlopen(req, timeout=10, context=ctx)
result = json.loads(resp.read())
print(f"Feishu: {result.get('code')} — {result.get('msg', 'ok')}")

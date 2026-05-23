#!/usr/bin/env python3
"""Send daily briefing card to Feishu bot webhook."""
import json, urllib.request, ssl, re, os, sys

WEBHOOK = os.environ.get("FEISHU_WEBHOOK", "")
SITE_URL = "https://zhuxinfei.github.io/ai-daily-site"
FILE = sys.argv[1] if len(sys.argv) > 1 else None

if not FILE or not os.path.exists(FILE):
    print(f"File not found: {FILE}")
    sys.exit(1)

with open(FILE) as f:
    content = f.read()

# Extract date for URL
date_match = re.search(r'(\d{4}/\d{1,2}/\d{1,2})', content)
date_str = date_match.group(1) if date_match else ""
if date_str:
    parts = date_str.split("/")
    yy, mm, dd = parts[0], parts[1].zfill(2), parts[2].zfill(2)
    detail_url = SITE_URL + "/" + yy + "/" + yy + "-" + mm + "/" + yy + "-" + mm + "-" + dd + "/"
else:
    detail_url = SITE_URL + "/"

# Extract sections
def extract_section(text, header):
    lines = text.split('\n')
    in_section = False
    result = []
    for line in lines:
        if header in line and line.strip().startswith('#'):
            in_section = True
            continue
        if in_section:
            if line.strip().startswith('###') or line.strip().startswith('---'):
                break
            if line.strip().startswith('> 本周周报'):
                break
            result.append(line)
    return '\n'.join(result).strip()

summary = extract_section(content, "今日摘要")
insight = extract_section(content, "行业洞察")
takeaways = extract_section(content, "对我们的启发")

# Clean markdown for Feishu card
def clean_for_feishu(text):
    # Remove code fences
    text = re.sub(r'```[a-z]*\n?', '', text)
    text = re.sub(r'\n```', '', text)
    # Keep bold markers for Feishu lark_md
    # Strip trailing spaces but keep intentional line breaks
    text = re.sub(r'  \n', '\n\n', text)  # double-space newlines -> paragraph break
    # Remove image references
    text = re.sub(r'!\[.*?\]\(.*?\)', '', text)
    # Remove link syntax but keep text: [text](url) -> text
    text = re.sub(r'\[([^\]]+)\]\([^)]+\)', r'\1', text)
    # Remove single # headers
    text = re.sub(r'^#+\s+.*$', '', text, flags=re.MULTILINE)
    # Remove "本周周报" line
    text = re.sub(r'> 本周周报.*$', '', text)
    # Collapse 3+ newlines to 2
    text = re.sub(r'\n{3,}', '\n\n', text)
    return text.strip()

summary = clean_for_feishu(summary)
insight = clean_for_feishu(insight)
takeaways = clean_for_feishu(takeaways)

# Build Feishu card elements
elements = []

if summary:
    elements.append({
        "tag": "div",
        "text": {"tag": "lark_md", "content": f"**📋 今日摘要**\n{summary[:600]}"}
    })
    elements.append({"tag": "hr"})

if insight:
    elements.append({
        "tag": "div",
        "text": {"tag": "lark_md", "content": f"**📊 行业洞察**\n{insight[:1000]}"}
    })
    elements.append({"tag": "hr"})

if takeaways:
    elements.append({
        "tag": "div",
        "text": {"tag": "lark_md", "content": f"**💭 对我们的启发**\n{takeaways[:1000]}"}
    })
    elements.append({"tag": "hr"})

elements.append({
    "tag": "note",
    "elements": [
        {"tag": "lark_md", "content": f"🔗 [查看完整日报]({detail_url})"}
    ]
})

card = {
    "msg_type": "interactive",
    "card": {
        "config": {"wide_screen_mode": True},
        "header": {
            "title": {"tag": "plain_text", "content": f"📰 AI资讯日报 {date_str}"},
            "template": "blue"
        },
        "elements": elements
    }
}

ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
req = urllib.request.Request(WEBHOOK, data=json.dumps(card).encode(),
    headers={"Content-Type": "application/json"})
resp = urllib.request.urlopen(req, timeout=10, context=ctx)
result = json.loads(resp.read())
print(f"Feishu: code={result.get('code')} — {result.get('msg', 'ok')}")

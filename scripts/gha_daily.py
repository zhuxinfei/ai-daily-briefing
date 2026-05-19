#!/usr/bin/env python3
"""AI Daily Briefing — GHA self-contained pipeline. RSS→LLM→push, ~30s."""

import json, base64, urllib.request, ssl, re, os, datetime, time

API = "https://api.gjs.ink/v1/chat/completions"
KEY = os.environ["OPENAI_API_KEY"]
MODEL = os.environ.get("OPENAI_MODEL", "gpt-5.4")
GH_TOKEN = os.environ["AI_DAILY_SITE_PUSH_TOKEN"]
GH_REPO = os.environ.get("GH_REPO", "zhuxinfei/ai-daily-site")

FEEDS = [
    "https://news.smol.ai/rss.xml",
    "https://the-decoder.com/feed/",
    "https://techcrunch.com/category/artificial-intelligence/feed/",
    "https://openai.com/news/rss.xml",
    "https://simonwillison.net/atom/everything/",
    "https://huggingface.co/blog/feed.xml",
    "https://rss.arxiv.org/rss/cs.AI",
]

ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE

def llm(system, user, max_tokens=6000):
    body = json.dumps({
        "model": MODEL,
        "messages": [{"role": "system", "content": system}, {"role": "user", "content": user}],
        "temperature": 0.3, "max_tokens": max_tokens
    }).encode()
    for attempt in range(3):
        try:
            req = urllib.request.Request(API, data=body, headers={
                "Content-Type": "application/json", "Authorization": f"Bearer {KEY}"
            })
            resp = urllib.request.urlopen(req, timeout=120, context=ctx)
            return json.loads(resp.read())["choices"][0]["message"]["content"]
        except Exception as e:
            print(f"  LLM attempt {attempt+1}/3 failed: {e}")
            if attempt < 2:
                time.sleep(10)
    raise Exception("LLM failed after 3 attempts")

tz_shanghai = datetime.timezone(datetime.timedelta(hours=8))
now = datetime.datetime.now(tz_shanghai)
today = now.date().isoformat()
today_zh = now.strftime("%Y/%m/%d")
print(f"[{today}] AI Daily Briefing")

# Step 1: Fetch RSS
articles = []
for url in FEEDS:
    try:
        req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0"})
        resp = urllib.request.urlopen(req, timeout=10, context=ctx)
        xml = resp.read().decode("utf-8", errors="ignore")
        items = re.findall(r'<item>[\s\S]*?</item>', xml) + re.findall(r'<entry>[\s\S]*?</entry>', xml)
        for block in items[:4]:
            title_m = re.search(r'<title[^>]*>([\s\S]*?)</title>', block, re.I)
            if not title_m: continue
            title = re.sub(r'<!\[CDATA\[|\]\]>', '', title_m.group(1)).strip()
            title = re.sub(r'<[^>]+>', '', title)
            link_m = re.search(r'<link[^>]*href="([^"]*)"', block, re.I) or re.search(r'<link>([\s\S]*?)</link>', block, re.I)
            link = link_m.group(1).strip() if link_m else url
            articles.append(f"{title} | {link} | {url.split('/')[2]}")
    except Exception as e:
        pass

# Dedup
seen = set()
articles = [a for a in articles if not (a[:50].lower() in seen or seen.add(a[:50].lower()))]
print(f"  {len(articles)} articles")
if len(articles) < 8:
    print("Too few articles, exit")
    exit(0)

# Step 2: LLM sections
print("  LLM: sections...")
sections_prompt = f"""你是专业的AI日报编辑。根据以下AI新闻标题生成中文日报。

格式: ### 🔵 产品与功能更新 / 🟢 前沿研究 / 🟡 行业展望 / 🟣 开源TOP / 🔴 社媒分享
每条: "1. **粗体标题**\n3-5句分析\n[锚文本](URL)"
专业名词加括号注释,每板块3-5条,严格来源于原材料。

新闻标题:
{chr(10).join(articles[:60])}"""

sys = "你是专业的AI日报编辑。面向非技术读者,专业名词必须加括号注释。严格按格式输出。"
sections = llm(sys, sections_prompt, 6000)
print(f"  sections: {len(sections)}c")

# Step 3: LLM insight
print("  LLM: insight...")
insight_sys = "你是资深AI行业分析师。输出3-4条行业洞察,用嵌套格式,专业名词加括号注释。至少2条整合多条新闻。"
insight = llm(insight_sys, f"以下为今日AI日报。请输出📊行业洞察(3-4条)和🗺️mermaid关系图。\n\n{sections[:5000]}", 3000)
print(f"  insight: {len(insight)}c")

# Step 4: Assemble & push
summary = llm("", f"用3句话概括以下AI日报要闻:\n{sections[:2000]}", 300)

body = f"## **今日摘要**\n\n{summary}\n\n{sections}"
if insight:
    body += f"\n\n---\n\n{insight}"

desc = body[:120].replace('"', '\\"').replace('\n', ' ')

markdown = f"""---
linkTitle: "{now.strftime('%m-%d')} AI资讯"
title: "AI资讯日报 {today_zh}"
weight: {datetime.date.today().day}
breadcrumbs: false
comments: false
description: "{desc}..."
---

> AI 早报 · 每日早读 · 全网深度聚合

{body}"""

y, m, d = today.split("-")
file_path = f"content/cn/{y}/{y}-{m}/{today}.md"
api_url = f"https://api.github.com/repos/{GH_REPO}/contents/{file_path}"

# Check if exists
try:
    check_req = urllib.request.Request(api_url, headers={
        "Authorization": f"token {GH_TOKEN}", "User-Agent": "AI-Daily"
    })
    sha = json.loads(urllib.request.urlopen(check_req, timeout=10, context=ctx).read()).get("sha")
except:
    sha = None

push_body = json.dumps({
    "message": f"chore: daily briefing {today}",
    "content": base64.b64encode(markdown.encode("utf-8")).decode("ascii"),
    **({"sha": sha} if sha else {})
}).encode()

push_req = urllib.request.Request(api_url, data=push_body, method="PUT", headers={
    "Authorization": f"token {GH_TOKEN}",
    "Content-Type": "application/json",
    "User-Agent": "AI-Daily"
})
resp = urllib.request.urlopen(push_req, timeout=15, context=ctx)
result = json.loads(resp.read())
print(f"✅ Published: {file_path}")
print(f"   https://zhuxinfei.github.io/ai-daily-site/{y}/{y}-{m}/{today}/")

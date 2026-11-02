"""
mitmdump addon：抓取 Qoder CN 签到链路
用法: mitmdump -s capture_addon.py -p 8080
"""
import json
import time
from pathlib import Path

CAPTURE_DIR = Path(__file__).parent / "capture"
CAPTURE_DIR.mkdir(exist_ok=True)

TARGET_DOMAINS = [
    # 国内版
    "qoder.com.cn", "qoder.cn",
    # 国际版（qoder.com / openapi.qoder.sh / api*.qoder.sh）
    "qoder.sh", "qoder.com",
    # 阿里云基础设施（活动页静态资源等）
    "aliyuncs.com", "aliyun.com", "alicdn.com",
]
KEYWORDS = [
    "activity", "credit", "claim", "gift", "reward", "campaign",
    "signin", "checkin", "check-in", "daily", "bonus", "event",
    "100credits", "usage", "quota",
    # 国际版（qoder.com）签到体系
    "seat-activity", "credits-summary", "credits-heatmap", "limited-number",
    "consecutive", "streak",
    # 模型列表 / 模型选择器
    "model", "sonus", "cantus", "tier",
]

_counter = {"n": 0}


def _parse_body(content):
    if not content:
        return None
    try:
        return json.loads(content.decode("utf-8", errors="replace"))
    except Exception:
        return content.decode("utf-8", errors="replace")[:8000]


def response(flow):
    url = flow.request.pretty_url
    host = flow.request.pretty_host
    if not any(d in host for d in TARGET_DOMAINS):
        return

    _counter["n"] += 1
    # 重点标记：命中关键词，或是非 GET（领取动作必是 POST/PUT）
    key = any(k in url.lower() for k in KEYWORDS) or flow.request.method not in ("GET", "OPTIONS", "HEAD")
    ts = time.strftime("%Y%m%d-%H%M%S")
    prefix = "KEY" if key else "req"
    path = CAPTURE_DIR / f"{prefix}-{ts}-{_counter['n']:03d}.json"

    record = {
        "time": ts,
        "key_request": key,
        "request": {
            "method": flow.request.method,
            "url": url,
            "host": host,
            "path": flow.request.path,
            "headers": dict(flow.request.headers),
            "body": _parse_body(flow.request.content),
        },
        "response": {
            "status": flow.response.status_code if flow.response else None,
            "headers": dict(flow.response.headers) if flow.response else None,
            "body": _parse_body(flow.response.content) if flow.response else None,
        },
    }
    path.write_text(json.dumps(record, ensure_ascii=False, indent=2), encoding="utf-8")

    marker = "KEY" if key else "   "
    status = flow.response.status_code if flow.response else "?"
    print(f"[{marker}] #{_counter['n']:03d} {flow.request.method} {url} -> {status}  saved={path.name}")

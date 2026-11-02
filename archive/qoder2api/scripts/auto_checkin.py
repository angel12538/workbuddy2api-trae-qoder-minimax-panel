#!/usr/bin/env python3
"""
Qoder 每日签到 CLI（国内版 / 国际版通用）

链路（两区路径一致，仅域名不同）：
  1. GET  /sash/api/v1/me/campaigns            → 找 CLAIMABLE 的 CLAIM_BENEFIT 活动
  2. POST /sash/api/v1/me/campaigns/{id}/claim → 领取

域名：
  cn     → openapi.qoder.com.cn
  global → openapi.qoder.sh

认证：Bearer device token + cosy-clienttype: 10，无需 COSY 签名、claim 无 body。

设备指纹（见 README「设备指纹政策」）：
  国际版账号在官方客户端**首次领取前**必须携带真实 cosy 指纹，否则服务端不下发
  活动（表现为 no_campaign）。指纹从账号 JSON 的可选字段 checkin_fingerprint 读取；
  未配置则只发送最小头集合（国内版通常无需指纹）。
  首次在官方客户端领取一次后服务端转为「懒下发」，之后无需指纹。
"""

import json
import ssl
import sys
import http.client
from pathlib import Path

REGION_HOSTS = {
    "cn": "openapi.qoder.com.cn",
    "global": "openapi.qoder.sh",
}

# 桌面端标识（抓包确认 cosy-clienttype=10）
APP_HEADERS = {
    "accept": "application/json",
    "accept-language": "zh-CN",
    "user-agent": "Qoder",
    "cosy-clienttype": "10",
}


def _ssl_ctx():
    ctx = ssl.create_default_context()
    for cafile in ["/etc/ssl/cert.pem", "/usr/local/etc/ca-certificates.crt"]:
        try:
            ctx.load_verify_locations(cafile)
            break
        except Exception:
            continue
    return ctx


def _request(host, method, path, token, extra_headers=None, timeout=60):
    """发送请求，返回 (status, parsed_body, raw_text)"""
    headers = dict(APP_HEADERS)
    headers["authorization"] = "Bearer " + token
    if extra_headers:
        headers.update(extra_headers)

    conn = http.client.HTTPSConnection(host, timeout=timeout, context=_ssl_ctx())
    conn.request(method, path, headers=headers)
    resp = conn.getresponse()
    raw = resp.read().decode("utf-8", errors="replace")
    conn.close()

    body = None
    try:
        body = json.loads(raw) if raw else None
    except Exception:
        body = raw
    return resp.status, body, raw


def fingerprint_headers(acc):
    """从账号配置读取签到指纹；未配置返回 None。"""
    fp = acc.get("checkin_fingerprint") or {}
    if not fp.get("machine_token"):
        return None
    return {
        "cosy-version": fp.get("version") or "0.4.4",
        "cosy-machineid": fp.get("machine_id", ""),
        "cosy-machinetoken": fp["machine_token"],
        "cosy-machinetype": fp.get("machine_type", ""),
        "cosy-machinecode": fp.get("machine_code", ""),
        "cosy-machinehostname": fp.get("hostname") or "localhost",
        "cosy-machineos": fp.get("machine_os") or "aarch64_darwin",
    }


def load_account(name=None):
    """从 qoder2api 数据目录加载账号（含 region 与可选指纹）。"""
    home = Path.home()
    for instance in [".qoder2api-cn", ".qoder2api-global", ".qoder2api"]:
        accounts_dir = home / instance / "accounts"
        secrets_dir = home / instance / "secrets"
        if not accounts_dir.exists():
            continue
        for acc_file in accounts_dir.glob("*.json"):
            try:
                acc = json.loads(acc_file.read_text())
            except Exception:
                continue
            if name and acc.get("name") != name:
                continue
            secret_file = secrets_dir / f"{acc['id']}.token"
            if not secret_file.exists():
                continue
            secret = secret_file.read_text().strip()
            token = secret
            if secret.startswith("{"):
                token = json.loads(secret).get("device_token", "")
            if not token:
                continue
            acc["_token"] = token
            acc.setdefault("region", "global")
            return acc
    return None


def checkin(acc):
    """执行签到，返回退出码。"""
    token = acc["_token"]
    region = acc.get("region", "global")
    host = REGION_HOSTS.get(region, REGION_HOSTS["global"])
    fph = fingerprint_headers(acc)

    print(f"账号: {acc.get('name')} ({acc.get('id')})")
    print(f"区域: {region}  Host: {host}")
    print(f"指纹: {'已配置' if fph else '未配置（国内通常不需要）'}")
    print()

    print("[1/2] 查询活动列表...")
    status, body, raw = _request(
        host, "GET", "/sash/api/v1/me/campaigns", token, extra_headers=fph
    )
    if status != 200 or not isinstance(body, dict):
        print(f"  ❌ HTTP {status}: {raw[:400]}")
        return 1

    campaigns = body.get("campaigns", [])
    print(f"  claimable={body.get('claimable')}  活动数={len(campaigns)}")

    target = None
    for c in campaigns:
        key = c.get("campaignKey", "?")
        atype = c.get("actionType", "?")
        cstatus = c.get("claimStatus", "?")
        benefit = c.get("benefit") or {}
        print(f"    - {key}  type={atype} status={cstatus} amount={benefit.get('amount', '-')}")
        if atype == "CLAIM_BENEFIT" and cstatus == "CLAIMABLE":
            target = c

    if not target:
        claimed = any(
            c.get("actionType") == "CLAIM_BENEFIT" and c.get("claimStatus") == "CLAIMED"
            for c in campaigns
        )
        if claimed:
            print("\n  ✅ 今日已领取")
        else:
            print("\n  ⚠️  无可用签到活动（no_campaign）")
            print("     国际版账号常见原因：本账号从未在官方客户端领取过，")
            print("     且未配置真实设备指纹。解决办法二选一：")
            print("       1) 先在官方客户端手动领取一次（之后服务端懒下发，无需指纹）")
            print("       2) 抓包取得真实指纹，写入账号 JSON 的 checkin_fingerprint")
            print("     参见 README「设备指纹政策」与 scripts/extract_fingerprint.sh")
        return 0

    cid = target["campaignId"]
    print(f"\n[2/2] 领取 {target.get('campaignKey')} ...")
    status, body, raw = _request(
        host,
        "POST",
        f"/sash/api/v1/me/campaigns/{cid}/claim",
        token,
        extra_headers={**(fph or {}), "origin": f"https://{host}"},
    )

    if status == 200 and isinstance(body, dict) and body.get("status") == "CLAIMED":
        if body.get("replayed"):
            print("  ✅ 今日已领取过（幂等返回）")
        else:
            amount = (body.get("benefit") or {}).get("amount", "-")
            print(f"  🎉 领取成功！{amount} Credits，有效期至 {body.get('expiresAt', '-')}")
        return 0

    print(f"  ❌ HTTP {status}")
    print(f"  {json.dumps(body, ensure_ascii=False, indent=2) if isinstance(body, dict) else raw[:400]}")
    return 1


def main():
    name = sys.argv[1] if len(sys.argv) > 1 else None
    if not name:
        print("用法: python3 auto_checkin.py <账号名>")
        print("提示: 账号名见 Web 控制台「账号列表」")
        return 2

    acc = load_account(name)
    if not acc:
        print(f"❌ 未找到账号 {name}（或其 token 缺失）")
        return 2
    return checkin(acc)


if __name__ == "__main__":
    sys.exit(main())

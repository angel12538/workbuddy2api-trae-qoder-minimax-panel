# -*- coding: utf-8 -*-
"""只读诊断：账号池健康、排队/风控信号、生效中的旋钮。

    python scripts/risk_diag.py            # 只读本地状态，不发任何上游请求
    python scripts/risk_diag.py --probe    # 额外做一次网关域名连通性探测（无鉴权）

设计原则：
  · 默认**不联网**——排查风控时最忌讳「为了诊断又打一轮上游」；
  · 不打印任何 token / uid 全量（只显示前 8 位）；
  · 结论直接给「现在处于什么状态、下一步该看哪」。
"""
import glob
import json
import os
import socket
import ssl
import sys
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)

ACCOUNTS = os.path.join(ROOT, "accounts")
SKIP = ("active_realm.json", "machine_identity.json", "settings.json",
        "models-cache-cn.json", "models-cache-intl.json")


def _fmt_ts(v):
    try:
        return time.strftime("%m-%d %H:%M", time.localtime(float(v)))
    except Exception:
        return "-"


def _left(v):
    try:
        return max(0, int(float(v) - time.time()))
    except Exception:
        return 0


def load_accounts():
    rows = []
    for path in sorted(glob.glob(os.path.join(ACCOUNTS, "*.json"))):
        name = os.path.basename(path)
        if name in SKIP:
            continue
        try:
            with open(path, encoding="utf-8") as fh:
                data = json.load(fh)
        except Exception:
            continue
        if not isinstance(data, dict) or not data.get("accessToken"):
            continue
        rows.append((name[:8], data))
    return rows


def container_detect():
    try:
        if os.path.exists("/.dockerenv"):
            return True
        with open("/proc/1/cgroup", encoding="utf-8", errors="ignore") as fh:
            blob = fh.read()
        return any(k in blob for k in ("docker", "containerd", "kubepods"))
    except Exception:
        return False


def probe(host, port=443, timeout=4.0):
    """仅做 TCP+TLS 握手，不发 HTTP、不带任何凭证。"""
    try:
        ctx = ssl.create_default_context()
        with socket.create_connection((host, port), timeout=timeout) as sock:
            with ctx.wrap_socket(sock, server_hostname=host):
                return "ok"
    except Exception as exc:
        return type(exc).__name__


def main(argv):
    do_probe = "--probe" in argv
    rows = load_accounts()
    print("=" * 68)
    print("Qoder 代理 · 风控/排队只读诊断")
    print("=" * 68)

    if not rows:
        print("未找到账号（accounts/*.json）。")
        return 0

    now = time.time()
    by_realm = {}
    print("\n[1] 账号状态")
    print("%-8s %-5s %-7s %-8s %-10s %s" % ("uid", "realm", "enabled", "计划", "冷却/熔断", "备注"))
    for short, d in rows:
        realm = d.get("realm") or "?"
        by_realm.setdefault(realm, []).append(d)
        cd = _left(d.get("cooldownUntil"))
        br = _left(d.get("breakerUntil"))
        dg = _left(d.get("degradeUntil"))
        flags = []
        if cd:
            flags.append("冷却%ds" % cd)
        if br:
            flags.append("熔断%ds" % br)
        if dg:
            flags.append("降权%ds" % dg)
        note = (d.get("lastError") or "")[:38]
        print("%-8s %-5s %-7s %-8s %-10s %s" % (
            short, realm, "yes" if d.get("enabled") else "no",
            (d.get("plan") or "-")[:8], ",".join(flags) or "-", note))

    print("\n[2] 额度快照（credits）")
    for short, d in rows:
        c = d.get("credits") or {}
        if not isinstance(c, dict):
            print("  %-8s 未查询" % short)
            continue
        shared = c.get("shared") or {}
        print("  %-8s 套餐 remain=%s/%s  共享=%s  更新=%s" % (
            short, c.get("remain"), c.get("size"),
            shared.get("remain") if shared else "-",
            (c.get("updated_iso") or "-")[5:16]))

    print("\n[3] 生效中的旋钮")
    keys = [
        ("QD_QUEUE_MAX_ROUNDS", "排队最多等几轮"),
        ("QD_QUEUE_WAIT_MAX_SECS", "单轮等待上限(s)"),
        ("QD_QUEUE_WAIT_BUDGET_SECS", "整次请求等待预算(s)"),
        ("QD_QUEUE_WAIT_JITTER", "等待抖动(s)"),
        ("QD_TRAFFIC_GUARD", "流量守卫 on/off（对所有账号生效）"),
        ("QD_TEAM_MAX_CONCURRENCY", "单号并发上限（默认 1）"),
        ("QD_TEAM_MIN_GAP_MS", "单号最小请求间隔(ms，默认 400)"),
        ("QD_STRUCTURED_TOOL_HISTORY", "结构化工具历史 on/off/auto"),
        ("QD_TOOL_RESULT_SCOPE", "工具结果削减范围 tail/all/turn"),
        ("QD_MACHINE_IDENTITY_CACHE", "设备身份缓存"),
        ("QD_KEEP_TOPLEVEL_IMAGES", "顶层重复图片(应关)"),
        ("QD_MAX_BODY_MB", "出站体积上限(MB)"),
        ("HTTPS_PROXY", "出站代理（只认环境变量）"),
        ("HTTP_PROXY", "出站代理"),
    ]
    for k, desc in keys:
        v = os.environ.get(k)
        print("  %-26s %-24s %s" % (k, v if v else "（未设置，用默认）", desc))

    print("\n[4] 环境判定")
    in_container = container_detect()
    intl = [d for _s, d in rows
            if (d.get("realm") == "intl") and d.get("enabled", True)]
    print("  容器环境: %s" % ("是" if in_container else "否"))
    print("  国际版启用账号: %d" % len(intl))
    if in_container and intl:
        print("  ⚠ 容器出口 + 国际版：容易被风控/排队（issue #57）。"
              "建议住宅出口，或改用国内版。")
    if not (os.environ.get("HTTPS_PROXY") or os.environ.get("HTTP_PROXY")):
        print("  · 未设置代理环境变量：网关将直连上游（数据中心 IP 更易被限）。")

    print("\n[5] 最近日志信号（若开启文件日志）")
    log_hits = 0
    for path in glob.glob(os.path.join(ROOT, "*.log")):
        try:
            with open(path, encoding="utf-8", errors="ignore") as fh:
                for line in fh.readlines()[-4000:]:
                    if any(k in line for k in ("10605", "isQueued", "RESOURCE_LIMIT",
                                               "429", "TOKEN_EXPIRE", "queued on")):
                        print("  %s: %s" % (os.path.basename(path), line.strip()[:120]))
                        log_hits += 1
                        if log_hits >= 12:
                            break
        except Exception:
            continue
        if log_hits >= 12:
            break
    if not log_hits:
        print("  未发现 10605/429/TOKEN_EXPIRE 记录（或未开启文件日志）。")

    if do_probe:
        print("\n[6] 域名连通性（TCP+TLS，无鉴权、不发请求）")
        hosts = ["gateway.qoder.com.cn", "openapi.qoder.com.cn",
                 "api1.qoder.sh", "api2.qoder.sh", "api3.qoder.sh",
                 "openapi.qoder.sh"]
        for h in hosts:
            print("  %-24s %s" % (h, probe(h)))

    print("\n提示：排队(10605)是**模型级**的——换账号不加容量；"
          "换模型/错峰/换住宅出口才有效。详见 docs/risk-control-and-queueing.md")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

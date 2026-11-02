"""账号行：今日签到状态 + 积分变化（面板）。"""
import io
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_accounts.py", "dashboard.html"])

print("[acct] 今日签到状态")
_acc = A.Account({"uid": "u-ck-1", "realm": "cn", "accessToken": "t"})
check("初始为未签到", _acc._checkin_stamped_today() is False)
_acc.last_checkin = time.strftime("%Y-%m-%d") + " 09:00:00"
check("今天签到过 -> True", _acc._checkin_stamped_today() is True)
_pub = _acc.public()
check("public() 暴露 checkedInToday", _pub.get("checkedInToday") is True, _pub.get("checkedInToday"))
check("public() 暴露 lastCheckinDate", _pub.get("lastCheckinDate") == time.strftime("%Y-%m-%d"),
      _pub.get("lastCheckinDate"))
check("public() 暴露 lastCreditsDelta 字段（可为 None）",
      "lastCreditsDelta" in _pub and "lastCreditsDeltaAt" in _pub)

print("[acct] 积分变化（+ / -）")
_q1 = {"userQuota": {"total": 1000, "used": 900, "remaining": 100},
       "addOnQuota": {"total": 0, "used": 0, "remaining": 0},
       "isQuotaExceeded": False}
_q2 = {"userQuota": {"total": 1000, "used": 800, "remaining": 200},
       "addOnQuota": {"total": 0, "used": 0, "remaining": 0},
       "isQuotaExceeded": False}
_q3 = {"userQuota": {"total": 1000, "used": 900, "remaining": 100},
       "addOnQuota": {"total": 0, "used": 0, "remaining": 0},
       "isQuotaExceeded": False}
_real = A.http_json
_payload = {"v": _q1}
A.http_json = lambda *a, **k: dict(_payload["v"])
try:
    _a2 = A.Account({"uid": "u-ck-2", "realm": "cn", "accessToken": "t"})
    _a2.fetch_credits()
    check("首次拉取不产生变化（没有旧值可比）",
          _a2.last_credits_delta is None, _a2.last_credits_delta)
    _payload["v"] = _q2
    _a2.fetch_credits()
    check("余额 100 -> 200 记 +100", _a2.last_credits_delta == 100,
          _a2.last_credits_delta)
    check("变化时间被记录", _a2.last_credits_delta_at > 0, _a2.last_credits_delta_at)
    _payload["v"] = _q3
    _a2.fetch_credits()
    check("余额 200 -> 100 记 -100", _a2.last_credits_delta == -100,
          _a2.last_credits_delta)
    _payload["v"] = _q3
    _a2.fetch_credits()
    check("余额不变时不覆盖上次变化值", _a2.last_credits_delta == -100,
          _a2.last_credits_delta)
finally:
    A.http_json = _real

_round = A.Account({"uid": "u-ck-3", "realm": "cn", "accessToken": "t",
                    "lastCreditsDelta": 100, "lastCreditsDeltaAt": 1700000000})
check("变化值可落盘并读回", _round.last_credits_delta == 100
      and _round.last_credits_delta_at == 1700000000.0,
      (_round.last_credits_delta, _round.last_credits_delta_at))
check("to_dict 带字段",
      _round.to_dict().get("lastCreditsDelta") == 100)

print("[acct] 面板渲染")
_dash = io.open(os.path.join(ROOT, "dashboard.html"), encoding="utf-8").read()
check("有 checkinBadge 渲染函数", "function checkinBadge(" in _dash)
check("有积分变化 chip 渲染函数", "function creditsDeltaChip(" in _dash)
check("账号名旁插入签到徽标", "checkinBadge(a)" in _dash)
check("积分列插入变化 chip", "creditsDeltaChip(a)" in _dash)
check("徽标样式（已签到/未签到）",
      ".ck-badge.done" in _dash and ".ck-badge.pending" in _dash)
check("chip 样式（涨/跌）与动画",
      ".delta-chip.up" in _dash and ".delta-chip.down" in _dash
      and "deltaPop" in _dash)
check("尊重 reduced-motion",
      "prefers-reduced-motion" in _dash)

finish()
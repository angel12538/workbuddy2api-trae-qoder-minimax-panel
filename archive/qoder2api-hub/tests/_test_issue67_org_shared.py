"""#67 组织共享池：cap=-1 无限池 + 按组织去重汇总。

    python tests/_test_issue67_org_shared.py
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_accounts.py", "qoder_proxy.py"])


class _Pool:
    def __init__(self, accounts):
        self.accounts = list(accounts)


def _mk(uid, plan, shared_remain, org, unlimited=False, name=None):
    acc = A.Account({"uid": uid, "realm": "cn", "accessToken": "t",
                     "organizationId": org})
    sh = {"remain": shared_remain, "used": 0, "size": 2500, "available": True}
    if unlimited:
        sh["unlimited"] = True
        sh["size"] = -1
    acc.credits = {"remain": plan,
                   "remain_effective": plan + (0 if unlimited else shared_remain),
                   "shared": sh, "used": 0, "size": plan}
    acc.nickname = name or uid
    return acc


# ---------------------------------------------------------------- 无限池
print("[67-a] cap=-1 识别为无限池")
_Q = {"userQuota": {"total": 2970, "used": 248, "remaining": 2722},
      "addOnQuota": {"total": 0, "used": 0, "remaining": 0},
      "orgResourcePackage": {"remaining": 1000000, "used": 0, "cap": -1,
                             "available": True},
      "isQuotaExceeded": False}
_real = A.http_json
A.http_json = lambda *a, **k: dict(_Q)
try:
    _acc = A.Account({"uid": "u67", "realm": "cn", "accessToken": "t",
                      "organizationId": "org-67"})
    _res = _acc.fetch_credits()
finally:
    A.http_json = _real
_c = (_res or {}).get("credits") or {}
check("#67 cap=-1 -> shared.unlimited=True",
      (_c.get("shared") or {}).get("unlimited") is True, _c.get("shared"))
check("#67 占位的 1,000,000 不计入 remain_effective（=套餐 2722）",
      _c.get("remain_effective") == 2722, _c.get("remain_effective"))
check("#67 shared_unlimited() 可用", A.shared_unlimited(_c) is True)

_acc.reserve_credits = 999999
check("#67 无限池时余额地板护栏显式不拦（不靠大数字碰巧不拦）",
      _acc.reserve_blocked() is False)
_acc.balance_until = 9e9
check("#67 无限池时 402 停放可解除",
      _acc.revive_balance_cooldown() is True and _acc.balance_until == 0.0)

# ---------------------------------------------------------------- 有界池仍照旧
_Q2 = dict(_Q)
_Q2["orgResourcePackage"] = {"remaining": 952, "used": 1548, "cap": 2500,
                             "available": True}
A.http_json = lambda *a, **k: dict(_Q2)
try:
    _acc2 = A.Account({"uid": "u67b", "realm": "cn", "accessToken": "t"})
    _res2 = _acc2.fetch_credits()
finally:
    A.http_json = _real
_c2 = (_res2 or {}).get("credits") or {}
check("#67 有界共享池仍计入有效余额（952+2722）",
      _c2.get("remain_effective") == 2722 + 952, _c2.get("remain_effective"))
check("#67 有界池不标 unlimited", not (_c2.get("shared") or {}).get("unlimited"))

# ---------------------------------------------------------------- 汇总去重
print("[67-b] /credits/summary 按组织去重")
_old_pool = P.POOL
try:
    P.POOL = _Pool([_mk("a1", 10, 1000, "org-1"),
                    _mk("a2", 20, 1000, "org-1"),
                    _mk("a3", 30, 1000, "org-1")])
    _s = P.credits_summary()
finally:
    P.POOL = _old_pool
check("#67 同组织 3 账号：共享池只算一次（60+1000=1060）",
      _s["totals"]["remain"] == 1060, _s["totals"])
check("#67 首行计 1010、其余只计套餐（10+1000 / 20 / 30）",
      [r["credits_counted"] for r in _s["accounts"]] == [1010, 20, 30],
      [r["credits_counted"] for r in _s["accounts"]])
check("#67 行内保留账号视角 effective（兼容旧字段）",
      _s["accounts"][0]["credits_remain_effective"] == 1010)

try:
    P.POOL = _Pool([_mk("b1", 10, 1000, "org-A"),
                    _mk("b2", 20, 1000, "org-B")])
    _s2 = P.credits_summary()
finally:
    P.POOL = _old_pool
check("#67 不同组织各自计一次（30+1000+1000=2030）",
      _s2["totals"]["remain"] == 2030, _s2["totals"])

try:
    P.POOL = _Pool([_mk("c1", 10, 1000000, "org-U", unlimited=True),
                    _mk("c2", 20, 1000000, "org-U", unlimited=True)])
    _s3 = P.credits_summary()
finally:
    P.POOL = _old_pool
check("#67 无限池不计入数字，只记 unlimited_orgs=1",
      _s3["totals"]["remain"] == 30
      and _s3["totals"]["unlimited_orgs"] == 1, _s3["totals"])

try:
    P.POOL = _Pool([_mk("d1", 10, 500, ""), _mk("d2", 20, 500, "")])
    _s4 = P.credits_summary()
finally:
    P.POOL = _old_pool
check("#67 无组织标识（个人号）仍按账号累加（30+500+500=1030）",
      _s4["totals"]["remain"] == 1030, _s4["totals"])

finish()

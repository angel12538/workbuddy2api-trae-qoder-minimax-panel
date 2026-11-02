"""官方积分消耗（0.4.3 客户端新接口）：解析 / 缓存 / 路由 / 面板。"""
import io
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_accounts.py", "qoder_proxy.py", "dashboard.html"])

_src = io.open(os.path.join(ROOT, "qoder_accounts.py"), encoding="utf-8").read()
_psrc = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
_dash = io.open(os.path.join(ROOT, "dashboard.html"), encoding="utf-8").read()

print("[credits] 端点常量（逆向 0.4.3 客户端）")
check("credits-summary 常量存在",
      A.PATH_CREDIT_SUMMARY == "/sash/api/v1/ai-conversations/credits-summary",
      A.PATH_CREDIT_SUMMARY)
check("credits-heatmap 常量存在",
      A.PATH_CREDIT_HEATMAP == "/sash/api/v1/ai-conversations/credits-heatmap",
      A.PATH_CREDIT_HEATMAP)
check("achievements 常量存在",
      A.PATH_ACHIEVEMENTS == "/sash/api/v1/me/achievements", A.PATH_ACHIEVEMENTS)
check("消耗接口有 5 分钟缓存 TTL",
      getattr(A, "CREDIT_USAGE_TTL", 0) == 300)

print("[credits] intl 活动平台第二 host")
_c = A.openapi_candidates("intl")
check("intl 候选含 center.qoder.sh", "https://center.qoder.sh" in _c, _c)
check("center 排在官网域名之前",
      _c.index("https://center.qoder.sh") < _c.index("https://qoder.com"), _c)
check("cn 候选不含 center.qoder.sh（实测 401）",
      "https://center.qoder.sh" not in A.openapi_candidates("cn"))

print("[credits] 解析与缓存")
_acc = A.Account({"uid": "u-cu", "realm": "cn", "accessToken": "t"})
_calls = {"n": 0}


def _fake_openapi(path, **kw):
    _calls["n"] += 1
    if path == A.PATH_CREDIT_SUMMARY:
        return ({"totalCredits": 1022.89, "peakCredits": 1015.1,
                 "peakDate": "2026-10-14", "unit": "credits"}, 200, "")
    if path == A.PATH_CREDIT_HEATMAP:
        return ({"items": [{"date": "2026-10-13", "value": 12.5},
                           {"date": "2026-10-14", "value": 1015.1}]}, 200, "")
    if path == A.PATH_ACHIEVEMENTS:
        return ({"achievements": [{"achievementKey": "goal_first_use"},
                                  {"achievementKey": "x"}]}, 200, "")
    return (None, 404, "nope")


_real = _acc._openapi_json
_acc._openapi_json = _fake_openapi
cu = _acc.fetch_credit_usage()
check("ok=True", cu.get("ok") is True)
check("totalCredits 解析", abs(cu["total"] - 1022.89) < 0.01, cu["total"])
check("peak / peak_date 解析",
      abs(cu["peak"] - 1015.1) < 0.01 and cu["peak_date"] == "2026-10-14")
check("逐日消耗解析", len(cu["daily"]) == 2 and cu["daily"][-1]["value"] == 1015.1,
      cu["daily"])
_n1 = _calls["n"]
_acc.fetch_credit_usage()
check("第二次命中缓存（不再发请求）", _calls["n"] == _n1, _calls["n"])

ach = _acc.fetch_achievements()
check("成就 key 解析", ach["keys"] == ["goal_first_use", "x"], ach["keys"])

# 失败路径：summary 不可用 -> ok False，且不抛异常
_acc2 = A.Account({"uid": "u-cu2", "realm": "cn", "accessToken": "t"})
_acc2._openapi_json = lambda path, **kw: (None, 500, "boom")
_bad = _acc2.fetch_credit_usage()
check("失败时 ok=False 且带错误", _bad["ok"] is False and "boom" in _bad["error"], _bad)
_acc._openapi_json = _real

print("[credits] 路由与面板")
check("服务端有 /accounts/credits/usage",
      'if path == "/accounts/credits/usage":' in _psrc)
check("路由汇总 total/peak/daily 并支持 uid/realm 过滤",
      'want_uid' in _psrc and '"daily": daily' in _psrc)
check("面板有「官方积分消耗」区块",
      "creditUsageWrap" in _dash and "官方积分消耗" in _dash)
check("面板渲染逐日热力图 + 账号明细表",
      "heat-grid" in _dash and "峰值日期 / 错误" in _dash)
check("面板有 60 秒节流（服务端另有 5 分钟缓存）",
      "CREDIT_USAGE_AT" in _dash and "60000" in _dash)
check("看板加载时挂钩（失败不影响主看板）",
      "loadCreditUsage(!!force)" in _dash)
check("渲染有签名去重（避免 issue #98 的跳动）",
      "CREDIT_USAGE_SIG" in _dash)

finish()
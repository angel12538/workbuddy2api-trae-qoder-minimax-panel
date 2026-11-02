"""统计概览（新版数据看板）：后端聚合 + 页面结构 + 交互。"""
import io
import json
import os
import sys
import tempfile
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py", "dashboard.html"])

print("[stats] 后端聚合")
_tmp = tempfile.mkdtemp(prefix="stats-")
_log = os.path.join(_tmp, "usage.jsonl")
_now = time.time()
# 时间无关夹具（#70 同款教训）：把「今天的行」钉在本地零点之后，
# 而不是 now-5h —— 否则在本地零点附近跑，部分行会掉进昨天，
# 热力图峰值/活跃天数随跑测时刻漂移（CI 03:31 UTC 就红过）。
_lt = time.localtime(_now)
_midnight = time.mktime((_lt.tm_year, _lt.tm_mon, _lt.tm_mday, 0, 0, 0, 0, 0, -1))
_today_at = lambda i: _midnight + 60 * (i + 1)      # 今天 00:01, 00:02 ...
_yday_at = lambda i: _midnight - 3600 - 60 * i      # 昨天 23:00 起
_rows = []
# 3 天、3 个模型（两家厂商）、含 1 条错误行
for i in range(5):
    _rows.append({"at": _today_at(i), "iso": "", "model": "claude-4-sonnet",
                  "prompt_tokens": 1000, "completion_tokens": 100,
                  "total_tokens": 1100, "cached_tokens": 600,
                  "account": "acct-1", "realm": "cn"})
for i in range(3):
    _rows.append({"at": _yday_at(i), "iso": "", "model": "gpt-5",
                  "prompt_tokens": 500, "completion_tokens": 50,
                  "total_tokens": 550, "cached_tokens": 0,
                  "account": "acct-2", "realm": "cn"})
_rows.append({"at": _now - 2 * 86400, "iso": "", "model": "qwen3-max",
              "prompt_tokens": 10, "completion_tokens": 1, "total_tokens": 11,
              "cached_tokens": 0, "account": "acct-1", "realm": "cn"})
_rows.append({"at": _now - 60, "iso": "", "model": "claude-4-sonnet",
              "error": "upstream 500", "account": "acct-1", "realm": "cn"})
with io.open(_log, "w", encoding="utf-8") as fh:
    for r in _rows:
        fh.write(json.dumps(r) + "\n")

_orig = P.USAGE_LOG
P.USAGE_LOG = _log
try:
    _d = P.compute_usage_stats(range="all")
finally:
    P.USAGE_LOG = _orig

check("KPI 成功数=9 / 失败=1",
      _d["kpi"]["requests"] == 9 and _d["kpi"]["errors"] == 1,
      (_d["kpi"]["requests"], _d["kpi"]["errors"]))
check("总 Token 汇总正确", _d["kpi"]["tokens"] == 5 * 1100 + 3 * 550 + 11,
      _d["kpi"]["tokens"])
check("活跃天数=3", _d["kpi"]["active_days"] == 3, _d["kpi"]["active_days"])
check("Top 模型按 token 排序",
      _d["kpi"]["top_model"]["key"] == "claude-4-sonnet", _d["kpi"]["top_model"])
check("Top 账号聚合", len(_d["top_accounts"]) == 2
      and _d["top_accounts"][0]["key"] == "acct-1", _d["top_accounts"][:1])
check("厂商归并 claude -> Anthropic / gpt -> OpenAI",
      {r["key"] for r in _d["providers"]} == {"Anthropic", "OpenAI", "通义千问"},
      [r["key"] for r in _d["providers"]])
check("缓存窗口 7 天命中率有值",
      _d["cache"]["windows"][3]["pct"] is not None)
check("24h 曲线 24 个点", len(_d["hourly"]["points"]) == 24)
check("热力图覆盖 365 天", len(_d["heatmap"]["days"]) == 365,
      len(_d["heatmap"]["days"]))
check("热力图峰值 = 单日最大 token", _d["heatmap"]["max"] >= 5500,
      _d["heatmap"]["max"])

print("[stats] 窗口语义")
_t = tempfile.mkdtemp(prefix="stats2-")
_log2 = os.path.join(_t, "usage.jsonl")
with io.open(_log2, "w", encoding="utf-8") as fh:
    for r in _rows:
        fh.write(json.dumps(r) + "\n")
P.USAGE_LOG = _log2
try:
    _today = P.compute_usage_stats(range="today")
    _all = P.compute_usage_stats(range="all")
finally:
    P.USAGE_LOG = _orig
check("today 只含今天的行", _today["kpi"]["requests"] < _all["kpi"]["requests"],
      (_today["kpi"]["requests"], _all["kpi"]["requests"]))
check("custom 区间按 since/until 过滤",
      P.compute_usage_stats(range="custom", since=_now - 7200,
                            until=_now)["kpi"]["requests"] <= 3)

print("[stats] 面板结构")
_dash = io.open(os.path.join(ROOT, "dashboard.html"), encoding="utf-8").read()
check("有统计概览标题与自定义区间标签", "统计概览" in _dash and "statRangeLabel" in _dash)
check("区间胶囊含 6 个档位",
      all(("data-r=\"" + r + "\"") in _dash
          for r in ("today", "7d", "30d", "month", "all", "custom")))
check("两个日期输入（自定义区间）",
      'id="statFrom"' in _dash and 'id="statTo"' in _dash)
check("6 个 KPI 卡片容器", 'id="statKpis"' in _dash)
check("四张排行面板",
      all(("id=\"" + i + "\"") in _dash for i in (
          "topAccounts", "topProviders", "modelUsage", "providerUsage")))
check("热力图 / 缓存卡 / 24h 曲线 / 按天趋势 容器齐全",
      all(("id=\"" + i + "\"") in _dash for i in (
          "heatWrap", "heatLegend", "cacheCards", "hourlyChart", "dailyChart")))
check("面板标题用 h2（左侧导航靠它建锚点）",
      _dash.count("<section class=\"stat-panel\">") >= 6)
check("有悬浮提示与 reduced-motion 支持",
      "stat-tip" in _dash and "prefers-reduced-motion" in _dash)
check("区间切换有滑动指示器",
      "statSegPill" in _dash and "seg-pill" in _dash)
check("数字滚动只在首屏/换区间（轮询不重播）",
      "let statAnimateOnce = true;" in _dash
      and "statAnimateOnce = !!force || !statData;" in _dash
      and "statAnimateOnce = false;" in _dash)
check("统计接口已挂路由", "/usage/stats" in io.open(
      os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read())

print("[stats] 目录/关键词")
check("pageNavSections 会跳过 .hint-icon（侧栏标签不带 ?）",
      "clone.querySelectorAll('em, .hint-icon')" in _dash)
check("boot 对每步容错（否则页签初始化会被跳过）",
      "boot step failed:" in _dash)

print("[stats] getElementById 字面量 id 必须真实存在")
import re as _re
# 回归护栏：v1.7.1 删掉性能矩阵容器后，refreshInner() 里仍留着
# document.getElementById('perfMatrix').innerHTML = ... ，每 5 秒抛一次
# TypeError。导入账号后接着调用的 refresh() 也抛，于是用户看到
# 「导入失败: Cannot set properties of null」——其实账号已经导入成功了。
_id_defs = set(_re.findall(r'id="([\w\-]+)"', _dash))
_id_defs |= set(_re.findall(r"\.id\s*=\s*['\"]([\w\-]+)['\"]", _dash))
_id_defs |= set(_re.findall(r"id:\s*['\"]([\w\-]+)['\"]", _dash))
_refs = set(_re.findall(r"getElementById\(\s*['\"]([\w\-]+)['\"]\s*\)", _dash))
# 只有「取到就直接赋属性」才会抛；写成 if(x) 判断或 || 兜底的允许悬空
_unguarded = set(_re.findall(
    r"getElementById\(\s*['\"]([\w\-]+)['\"]\s*\)\s*\.\s*"
    r"(?:innerHTML|textContent|className|style|value|checked|disabled|classList)",
    _dash))
_dangling = sorted(_unguarded - _id_defs)
check("没有「指向不存在 id 又直接赋属性」的 getElementById（引用 %d 个 id）" % len(_refs),
      not _dangling, "悬空: " + ", ".join(_dangling))

print("[stats] 顶栏吸顶只作用于 body>header")
check("顶栏规则已收窄为 body>header（面板内的 <header> 不再吸顶）",
      "body>header{" in _dash and _re.search(r'(^|\n)header\{', _dash) is None)
check("面板标题显式清掉吸顶/层级",
      ".stat-panel>header,.card>header,section>header{position:static;z-index:auto" in _dash)
check("账号页工具栏吸顶停在顶栏下方（不再钻到导航底下）",
      "top:calc(var(--page-nav-top, 84px) - 10px)" in _dash)

finish()
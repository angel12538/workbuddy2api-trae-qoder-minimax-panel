"""跟进 wb 的三项能力：费用估算 / 积分获取与流水 / 剩余用量估算。"""
import io
import json
import os
import sys
import tempfile
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_pricing.py", "qoder_activity.py",
                         "qoder_proxy.py", "dashboard.html"])
import qoder_pricing as PR         # noqa: E402
import qoder_activity as AC        # noqa: E402

# ---------------------------------------------------------------- 费用估算
print("[pricing] 参考价折算")
_c = PR.cost_of("dfmodel", prompt_tokens=1_000_000, cached_tokens=0,
                completion_tokens=1_000_000)
check("未命中输入 + 输出按单价折算", abs(_c - (0.28 + 0.42)) < 1e-9, _c)
_c2 = PR.cost_of("dfmodel", 1_000_000, 1_000_000, 0)
check("缓存命中按更低单价", abs(_c2 - 0.028) < 1e-9, _c2)
check("缓存命中数不会超过输入（防脏数据多算）",
      abs(PR.cost_of("dfmodel", 100, 999, 0) - 0.028 * 100 / 1e6) < 1e-12)
check("未知模型不计价（返回 None，不猜数字）",
      PR.cost_of("totally-unknown") is None)
check("别名可命中（deepseek-v4-flash -> dfmodel）",
      PR.price_for("DeepSeek-V4-Flash") == PR.DEFAULT_PRICES["dfmodel"])
check("带版本后缀可继承（deepseek-v4-flash-0731）",
      PR.price_for("deepseek-v4-flash-0731") == PR.DEFAULT_PRICES["dfmodel"])
_tmp = tempfile.mkdtemp(prefix="price-")
json.dump({"dfmodel": {"in": 1, "cached": 0.5, "out": 2}},
          io.open(os.path.join(_tmp, "pricing-overrides.json"), "w",
                  encoding="utf-8"))
_ov = PR.load_overrides(_tmp)
check("覆盖表生效", PR.cost_of("dfmodel", 1_000_000, 0, 0, _ov) == 1.0)
_tbl = PR.table(_tmp)
check("覆盖表在 table() 里标 source=override",
      _tbl["dfmodel"]["source"] == "override", _tbl.get("dfmodel"))

# ---------------------------------------------------------------- 动作流水
print("[activity] 签到 / 领取流水")
_adir = tempfile.mkdtemp(prefix="act-")
AC.record(_adir, "checkin", True, uid="uid123456", nickname="A", amount=100,
          detail="ok")
AC.record(_adir, "checkin", False, uid="uid123456", nickname="A",
          detail="no quota")
rows = AC.recent(_adir)
check("记录可按时间倒序读回", len(rows) == 2 and rows[0]["ok"] is False,
      [r["ok"] for r in rows])
check("成功记录的积分可统计",
      sum(r["amount"] for r in rows if r["ok"]) == 100)
check("空白目录返回空列表", AC.recent(tempfile.mkdtemp()) == [])

# 过期过滤
_old = os.path.join(_adir, "activity.jsonl")
with io.open(_old, "a", encoding="utf-8") as fh:
    fh.write(json.dumps({"ts": time.time() - 40 * 86400, "kind": "checkin",
                         "ok": True, "amount": 1}) + "\n")
check("超过 30 天的记录不再返回", len(AC.recent(_adir)) == 2,
      len(AC.recent(_adir)))

# ---------------------------------------------------------------- 端点与面板
_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
for _p in ("/pricing", "/accounts/credits/grants", "/usage/remaining",
           "/accounts/activity"):
    check("端点存在: %s" % _p, 'if path == "%s":' % _p in _src)
check("签到路由会写流水", "qoder_activity.record(" in _src)
check("快照里带 cost_usd / unpriced",
      '"cost_usd"' in _src and '"unpriced"' in _src)

_dash = io.open(os.path.join(ROOT, "dashboard.html"), encoding="utf-8").read()
for _fn in ("loadInsights", "renderGrants", "renderRemaining", "renderActivity"):
    check("面板有 %s" % _fn, _fn in _dash)
check("洞察区块已挂载", 'id="insightCards"' in _dash)

finish()
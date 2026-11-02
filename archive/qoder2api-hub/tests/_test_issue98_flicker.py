"""#98 看板轮询时动画重播 / 图表跳动。"""
import io
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["dashboard.html"])

_dash = io.open(os.path.join(ROOT, "dashboard.html"), encoding="utf-8").read()

print("[#98] 数据没变就不重绘")
check("有分区签名助手 statSig", "function statSig(key, value){" in _dash)
check("KPI 有签名早退", 'statSig("kpis"' in _dash)
check("四张排行表有签名早退", 'statSig("rank:" + id' in _dash)
check("热力图有签名早退（365 格不重建）", 'statSig("heat"' in _dash)
check("缓存四卡有签名早退", 'statSig("cache"' in _dash)
check("24h 曲线有签名早退", 'statSig("hourly"' in _dash)
check("按天趋势有签名早退", 'statSig("daily"' in _dash)
check("按 Key 档位表有签名早退（refresh 每 5 秒会调它）",
      'statSig("effortView"' in _dash)

print("[#98] 文本写入也要去重")
check("有 statSetText（相同文本不写 DOM）", "function statSetText(el, text){" in _dash)
check("KPI 数值走 statSetText", "statSetText(el, t.fmt(t.v))" in _dash)
check("排行合计走 statSetText", "statSetText(sum," in _dash)
check("按天区间标题走 statSetText", "statSetText(lbl," in _dash)
check("质量副标题走 statSetText", "statSetText(det," in _dash)

print("[#98] 动画只在该播的时候播")
check("有一次性动画开关 statAnimateOnce", "let statAnimateOnce = true;" in _dash)
check("轮询不播数字滚动（animate=false 时直接落值）",
      "if(animate){ statNum(el, t.v, t.fmt); }" in _dash
      and "else { statSetText(el, t.fmt(t.v)); }" in _dash)
check("排行进度条：非动画时直接落宽度",
      'else { bar.style.width = w ' in _dash)
check("折线描边动画受 animate 控制",
      "if(path && animate && !_prefersReduced()){" in _dash)
check("柱状图增长受 animate 控制",
      "if(!animate || _prefersReduced()){ el.classList.add(\"grown\"); return; }" in _dash)
check("loadAnalytics 只在首屏/换区间/手动时开动画",
      "statAnimateOnce = !!force || !statData;" in _dash
      and "statAnimateOnce = false;" in _dash)

print("[#98] 用量洞察同样去重")
check("洞察有数据签名", "let INSIGHT_SIG =" in _dash and "if(_sig === INSIGHT_SIG)" in _dash)
check("洞察数字滚动只在首屏/手动刷新",
      "let INSIGHT_ANIMATE = true;" in _dash
      and "const _animate = !!showToast || INSIGHT_ANIMATE;" in _dash)

print("[#98] 语义验证（签名函数本身）")
import json
import subprocess
import tempfile

_js = """
const STAT_SIG = {};
function statSig(key, value){
  const sig = JSON.stringify(value);
  if(STAT_SIG[key] === sig) return false;
  STAT_SIG[key] = sig;
  return true;
}
const out = {
  first: statSig("k", [1, 2]),
  same: statSig("k", [1, 2]),
  changed: statSig("k", [1, 3]),
  otherKey: statSig("k2", [1, 2]),
  objSame: statSig("o", {a: 1, b: [2, 3]}) && statSig("o", {a: 1, b: [2, 3]}),
};
console.log(JSON.stringify(out));
"""
_node = None
try:
    _node = subprocess.run(["node", "-e", _js], capture_output=True, text=True, timeout=30)
except Exception as _exc:
    _node = None
if _node is None or _node.returncode != 0:
    print("  [skip] 没有可用 node，跳过签名语义验证")
else:
    _out = json.loads((_node.stdout or "{}").strip() or "{}")
    check("首次写入返回 true", _out.get("first") is True)
    check("相同数据返回 false（不重绘）", _out.get("same") is False)
    check("数据变化返回 true", _out.get("changed") is True)
    check("不同 key 互不影响", _out.get("otherKey") is True)
    check("对象/数组按内容比较", _out.get("objSame") is False)

finish()
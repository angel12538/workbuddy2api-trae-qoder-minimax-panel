"""#70 排程：可配置时刻 / cron / 抖动 + 不漏命中。

    python tests/_test_issue70_schedule.py
"""
import os
import sys
import tempfile
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_scheduler.py"])
import qoder_scheduler as S         # noqa: E402


class _Pool:
    dir = tempfile.mkdtemp(prefix="sched70-")
    accounts = []


_s = S.Scheduler(_Pool())

# ---------------------------------------------------------------- 解析
check("#70 默认签到时刻 09:00 / 21:00",
      S.Scheduler._fmt_times(_s.checkin_times) == "09:00,21:00",
      _s.checkin_times)
check("#70 默认保活 22:00",
      S.Scheduler._fmt_times(_s.keepalive_times) == "22:00",
      _s.keepalive_times)
check("#70 自定义时刻解析（含非法项丢弃）",
      S.Scheduler._parse_times("07:30, 25:00, 23:15, x", [(1, 0)])
      == [(7, 30), (23, 15)])
check("#70 全非法时回退默认",
      S.Scheduler._parse_times("nope", [(9, 0)]) == [(9, 0)])

# ---------------------------------------------------------------- cron
_s.cron = "0 9,21 * * *"
check("#70 cron 字段可解析", _s._cron_fields() is not None)
_tm9 = time.localtime(time.mktime((2026, 10, 20, 9, 0, 0, 0, 0, -1)))
_tm10 = time.localtime(time.mktime((2026, 10, 20, 10, 0, 0, 0, 0, -1)))
check("#70 cron 命中 09:00", _s._cron_matches(_tm9) is True)
check("#70 cron 不命中 10:00", _s._cron_matches(_tm10) is False)
_s.cron = "*/15 * * * *"
_tm = time.localtime(time.mktime((2026, 10, 20, 10, 30, 0, 0, 0, -1)))
check("#70 cron 支持 */n 步进", _s._cron_matches(_tm) is True)
_s.cron = "bad cron"
check("#70 非法 cron 被识别为 None", _s._cron_fields() is None)

# ---------------------------------------------------------------- 不漏命中
_s.cron = ""
_s.checkin_times = [(9, 0)]
_s.keepalive_times = [(22, 0)]
_s._fired = {}
_occ = _s._occurrences(0)
check("#70 当天排程点包含签到与保活", len(_occ) == 2, [o[1] for o in _occ])
_now = time.time()
# issue #87：原断言只看「今天已过去的点」，CI 在 00:0x UTC 跑时今天的点全是
# 未来 -> _fired 为空 -> 必然红（时间依赖的 flake）。这里把所有今天点都标记
# 为已触发，再断言「不会重复返回同一个时间戳」（允许跨天滚动）。
_occ_ts = {ts for _k, _l, ts in _occ}
_s._fired = {ts: True for ts in _occ_ts}
_nxt70 = _s._next_target()
check("#70 已触发过的点不会被重复触发",
      _nxt70 is None or _nxt70[2] not in _occ_ts, _nxt70)
_s._fired = {}
_due = _s._next_target()
check("#70 过点未触发会立即补一次（回补窗口 3h）",
      _due is None or (_due[2] <= _now + 1), _due)

# ---------------------------------------------------------------- 配置生效
_st = _s.set_schedule(checkin_times="07:15,19:45", keepalive_times="23:30",
                      cron="", jitter_secs=12)
check("#70 set_schedule 立即生效（签到）", _st["checkin_times"] == "07:15,19:45",
      _st.get("checkin_times"))
check("#70 set_schedule 立即生效（保活）", _st["keepalive_times"] == "23:30")
check("#70 抖动可配置", _st["jitter_secs"] == 12, _st.get("jitter_secs"))
check("#70 状态里带时区（容器 UTC 一眼可见）", bool(_st.get("timezone")),
      _st.get("timezone"))
_st2 = _s.set_schedule(cron="0 8 * * *")
check("#70 设置 cron 后状态可见", _st2["cron"] == "0 8 * * *", _st2.get("cron"))
_st3 = _s.set_schedule(cron="垃圾")
check("#70 非法 cron 被忽略（回到时刻表）", _st3["cron"] == "", _st3.get("cron"))

# ---------------------------------------------------------------- 端点与面板
_src = open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("#70 POST /scheduler/config 存在", 'if path == "/scheduler/config":' in _src)
_dash = open(os.path.join(ROOT, "dashboard.html"), encoding="utf-8").read()
check("#70 面板有排程设置入口", "toggleSchedEditor" in _dash
      and "schCron" in _dash)
# issue #70 后续：文本输入改成 24 小时刻度圆环，端点也从 /accounts 路由兜底里修出来
check("#70 圆环渲染函数存在（点刻度选整点）",
      all(k in _dash for k in ("renderSchedDial", "toggleSchedHour",
                               "switchSchedMode", "_dialPath")))
check("#70 圆环有 24 个刻度（循环 0..23）", "for(let h = 0; h < 24; h++)" in _dash)
check("#70 保存时从圆环状态取时刻（不再手写文本）",
      "_schedFmt(SCHED.checkin)" in _dash
      and "_schedFmt(SCHED.keepalive)" in _dash)
check("#70 不再依赖已删除的文本输入框", "schCheckin" not in _dash)
_seg70 = _src.split('def _handle_accounts(self, path, payload):')[1][:6000]
check("#70 /scheduler/config 在账号路由里处理（否则 404 unknown account endpoint）",
      'if path == "/scheduler/config":' in _seg70)
check("#70 旧的无路由副本已删除（全文件只出现一次 set_schedule 调用点）",
      _src.count("SCHEDULER.set_schedule") == 1,
      _src.count("SCHEDULER.set_schedule"))
check("#70 状态行显示时区与下次执行",
      "时区 " in _dash and "下次 " in _dash)

finish()
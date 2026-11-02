"""#102 禁用与签到解耦：暂停推理的账号仍可签到（独立开关，默认关）。"""
import io
import json
import os
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_accounts.py", "qoder_proxy.py",
                         "qoder_tasks.py", "qoder_scheduler.py", "dashboard.html"])

print("[#102] 两个谓词解耦")
_on = A.Account({"uid": "u1", "realm": "cn", "accessToken": "t", "enabled": True})
_off = A.Account({"uid": "u2", "realm": "cn", "accessToken": "t", "enabled": False})
_off_ck = A.Account({"uid": "u3", "realm": "cn", "accessToken": "t",
                     "enabled": False, "checkinWhenDisabled": True})
_no_tok = A.Account({"uid": "u4", "realm": "cn", "accessToken": "", "enabled": True})
_no_tok_ck = A.Account({"uid": "u5", "realm": "cn", "accessToken": "",
                        "enabled": False, "checkinWhenDisabled": True})

check("默认关（维持历史行为）", _off.checkin_when_disabled is False)
check("启用账号：两个口径都为真",
      _on.inference_eligible() is True and _on.checkin_eligible() is True)
check("禁用 + 开关关：都不参与",
      _off.inference_eligible() is False and _off.checkin_eligible() is False)
check("禁用 + 开关开：不接推理，但参与签到",
      _off_ck.inference_eligible() is False and _off_ck.checkin_eligible() is True)
check("缺凭证：即使开了开关也不参与签到（签了必然失败）",
      _no_tok.checkin_eligible() is False
      and _no_tok_ck.checkin_eligible() is False)

print("[#102] 持久化与面板字段")
_round = A.Account(_off_ck.to_dict())
check("to_dict 写入 checkinWhenDisabled",
      _off_ck.to_dict().get("checkinWhenDisabled") is True)
check("重新加载后仍生效", _round.checkin_eligible() is True)
check("public() 暴露给面板",
      _off_ck.public().get("checkinWhenDisabled") is True)

print("[#102] 三条签到路径都改了口径")
_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
_tasks = io.open(os.path.join(ROOT, "qoder_tasks.py"), encoding="utf-8").read()
_sched = io.open(os.path.join(ROOT, "qoder_scheduler.py"), encoding="utf-8").read()
_dash = io.open(os.path.join(ROOT, "dashboard.html"), encoding="utf-8").read()
check("面板每日签到用 checkin_eligible",
      "if a is not None and a.checkin_eligible()" in _src)
check("批量签到/领福利 targets 用 checkin_eligible",
      _src.count("[a for a in POOL.accounts if a.checkin_eligible()]") >= 2,
      _src.count("[a for a in POOL.accounts if a.checkin_eligible()]"))
check("任务中心可操作账号用 checkin_eligible",
      "if a.checkin_eligible()" in _tasks)
check("调度器 targets 用 checkin_eligible",
      "if a.checkin_eligible() and a.uid not in stale_uids" in _sched)
check("调度器仍排除失效凭证（stale_uids 不变）", "stale_uids" in _sched)

print("[#102] 切换接口与面板控件")
check("有 /accounts/checkin-when-disabled 路由",
      'if path == "/accounts/checkin-when-disabled":' in _src)
check("路由会落盘", "target.save(os.path.dirname(target.path))" in _src)
check("面板有「仍签到」勾选",
      "ck-when-off" in _dash and "setCheckinWhenDisabled" in _dash)
check("只在停用行显示", "(!a.enabled)" in _dash and "仍签到" in _dash)

print("[#102] 端到端：切换后保存并可读回")
_tmp = tempfile.mkdtemp(prefix="ck102-")
_acc = A.Account({"uid": "u-e2e", "realm": "cn", "accessToken": "t",
                  "enabled": False})
_acc.path = os.path.join(_tmp, "u-e2e.json")
_acc.save(_tmp)
_acc.checkin_when_disabled = True
_acc.save(_tmp)
_back = A.Account(json.load(io.open(_acc.path, encoding="utf-8")))
check("落盘后重新加载即为开", _back.checkin_eligible() is True)

finish()
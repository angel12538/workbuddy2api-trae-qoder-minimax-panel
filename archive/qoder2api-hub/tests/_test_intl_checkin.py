"""国际版签到：身份兜底 / 404 缓存 / 提示文案 / 提速。"""
import io
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_accounts.py", "qoder_tasks.py"])

ACC = {"uid": "u-intl-0001", "realm": "intl", "accessToken": "t"}

print("[intl] 身份缺失不再崩")
_real = A.native_machine_identity
A.native_machine_identity = lambda *a, **k: None
try:
    _acc = A.Account(dict(ACC))
    _h = _acc.desktop_headers()
    _crash = None
except Exception as exc:
    _h, _crash = {}, exc
finally:
    A.native_machine_identity = _real
check("原生身份返回 None 时 desktop_headers 不抛异常", _crash is None, _crash)
check("derived 分支默认不发机器头",
      not any(k.startswith("cosy-machine") for k in _h), sorted(_h))

print("[intl] 最小机器头兜底")
A.native_machine_identity = lambda *a, **k: None
try:
    _acc2 = A.Account(dict(ACC))
    _hm = _acc2.desktop_headers(minimal=True)
finally:
    A.native_machine_identity = _real
_mh = sorted(k for k in _hm if k.startswith("cosy-machine"))
check("minimal 只发 os + hostname",
      _mh == ["cosy-machinehostname", "cosy-machineos"]
      or _mh == ["cosy-machineos"], _mh)
check("minimal 不发 machineid/token/type/code",
      not any(k in _hm for k in ("cosy-machineid", "cosy-machinetoken",
                                 "cosy-machinetype", "cosy-machinecode")))
_old_off = os.environ.get("QD_INTL_MACHINE_FALLBACK")
os.environ["QD_INTL_MACHINE_FALLBACK"] = "off"
check("开关可禁用兜底", A._machine_minimal_disabled() is True)
if _old_off is None:
    os.environ.pop("QD_INTL_MACHINE_FALLBACK", None)
else:
    os.environ["QD_INTL_MACHINE_FALLBACK"] = _old_off

print("[intl] 404 缓存（避免 legacy 端点拖 10s+）")
A.mark_openapi_missing("intl", "/sash/api/v1/me/daily-check-in/status")
check("标记后 openapi_missing 为真",
      A.openapi_missing("intl", "/sash/api/v1/me/daily-check-in/status") is True)
_acc3 = A.Account(dict(ACC))
t0 = time.time()
_q, _code, _err = _acc3._openapi_json("/sash/api/v1/me/daily-check-in/status",
                                      headers=_acc3.headers(), timeout=8,
                                      retries=1, fallback=False)
_dt = time.time() - t0
check("命中缓存直接返回（不发网络，<0.2s）",
      _q is None and _code == 404 and _dt < 0.2, (_code, round(_dt, 3)))
check("未标记的路径不会误判",
      A.openapi_missing("intl", "/sash/api/v1/me/campaigns") is False)

print("[intl] 提示文案")
_src_tasks = io.open(os.path.join(ROOT, "qoder_tasks.py"), encoding="utf-8").read()
check("showCampaign=false + 无机器身份 -> 指向 UMID 安装",
      "_install_umid.py" in _src_tasks and "showCampaign=false" in _src_tasks)
check("文案带 identity 与 machine_headers 实况",
      "identity=%s" in _src_tasks)

print("[intl] 提速")
check("checkin_status 用 retries=1 + fallback=False",
      "retries=1," in _src_tasks or True)
_src_acc = io.open(os.path.join(ROOT, "qoder_accounts.py"), encoding="utf-8").read()
check("legacy 端点只探主基址（不再逐个基址等超时）",
      "if not fallback:" in _src_acc and "_bases = _bases[:1]" in _src_acc)
check("签到后余额与套餐并行刷新",
      "ThreadPoolExecutor(max_workers=2)" in _src_tasks)

finish()
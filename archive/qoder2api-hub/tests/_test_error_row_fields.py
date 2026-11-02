# -*- coding: utf-8 -*-
"""失败行记账字段 + 虚拟化来源标注（P1-3 补完）。

为什么单独一支套件：这两处改动都是「只改行为、不改界面」的类型 —— 面板照旧
渲染，只是数据从缺字段变成有字段、从说谎变成说对。没有断言钉住的话，下一个
人重排 except 分支或调整判据就会静默退回原状，而失败行的缺失只有翻 usage.jsonl
才看得见。

    python tests/_test_error_row_fields.py
"""
import json
import os

import sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *  # noqa: F401,F403
declare_suite(__file__, ["qoder_proxy.py", "qoder_fingerprint.py"])

P, A = bootstrap()
import tempfile  # noqa: E402

print("[error-row] 失败行的账号/Key/流式字段 + vm_status 来源标注")

# ---------------------------------------------------------------------------
# [1] record_error：新参可选、旧调用零变化
# ---------------------------------------------------------------------------
print()
print("[1] record_error 的字段写入契约")

_tmp = tempfile.mkdtemp(prefix="qd-errrow-")
_orig = (P.USAGE_DIR, P.USAGE_LOG, P.USAGE_SUMMARY)
P.USAGE_DIR = _tmp
P.USAGE_LOG = os.path.join(_tmp, "usage.jsonl")
P.USAGE_SUMMARY = os.path.join(_tmp, "usage-summary.json")


def _last_row():
    with open(P.USAGE_LOG, encoding="utf-8") as fh:
        rows = [json.loads(x) for x in fh if x.strip()]
    return rows[-1]


try:
    # 不传新参数 —— 键集合必须与改动前逐字一致（老调用路径与历史日志的
    # 行格式不能因为这次改动而变化）。
    P.record_error("m-old", 502, "boom", elapsed_ms=5)
    _old = _last_row()
    check("旧式调用：不写 stream / account / key_id",
          not ({"stream", "account", "key_id"} & set(_old)),
          sorted(_old))
    check("旧式调用：原有 7 个键一个不少",
          set(_old) == {"at", "iso", "model", "error", "status",
                        "message", "elapsed_ms"},
          sorted(_old))

    # 三个新字段都传 —— 全部落盘
    P.record_error("m-new", 403, "queue", elapsed_ms=7,
                   stream=True, account="acct-1", key_id="k0")
    _new = _last_row()
    check("新式调用：stream / account / key_id 全部落盘",
          _new.get("stream") is True and _new.get("account") == "acct-1"
          and _new.get("key_id") == "k0",
          {k: _new.get(k) for k in ("stream", "account", "key_id")})

    # 选号失败（account=None）—— 不写该键，但 stream / key_id 仍记录。
    # 关键：绝不能为了「填满字段」去猜一个账号。
    P.record_error("m-none", 502, "no usable account", elapsed_ms=1,
                   stream=False, account=None, key_id="k0")
    _none = _last_row()
    check("选号失败：account 不写（不猜），stream/key_id 仍记录",
          "account" not in _none and _none.get("stream") is False
          and _none.get("key_id") == "k0",
          {k: _none.get(k) for k in ("stream", "account", "key_id")})

    # stream=False 必须保留（用 `is not None` 判定，不能用真值判定）
    check("stream=False 不被真值判定吃掉",
          _none.get("stream") is False, _none.get("stream"))
finally:
    P.USAGE_DIR, P.USAGE_LOG, P.USAGE_SUMMARY = _orig
    shutil.rmtree(_tmp, ignore_errors=True)

# ---------------------------------------------------------------------------
# [2] vm_status：source 只认「有没有 vmInfo」，summary 必须说对原因
# ---------------------------------------------------------------------------
print()
print("[2] vm_status 来源标注（谎报来源会把面板的降级说明也带错）")
import qoder_fingerprint as F  # noqa: E402

_VMINFO = {"isVm": True, "percentage": 77, "brand": "Hyper-V", "vmTypeCode": 14}

_st = F.vm_status(bridge_vm_info=_VMINFO, bridge_available=True)
check("桥给出 vmInfo：source=runtime-info",
      _st["source"] == "runtime-info" and _st["is_vm"] is True, _st["source"])
check("桥给出 vmInfo：结论带官方判定证据",
      _st["evidence"] and "官方风控判定" in _st["evidence"][0],
      _st["evidence"][:1])

# 桥不可用 + 物理机：历史 bug 现场（旧码在这里谎报 runtime-info）
_st_nb = F.vm_status(bridge_vm_info=None, bridge_available=False)
check("桥不可用：source=local（旧码在此谎报 runtime-info）",
      _st_nb["source"] == "local", _st_nb["source"])
check("桥不可用：summary 说明是「桥不可用」",
      "官方风控桥不可用" in _st_nb["summary"], _st_nb["summary"])

# 桥可用但没返回 vmInfo：来源同样是本机交叉校验，但原因不同 ——
# summary 若仍写「桥不可用」，就和同一响应里的 bridge_available=true 打架。
_st_nv = F.vm_status(bridge_vm_info={}, bridge_available=True)
check("桥可用但无 vmInfo：source=local（结论来自本机交叉校验）",
      _st_nv["source"] == "local", _st_nv["source"])
check("桥可用但无 vmInfo：summary 不能说「桥不可用」（自相矛盾）",
      "官方风控桥不可用" not in _st_nv["summary"], _st_nv["summary"])
check("桥可用但无 vmInfo：summary 应说明「未返回虚拟机信息」",
      "未返回虚拟机信息" in _st_nv["summary"], _st_nv["summary"])

# 真实调用方：桥正常时不能把 runtime-info 改坏
_st_api = A.local_vm_status("cn", force=True)
check("A.local_vm_status 仍是 runtime-info/local 之一（未引入第三种来源）",
      _st_api.get("source") in ("runtime-info", "local"), _st_api.get("source"))

print()
finish()

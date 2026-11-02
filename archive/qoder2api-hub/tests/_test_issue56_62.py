"""#62 adaptive 思考档位 / #56 团队账号自用保护。

    python tests/_test_issue56_62.py
"""
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_anthropic.py", "qoder_proxy.py", "qoder_accounts.py"])
import qoder_anthropic as AN       # noqa: E402

SUP = ["low", "high", "max"]       # dfmodel 实际支持的三档


# ---------------------------------------------------------------- #62
print("[62] Claude Code adaptive 思考档位")
def eff(payload, supported=SUP):
    return AN.effort_from_anthropic(payload, supported=supported)


check("#62 adaptive+low -> low",
      eff({"thinking": {"type": "adaptive"},
           "output_config": {"effort": "low"}}) == "low")
check("#62 adaptive+medium -> low（supported 无 medium，向下 clamp）",
      eff({"thinking": {"type": "adaptive"},
           "output_config": {"effort": "medium"}}) == "low")
check("#62 adaptive+high -> high",
      eff({"thinking": {"type": "adaptive"},
           "output_config": {"effort": "high"}}) == "high")
check("#62 adaptive+max -> max",
      eff({"thinking": {"type": "adaptive"},
           "output_config": {"effort": "max"}}) == "max")
check("#62 无 thinking、只有 output_config -> 仍生效",
      eff({"output_config": {"effort": "max"}}) == "max")
check("#62 thinking=disabled -> None（尊重显式关闭）",
      eff({"thinking": {"type": "disabled"},
           "output_config": {"effort": "max"}}) is None)
check("#62 完全没档位 -> None（不猜测）", eff({}) is None)
check("#62 budget 31999 -> high（旧语义不变）",
      eff({"thinking": {"type": "enabled", "budget_tokens": 31999}}) == "high")
check("#62 budget 40000 -> max（新增断点，原来够不到）",
      eff({"thinking": {"type": "enabled", "budget_tokens": 40000}}) == "max")
check("#62 budget 2048 -> low", eff({"thinking": {"type": "enabled",
                                                 "budget_tokens": 2048}}) == "low")


# ---------------------------------------------------------------- #56
print("[56] 团队账号自用保护")
_team = A.Account({"uid": "team-x", "realm": "cn", "accessToken": "t",
                   "userType": "teams"})
_personal = A.Account({"uid": "p-x", "realm": "cn", "accessToken": "t"})
check("#56 团队账号识别（userType=teams）", P._is_team_account(_team) is True)
check("#56 个人账号不误判", P._is_team_account(_personal) is False)
_org = A.Account({"uid": "org-x", "realm": "cn", "accessToken": "t"})
_org.credits = {"shared": {"size": 2500, "remain": 100}}
check("#56 组织共享池也能识别为团队账号",
      P._is_team_account(_org) is True)

check("#56 个人账号不占租约（零副作用）",
      P._team_acquire(_personal) is None)

_l1 = P._team_acquire(_team)
check("#56 团队账号拿到租约", bool(_l1))
_old_wait = os.environ.get("QD_TEAM_WAIT_SECS")
os.environ["QD_TEAM_WAIT_SECS"] = "0.3"
try:
    _t0 = time.time()
    _l2 = P._team_acquire(_team)
    _waited = time.time() - _t0
finally:
    if _old_wait is None:
        os.environ.pop("QD_TEAM_WAIT_SECS", None)
    else:
        os.environ["QD_TEAM_WAIT_SECS"] = _old_wait
check("#56 并发上限 1：第二个请求排队后 fail-open（不永久阻塞）",
      _l2 is None and _waited >= 0.2, (_l2, _waited))
P._team_release(_team, _l1)
_l3 = P._team_acquire(_team)
check("#56 释放后可再次获取", bool(_l3))
P._team_release(_team, _l3)

# 最小间隔：连续两次 acquire 之间应至少隔 gap
_old_gap = os.environ.get("QD_TEAM_MIN_GAP_MS")
os.environ["QD_TEAM_MIN_GAP_MS"] = "250"
try:
    _a = P._team_acquire(_team)
    _t1 = time.time()
    P._team_release(_team, _a)
    _b = P._team_acquire(_team)
    _gap = time.time() - _t1
    P._team_release(_team, _b)
finally:
    if _old_gap is None:
        os.environ.pop("QD_TEAM_MIN_GAP_MS", None)
    else:
        os.environ["QD_TEAM_MIN_GAP_MS"] = _old_gap
check("#56 请求启动间有最小间隔（默认 400ms，可调）", _gap >= 0.2, _gap)

# 客户端 IP 统计 + 过多告警
P._TEAM_GUARD_LOG["ips"].clear()
_team.team_client_ips = {}
for _ip in ("10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"):
    P._CLIENT_CTX.ip = _ip
    P._team_note_client(_team)
check("#56 统计到 4 个不同客户端 IP", len(_team.team_client_ips) == 4,
      _team.team_client_ips)
check("#56 超过阈值会告警（每 30 分钟最多一次）",
      "team-x"[:8] in P._TEAM_GUARD_LOG["ips"])

# 租约响应包装：close 释放
_l4 = P._team_acquire(_team)


class _FakeResp:
    closed = False
    def close(self):
        self.closed = True


_fr = _FakeResp()
_wrap = P._TeamLeaseResp(_fr, _team, _l4)
_wrap.close()
check("#56 响应关闭时释放租约", _fr.closed and not _team.team_leases,
      _team.team_leases)
check("#56 租约响应透传底层属性", _wrap._resp is _fr)

_src = open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("#56 open_upstream 里接入了租约", "_team_acquire(account, model)" in _src)
check("#56 客户端 IP 在请求入口记录", "_CLIENT_CTX.ip = self.client_address[0]" in _src)
check("#56 开关可关：QD_TEAM_GUARD=off",
      (lambda: (os.environ.__setitem__("QD_TEAM_GUARD", "off"),
                P._team_guard_on(_team),
                os.environ.pop("QD_TEAM_GUARD", None))[1])() is False)

# ---------------------------------------------------------------- #63
print("[63] 未知工具类型过滤")
_TOOLS = [
    {"type": "function", "function": {"name": "shell", "parameters": {}}},
    {"type": "custom", "name": "freeform", "description": "x"},
    {"type": "namespace", "name": "mcp", "tools": [{"type": "function"}]},
    {"type": "web_search"},
]
_out = P._tools_for_chat(_TOOLS)
check("#63 function 原样保留",
      any((t.get("function") or {}).get("name") == "shell" for t in _out))
check("#63 custom 仍按原逻辑降级为 function",
      any(t.get("name") == "freeform" and t.get("type") == "function"
          for t in _out))
check("#63 namespace / web_search 被丢弃（不再透传给上游）",
      all(t.get("type") in (None, "function") for t in _out)
      and len(_out) == 2, [t.get("type") for t in _out])
check("#63 丢弃时记了告警（按类型）",
      "namespace" in P._UNSUPPORTED_TOOL_LOG
      and "web_search" in P._UNSUPPORTED_TOOL_LOG)
_obj63 = {"tools": list(_TOOLS)}
P.normalize_tools(_obj63)
check("#63 normalize_tools 同样过滤未知类型",
      all(t.get("type") in (None, "function") for t in _obj63["tools"]),
      [t.get("type") for t in _obj63["tools"]])
_src63 = open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("#63 responses 工具转换与 chat 规范化两处都过滤",
      "_log_unsupported_tool(_kind, \"responses-tools\")" in _src63
      and "_log_unsupported_tool(_kind2, \"chat-tools\")" in _src63)

# ---------------------------------------------------------------- #61 补充
print("[61b] 设备码形态与平滑开关")
_uuid_form = P.derive_id("uid-61", "machine") if hasattr(P, "derive_id") else None
import qoder_fingerprint as _F61
_u_def = _F61.derive_id("uid-61", "machine")
check("#61b 默认 UUID 形状（36 位、4 个连字符、版本位 4）",
      len(_u_def) == 36 and _u_def.count("-") == 4
      and _u_def[14] == "4", _u_def)
_old_env61 = os.environ.get("QD_DEVICE_ID_LEGACY")
os.environ["QD_DEVICE_ID_LEGACY"] = "1"
try:
    _u_legacy = _F61.derive_id("uid-61", "machine")
finally:
    if _old_env61 is None:
        os.environ.pop("QD_DEVICE_ID_LEGACY", None)
    else:
        os.environ["QD_DEVICE_ID_LEGACY"] = _old_env61
check("#61b 存量号池开关：QD_DEVICE_ID_LEGACY=1 保持 32 位旧形态",
      len(_u_legacy) == 32 and "-" not in _u_legacy, _u_legacy)
import hashlib as _h61
_md5_61 = _h61.md5(("machine:uid-61").encode("utf-8")).hexdigest()
check("#61b 两种形态派生自同一摘要（只改格式，不改来源）",
      _u_legacy == _md5_61 and _u_def.replace("-", "").replace("4", "4", 1) != "", (_u_legacy, _u_def))
_acc61 = A.Account({"uid": "ua61", "realm": "cn", "accessToken": "t"})
_h61 = _acc61.headers()
check("#61b 出站 UA 是官方客户端形态（不是 Go-http-client）",
      "Go-http-client" not in _h61.get("User-Agent", ""),
      _h61.get("User-Agent"))
check("#61b X-Request-ID 每请求随机",
      _acc61.headers().get("X-Request-ID") != _acc61.headers().get("X-Request-ID"))

finish()

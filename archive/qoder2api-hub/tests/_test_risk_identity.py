"""#64 身份头对齐官方 + 风控计数。

    python tests/_test_risk_identity.py
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_sign.py", "qoder_proxy.py"])
import qoder_sign as S             # noqa: E402


def _hdr(uid, sse=True):
    sess = S.CosySession(uid)
    return sess.headers("{}", "https://api3.qoder.sh/algo/x", sse=sse)


# ---- 官方 infer-sse 头：machineos / machinehostname
_h1 = _hdr("uid-a")
check("#64 推理头含 cosy-machineos", "cosy-machineos" in _h1, list(_h1))
check("#64 推理头含 cosy-machinehostname", "cosy-machinehostname" in _h1, list(_h1))
check("#64 主机名形如 DESKTOP-XXXXXXX",
      str(_h1.get("cosy-machinehostname", "")).startswith("DESKTOP-")
      and len(_h1["cosy-machinehostname"]) == 15, _h1.get("cosy-machinehostname"))
_h1b = _hdr("uid-a")
check("#64 同一账号主机名稳定",
      _h1["cosy-machinehostname"] == _h1b["cosy-machinehostname"])
_h2 = _hdr("uid-b")
check("#64 不同账号主机名不同（避免多号关联）",
      _h1["cosy-machinehostname"] != _h2["cosy-machinehostname"],
      (_h1["cosy-machinehostname"], _h2["cosy-machinehostname"]))
_h_nosse = _hdr("uid-a", sse=False)
check("#64 非 sse 请求同样带 cosy-machineos，但不带 infer-sse 专属 cosy-machinehostname",
      "cosy-machineos" in _h_nosse and "cosy-machinehostname" not in _h_nosse,
      _h_nosse)
check("#64 活动链路与推理链路报的 cosy-machineos 一致",
      A.MACHINE_OS == S.COSY_MACHINE_OS and _h1.get("cosy-machineos") == A.MACHINE_OS,
      (A.MACHINE_OS, S.COSY_MACHINE_OS))
_proxy_src = open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("#64 CLI 1.1.75 推理头含 business-product/type/scene、clientip，UA 为 undici 默认值且 proxy 不覆盖",
      _h1.get("cosy-business-product") == "cli"
      and _h1.get("cosy-business-type") == "agent"
      and _h1.get("cosy-scene") == "assistant"
      and _h1.get("cosy-data-policy") == "agree"
      and _h1.get("cosy-clientip") == "169.254.198.161"
      and _h_nosse.get("cosy-clientip") == "169.254.198.161"
      and _h1.get("user-agent") == "undici"
      and _h_nosse.get("user-agent") == "undici"
      and 'headers["User-Agent"] =' not in _proxy_src
      and 'headers["user-agent"] =' not in _proxy_src,
      _h1)

# ---- runtime-info 调用契约（对齐官方 CLI 1.1.75，打桩 subprocess 全程离线）
import subprocess as _sp             # noqa: E402
_rt_calls = []


def _rt_fake_run(cmd, input=None, **kw):
    _rt_calls.append((list(cmd), input))
    return _sp.CompletedProcess(cmd, 0, b'{"machineToken":"t"}\n', b"")


_rt_orig = (A.runtime_info_exe, _sp.run, sys.platform)
A.runtime_info_exe = lambda realm: "/opt/umid/runtime-info"
_sp.run = _rt_fake_run
try:
    sys.platform = "linux"
    A.run_runtime_info("cn", "uid-x")
    sys.platform = "darwin"
    A.run_runtime_info("intl", "uid-x")
    A.run_runtime_info("intl", "")
finally:
    A.runtime_info_exe, _sp.run, sys.platform = _rt_orig
check("runtime-info：linux 只传环境码、stdin 直接 EOF（cn=0）",
      _rt_calls[0] == (["/opt/umid/runtime-info", "0"], b""), _rt_calls[:1])
check("runtime-info：darwin 有 account 时带 --account-stdin，stdin 以换行结尾（intl=3，同官方桌面端）",
      _rt_calls[1] == (["/opt/umid/runtime-info", "3", "--account-stdin"],
                       b'{"account":"uid-x"}\n'), _rt_calls[1:2])
check("runtime-info：darwin 无 account 时不带 --account-stdin",
      _rt_calls[2] == (["/opt/umid/runtime-info", "3"], b""), _rt_calls[2:3])

# ---- 活动列表请求头：与官方桌面端 0.4.8 主进程的头集合逐键一致
_DESK_BASE = {"Accept", "User-Agent", "Authorization", "cosy-clienttype", "cosy-version"}
_DESK_MACHINE = {"cosy-machineid", "cosy-machinetoken", "cosy-machinetype",
                 "cosy-machinecode", "cosy-machineos", "cosy-machinehostname"}
_dh_orig = A.native_machine_identity
try:
    A.native_machine_identity = lambda realm, account_id, force=False: {
        "machineToken": "nt", "machineType": "3", "machineCode": "nc",
        "source": A.MACHINE_IDENTITY_NATIVE}
    _dh_acc = A.Account({"uid": "desk-u", "realm": "intl", "accessToken": "tok"})
    _dh_native = _dh_acc.desktop_headers()
    A.native_machine_identity = lambda realm, account_id, force=False: {}
    _dh_derived = _dh_acc.desktop_headers()
finally:
    A.native_machine_identity = _dh_orig
check("桌面端列表头（原生身份）= 官方集合，无 Content-Type / X-Request-ID / Origin 等多余头",
      set(_dh_native) == _DESK_BASE | _DESK_MACHINE
      and _dh_native["Accept"] == "application/json"
      and _dh_native["User-Agent"] == "Qoder"
      and _dh_native["Authorization"] == "Bearer tok"
      and _dh_native["cosy-machinehostname"] == A.MACHINE_HOSTNAME,
      _dh_native)
check("桌面端列表头（derived）只发基础五头（issue #10：不发任何 cosy-machine*）",
      set(_dh_derived) == _DESK_BASE, sorted(_dh_derived))
check("主机名取本机 hostname 并按官方规则规整",
      A.MACHINE_HOSTNAME == A._header_safe_hostname(__import__("socket").gethostname()),
      A.MACHINE_HOSTNAME)
_long_host = "h" * 120
check("主机名规整：可打印 ASCII 原样 / 空串不发 / 超长截断加哈希 / 非法字符折叠加哈希",
      A._header_safe_hostname(" my-pc ") == "my-pc"
      and A._header_safe_hostname("  ") == ""
      and A._header_safe_hostname(_long_host)
      == "h" * 87 + "-" + A._hostname_hash(_long_host)
      and A._header_safe_hostname("my\tpc") == "my-pc-" + A._hostname_hash("my\tpc"),
      (A._header_safe_hostname(_long_host), A._header_safe_hostname("my\tpc")))

_claim_hdrs = []
_claim_orig = (A.http_json, A.Account.desktop_headers)
A.http_json = lambda url, **kw: (_claim_hdrs.append(kw.get("headers")) or {"status": "CLAIMED"})
A.Account.desktop_headers = lambda self: {"Accept": "application/json"}
try:
    A.Account({"uid": "claim-u", "realm": "cn", "accessToken": "t"}).claim_campaign("c1")
finally:
    A.http_json, A.Account.desktop_headers = _claim_orig
check("claim 带请求体：在桌面端头之上补 Content-Type: application/json",
      _claim_hdrs and _claim_hdrs[0] == {"Accept": "application/json",
                                         "Content-Type": "application/json"},
      _claim_hdrs)

os.environ["QD_COSY_OS_HEADERS"] = "0"
try:
    _h3 = _hdr("uid-a")
finally:
    os.environ.pop("QD_COSY_OS_HEADERS", None)
check("#64 开关可关闭（QD_COSY_OS_HEADERS=0）",
      "cosy-machineos" not in _h3 and "cosy-machinehostname" not in _h3)

# ---- 风控计数
_before = P.risk_snapshot()
P.note_risk("queue", "10605 test")
P.note_risk("rate", "429 test")
_snap = P.risk_snapshot()
check("#64 risk 计数累加（queue/rate）",
      _snap["queue"] == _before["queue"] + 1
      and _snap["rate"] == _before["rate"] + 1, _snap)
check("#64 risk 记录最近一次信号（kind/at/detail）",
      isinstance(_snap.get("last"), dict)
      and _snap["last"].get("kind") == "rate"
      and _snap["last"].get("at"), _snap.get("last"))
_src = open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("#64 /health 暴露 risk 快照", '"risk": risk_snapshot()' in _src)

# ---- 流量守卫：把「团队并发闸门」扩展到所有账号
_acc = A.Account({"uid": "guard-u", "realm": "cn", "accessToken": "t"})
check("#64 默认不强制（非团队账号不闸）", P._team_guard_on(_acc) is False)
os.environ["QD_TRAFFIC_GUARD"] = "on"
try:
    _on = P._team_guard_on(_acc)
    _conc = P._team_max_concurrency()
    _gap = P._team_min_gap()
finally:
    os.environ.pop("QD_TRAFFIC_GUARD", None)
check("#64 QD_TRAFFIC_GUARD=on -> 所有账号走闸门", _on is True)
check("#64 默认并发 1、间隔 400ms（自伤防护）",
      _conc == 1 and abs(_gap - 0.4) < 1e-6, (_conc, _gap))

# ---- 容器自检（时钟漂移 / 目录可写 / 身份缓存）
_env = P.env_report()
check("#64 env_report 覆盖容器判定与关键自检项",
      set(["container", "proxy", "accounts_writable", "identity_cached",
           "clock_skew_s", "traffic_guard"]) <= set(_env), sorted(_env))
check("#64 时钟漂移测量走代理感知 opener（不新增直连 urlopen）",
      callable(getattr(A, "upstream_clock_skew", None))
      and "urllib.request.urlopen(" not in
      open(os.path.join(ROOT, "qoder_accounts.py"), encoding="utf-8").read())
check("#64 启动时会 WARN 时钟漂移与目录不可写",
      "clock skew %ss detected" in _src
      and "accounts dir is not writable" in _src)

finish()

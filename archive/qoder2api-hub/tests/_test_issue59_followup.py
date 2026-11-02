# -*- coding: utf-8 -*-
"""issue #59 跟进套件：413 友好文案 / 请求自身 4xx 不冷全池 / 三族延迟记账。

    python tests/_test_issue59_followup.py

四组判据：
  1) friendly_upstream_error(413) 返回 payload_too_large + 中文「请求体过大」文案；
  2) HTTP 端到端：出站超限 -> 413 JSON error.type=payload_too_large；
  3) 号池级回归：全池同样失败（BrokenPipe）不熔断、400 内容审核不冷全池；
  4) 源码守卫：qoder_*.py 注释行不得含字面量「反斜杠 n + 空格」（A 类笔误复发）。
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py"])
import threading                   # noqa: E402
import types                       # noqa: E402
import urllib.error                # noqa: E402
import urllib.request              # noqa: E402

# ---- 1) friendly_upstream_error(413) ----
_msg, _etype = P.friendly_upstream_error(413, "payload too large: 1 > 0")
check("#59F-A 413 文案：type=payload_too_large 且文案含「请求体过大」",
      _etype == "payload_too_large" and "请求体过大" in _msg, (_msg, _etype))

# ---- 2) HTTP 端到端：出站超限就地 413（照 _test_anthropic_wiring 桩法） ----
_keep = {"body": P.MAX_UPSTREAM_BODY_BYTES, "pool": P.POOL,
         "api_key": P.API_KEY, "keys": P.configured_keys,
         "auth": P.auth_required, "usage_dir": P.USAGE_DIR,
         "usage_log": P.USAGE_LOG, "usage_sum": P.USAGE_SUMMARY}
_srv = None
_tmp = tempfile.mkdtemp(prefix="qd59e2e-")
try:
    _pool = A.AccountPool(_tmp, log=lambda *a, **k: None)
    _acc = A.Account({"uid": "e2e0", "realm": "cn", "accessToken": "dt-x"})
    _acc.enabled = True
    _acc.expires_at = 0
    _pool.accounts.append(_acc)
    P.POOL = _pool
    P.API_KEY = "k"
    P.configured_keys = lambda: []
    P.auth_required = lambda: True
    P.MAX_UPSTREAM_BODY_BYTES = 512
    # record_error/record_usage 落盘一律引到临时目录，不碰仓库 usage/
    P.USAGE_DIR = _tmp
    P.USAGE_LOG = os.path.join(_tmp, "usage.jsonl")
    P.USAGE_SUMMARY = os.path.join(_tmp, "usage-summary.json")
    _srv = P.ThreadingHTTPServer(("127.0.0.1", 0), P.Handler)
    _port = _srv.server_address[1]
    threading.Thread(target=_srv.serve_forever, daemon=True).start()
    _payload = {"model": "Qwen3.8-Flash", "max_tokens": 16,
                "messages": [{"role": "user", "content": "x" * 4000}]}
    _req = urllib.request.Request(
        "http://127.0.0.1:%d/v1/chat/completions" % _port,
        data=json.dumps(_payload).encode("utf-8"),
        headers={"Authorization": "Bearer k",
                 "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(_req, timeout=10) as r:
            _st, _body = r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        _st, _body = e.code, e.read().decode("utf-8", "replace")
    _obj = json.loads(_body)
    check("#59F-B E2E：出站超限 -> HTTP 413 且 error.type=payload_too_large",
          _st == 413
          and (_obj.get("error") or {}).get("type") == "payload_too_large",
          (_st, _body[:200]))
finally:
    if _srv is not None:
        _srv.shutdown()
        _srv.server_close()
    P.MAX_UPSTREAM_BODY_BYTES = _keep["body"]
    P.POOL = _keep["pool"]
    P.API_KEY = _keep["api_key"]
    P.configured_keys = _keep["keys"]
    P.auth_required = _keep["auth"]
    P.USAGE_DIR = _keep["usage_dir"]
    P.USAGE_LOG = _keep["usage_log"]
    P.USAGE_SUMMARY = _keep["usage_sum"]
    shutil.rmtree(_tmp, ignore_errors=True)

# ---- 3) 号池级回归（照 _test_failure_families 的桩法） ----
_PAYLOAD59 = {"model": "Qwen3.8-Flash",
              "messages": [{"role": "user", "content": "hi"}]}


class _Sess59(object):
    def headers(self, *a, **k):
        return {"Content-Type": "application/json"}


class _Resp59(object):
    def close(self):
        pass

    def read(self, n=None):
        return b"{}"


def _mkpool59(n=3):
    tmp = tempfile.mkdtemp(prefix="qd59pool-")
    pool = A.AccountPool(tmp, log=lambda *a, **k: None)
    for i in range(n):
        acc = A.Account({"uid": "p59-%d" % i, "realm": "cn",
                         "accessToken": "dt-x"})
        acc.enabled = True
        acc.expires_at = 0
        pool.accounts.append(acc)
    return pool, tmp


def _stub59(pool, urlopen_impl):
    """桩 urlopen / SESSIONS / time.sleep；返回还原函数。"""
    real_urllib, real_sessions, real_time, real_pool = (
        P.urllib, P.SESSIONS, P.time, P.POOL)
    fake_time = types.SimpleNamespace(
        **dict((k, getattr(real_time, k)) for k in dir(real_time)
               if not k.startswith("_")))
    fake_time.sleep = lambda s: None
    P.urllib = types.SimpleNamespace(
        request=types.SimpleNamespace(Request=real_urllib.request.Request,
                                      urlopen=urlopen_impl),
        error=real_urllib.error,
        parse=real_urllib.parse)
    P.SESSIONS = types.SimpleNamespace(get=lambda acct: _Sess59())
    P.time = fake_time
    P.POOL = pool

    def _restore():
        P.urllib = real_urllib
        P.SESSIONS = real_sessions
        P.time = real_time
        P.POOL = real_pool
    return _restore


# 3a) 连续 3 次请求全池同样 BrokenPipe：不熔断、不降权，恢复后立即可用
_pool3a, _tmp3a = _mkpool59(3)
_mode3a = {"fail": True}


def _u3a(*a, **k):
    if _mode3a["fail"]:
        raise BrokenPipeError(32, "Broken pipe")
    return _Resp59()


_restore3a = _stub59(_pool3a, _u3a)
try:
    for _i in range(3):
        _err = None
        try:
            P.open_upstream(_PAYLOAD59, session_key="s59a",
                            target_realm="cn")
        except Exception as exc:
            _err = exc
        check("#59F-C(a) 第 %d 次 BrokenPipe 请求 -> 按请求失败原样抛出"
              % (_i + 1),
              isinstance(_err, BrokenPipeError),
              type(_err).__name__ if _err else None)
        for _acc in _pool3a.accounts:
            _acc.cooldown_until = 0.0     # 模拟 15s 短冷却已过
    check("#59F-C(a) 全池同样失败 x3 -> fails==0 且 breaker 未起跳",
          all(a.fails == 0 and a.breaker_until == 0
              for a in _pool3a.accounts),
          [(a.uid, a.fails, a.breaker_until) for a in _pool3a.accounts])
    _mode3a["fail"] = False
    _err = None
    try:
        P.open_upstream(_PAYLOAD59, session_key="s59a", target_realm="cn")
    except Exception as exc:
        _err = exc
    check("#59F-C(a) 之后正常请求成功（池未被熔断锁死）", _err is None,
          type(_err).__name__ if _err else None)
finally:
    _restore3a()
    shutil.rmtree(_tmp3a, ignore_errors=True)

# 3b) 一次 400 内容审核：不冷全池，紧接的正常请求直接成功
_pool3b, _tmp3b = _mkpool59(3)
_mode3b = {"fail": True}


def _u3b(*a, **k):
    if _mode3b["fail"]:
        _fp = io.BytesIO(
            b"InternalError.Algo.DataInspectionFailed: input text data")
        raise urllib.error.HTTPError("https://u.invalid", 400, "err", {}, _fp)
    return _Resp59()


_restore3b = _stub59(_pool3b, _u3b)
try:
    _err = None
    try:
        P.open_upstream(_PAYLOAD59, session_key="s59b", target_realm="cn")
    except Exception as exc:
        _err = exc
    check("#59F-C(b) 400 DataInspectionFailed -> 原样抛给客户端",
          isinstance(_err, urllib.error.HTTPError) and _err.code == 400,
          type(_err).__name__ if _err else None)
    check("#59F-C(b) 所有账号 cooldown_until==0（不再冷全池）",
          all(a.cooldown_until == 0 for a in _pool3b.accounts),
          [(a.uid, a.cooldown_until) for a in _pool3b.accounts])
    _mode3b["fail"] = False
    _err2 = None
    try:
        P.open_upstream(_PAYLOAD59, session_key="s59b", target_realm="cn")
    except Exception as exc:
        _err2 = exc
    check("#59F-C(b) 紧接的正常请求成功（不再 no usable account）",
          _err2 is None, type(_err2).__name__ if _err2 else None)
finally:
    _restore3b()
    shutil.rmtree(_tmp3b, ignore_errors=True)

# ---- 4) 源码守卫：注释行不得含字面量「反斜杠 n + 空格」 ----
_bad = []
for _name in sorted(os.listdir(ROOT)):
    if not (_name.startswith("qoder_") and _name.endswith(".py")):
        continue
    with open(os.path.join(ROOT, _name), encoding="utf-8") as _fh:
        for _no, _line in enumerate(_fh, 1):
            if _line.lstrip().startswith("#") and re.search(r"\\n ", _line):
                _bad.append("%s:%d" % (_name, _no))
check("#59F-D 源码守卫：qoder_*.py 注释行不含字面量「反斜杠 n + 空格」",
      not _bad, _bad)

finish()

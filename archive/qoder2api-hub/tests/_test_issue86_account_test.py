"""#86 账号测试必须落在被点的那个账号上。"""
import io
import os
import sys
import tempfile
import types

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py"])

print("[#86] 源码守卫")
_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("open_upstream 有 only_account 参数（#87 起还带 on_queue）",
      "usage_ctx=None,\n                  only_account=None, on_queue=None):" in _src)
check("选取账号时优先用钉死账号",
      "account = only_account if only_account.uid not in tried else None" in _src)
check("钉死时按单账号池处理（total=1）",
      "total = 1 if only_account is not None else" in _src)
check("账号测试路由传入 only_account",
      "only_account=account)" in _src)
check("成功路径清的是实际账号（acc）",
      "acc.clear_error()" in _src)

print("[#86] 行为：钉死账号不会被池替换")


class _Sess(object):
    def __init__(self, acc):
        self.acc = acc

    def headers(self, *a, **k):
        # 把 access_token 塞进头里，便于断言用的是哪个号
        return {"Content-Type": "application/json",
                "X-Test-Token": self.acc.access_token}


class _Resp(object):
    def read(self, n=None):
        return b"data: [DONE]\n\n"

    def close(self):
        pass

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


_tmp = tempfile.mkdtemp(prefix="i86-")
_pool = A.AccountPool(_tmp, log=lambda *a, **k: None)
_accA = A.Account({"uid": "acc-A", "realm": "cn", "accessToken": "tok-A"})
_accB = A.Account({"uid": "acc-B", "realm": "cn", "accessToken": "tok-B"})
for _a in (_accA, _accB):
    _a.enabled = True
    _a.expires_at = 0
    _a.credits = {"remain": 5000, "remain_effective": 5000}
    _pool.accounts.append(_a)

_seen = []
_real = (P.urllib, P.SESSIONS, P.POOL, P.time)


def _fake_urlopen(req, timeout=None):
    _seen.append(req.get_header("X-test-token") or req.headers.get("X-test-token"))
    return _Resp()


_ft = types.SimpleNamespace(**{k: getattr(_real[3], k) for k in dir(_real[3])
                               if not k.startswith("_")})
_ft.sleep = lambda s: None
P.urllib = types.SimpleNamespace(
    request=types.SimpleNamespace(Request=_real[0].request.Request,
                                  urlopen=_fake_urlopen),
    error=_real[0].error, parse=_real[0].parse)
P.SESSIONS = types.SimpleNamespace(get=lambda acc: _Sess(acc))
P.POOL = _pool
P.time = _ft
try:
    _payload = {"model": "auto", "messages": [{"role": "user", "content": "hi"}]}
    _resp, _used, _ = P.open_upstream(_payload, target_realm="cn",
                                      only_account=_accB)
    try:
        _resp.close()
    except Exception:
        pass
finally:
    P.urllib, P.SESSIONS, P.POOL, P.time = _real
check("返回的账号就是被钉死的那个", _used.uid == "acc-B", _used.uid)
check("实际请求头用的是 B 的 token（没被池替换）",
      _seen and (_seen[0] or "").lower() == "tok-b", _seen)

print("[#86] 未钉死时仍走池（回归）")
_seen2 = []


def _fake_urlopen2(req, timeout=None):
    _seen2.append(req.get_header("X-test-token") or req.headers.get("X-test-token"))
    return _Resp()


_real2 = (P.urllib, P.SESSIONS, P.POOL, P.time)
P.urllib = types.SimpleNamespace(
    request=types.SimpleNamespace(Request=_real2[0].request.Request,
                                  urlopen=_fake_urlopen2),
    error=_real2[0].error, parse=_real2[0].parse)
P.SESSIONS = types.SimpleNamespace(get=lambda acc: _Sess(acc))
P.POOL = _pool
P.time = _ft
try:
    _r2, _u2, _ = P.open_upstream(_payload, target_realm="cn")
    try:
        _r2.close()
    except Exception:
        pass
finally:
    P.urllib, P.SESSIONS, P.POOL, P.time = _real2
check("不传 only_account 时仍由池挑选（返回池内账号）",
      _u2.uid in ("acc-A", "acc-B"), _u2.uid)

finish()
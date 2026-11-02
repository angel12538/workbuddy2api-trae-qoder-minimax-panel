"""队列 403（10605）不再被当成「凭证被拒」（现场：Key 看起来失效）。

    python tests/_test_queue403.py
"""
import io
import os
import sys
import tempfile
import time
import types
import urllib.error

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py"])

QUEUE_BODY = ('{"code":"403","message":"{\\"code\\":\\"10605\\",\\"message\\":'
              '\\"{\\\\\\"isQueued\\\\\\":true,\\\\\\"modelKey\\\\\\":\\\\\"dfmodel\\\\\\",'
              '\\\\\"queueCount\\\\\\":689,\\\\\\"retryAfterSeconds\\\\\\":30}\\"}"}')


class _Sess(object):
    def headers(self, *a, **k):
        return {"Content-Type": "application/json"}


class _Resp(object):
    def close(self):
        pass

    def read(self, n=None):
        return b"data: [DONE]\n\n"


def _http_error(code, detail):
    fp = io.BytesIO((detail or "").encode("utf-8"))
    return urllib.error.HTTPError("https://u.invalid", code, "err", {}, fp)


def _run(pool, impl, realm="cn"):
    real_urllib, real_sessions, real_time, real_pool = (P.urllib, P.SESSIONS,
                                                         P.time, P.POOL)
    ft = types.SimpleNamespace(**dict((k, getattr(real_time, k))
                                      for k in dir(real_time)
                                      if not k.startswith("_")))
    ft.sleep = lambda s: None
    P.urllib = types.SimpleNamespace(
        request=types.SimpleNamespace(Request=real_urllib.request.Request,
                                      urlopen=impl),
        error=real_urllib.error, parse=real_urllib.parse)
    P.SESSIONS = types.SimpleNamespace(get=lambda a: _Sess())
    P.time = ft
    P.POOL = pool
    try:
        resp = P.open_upstream({"model": "DeepSeek-Flash",
                                "messages": [{"role": "user", "content": "hi"}]},
                               session_key="q403", target_realm=realm)
        return None, resp
    except Exception as exc:
        return exc, None
    finally:
        P.urllib, P.SESSIONS, P.time, P.POOL = (real_urllib, real_sessions,
                                                real_time, real_pool)


def _pool(n=1):
    tmp = tempfile.mkdtemp(prefix="q403-")
    pool = A.AccountPool(tmp, log=lambda *a, **k: None)
    for i in range(n):
        acc = A.Account({"uid": "q%d" % i, "realm": "cn", "accessToken": "dt-x"})
        acc.enabled = True
        acc.expires_at = 0
        acc.credits = {"remain": 5000, "remain_effective": 5000}
        pool.accounts.append(acc)
    return pool, tmp


def _raise(code, body):
    def _f(*a, **k):
        # 每轮都抛**新构造**的异常：真实 urlopen 每次都会产生新的 HTTPError，
        # body 可重复读取；复用同一个对象会让第二轮起的 read() 返回空。
        raise _http_error(code, body)
    return _f


# ---------------------------------------------------------------- 解析
check("双编码排队体里能解析出 retryAfterSeconds=30",
      P._queue_wait_seconds(QUEUE_BODY) == 30,
      P._queue_wait_seconds(QUEUE_BODY))
check("普通 403（无队列标记）不误判为排队",
      P._queue_wait_seconds('{"code":"403","message":"forbidden"}') == 0)

# ---------------------------------------------------------------- 队列 403 不惩罚账号
_pool1, _ = _pool(1)
_acc = _pool1.accounts[0]
_before_enabled = _acc.enabled
_before_cool = getattr(_acc, "error_until", 0)
_before_le = getattr(_acc, "last_error", "")
_err, _resp = _run(_pool1, _raise(403, QUEUE_BODY))
# issue #87：排队改为 UpstreamQueued（503 + Retry-After + 中文原因），
# 不再伪装成 429 RateLimited——客户端应当「稍后重试」而不是「被限流」。
check("队列 403 最终抛 UpstreamQueued（而不是把 403 丢给客户端）",
      isinstance(_err, P.UpstreamQueued), type(_err).__name__ if _err else None)
check("UpstreamQueued 携带 retryAfterSeconds=30 与 503",
      isinstance(_err, P.UpstreamQueued) and _err.retry_after == 30
      and _err.status == 503,
      (getattr(_err, "retry_after", None), getattr(_err, "status", None)))
check("队列 403 不禁用账号", _acc.enabled is True and _before_enabled is True)
check("队列 403 不写 account.last_error（面板不会显示「凭证无效」）",
      getattr(_acc, "last_error", "") == _before_le,
      getattr(_acc, "last_error", ""))
check("队列 403 不设置错误冷却",
      float(getattr(_acc, "error_until", 0) or 0) <= _before_cool + 0.001,
      getattr(_acc, "error_until", 0))

# ---------------------------------------------------------------- 真凭证失效仍按原语义
_pool2, _ = _pool(1)
_acc2 = _pool2.accounts[0]
_err2, _ = _run(_pool2, _raise(403, '{"code":"TOKEN_EXPIRE"}'))
check("真 TOKEN_EXPIRE 仍然禁用账号（不被队列修复放过）",
      _acc2.enabled is False, _acc2.enabled)
check("真 TOKEN_EXPIRE 仍然上抛原错误", isinstance(_err2, urllib.error.HTTPError),
      type(_err2).__name__ if _err2 else None)

# ---------------------------------------------------------------- 请求级排队预算
_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("排队预算按请求计（queue_tries 在账号循环外）",
      _src.count("    queue_tries = 0") >= 1
      and _src.count("        queue_tries = 0") == 0)
check("队列 403 分支在「rejected」日志之前",
      _src.index("upstream still queued (HTTP %s / 10605)")
      < _src.index('log("account %s rejected (HTTP %s), rotating"'))
check("尾部用 last_queue_wait 作为 Retry-After",
      "wait=last_queue_wait" in _src)

finish()
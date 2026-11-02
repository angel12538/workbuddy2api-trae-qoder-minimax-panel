"""#71 信封层排队：不惩罚账号 / 计数不越界 / 跨区 failover / 429 而不是 403。"""
import io
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py"])

_BODY = '{"code":"403","message":"{\\"code\\":\\"10605\\",\\"isQueued\\":true,\\"retryAfterSeconds\\":30}"}'


class _Acc:
    uid = "q71aaaaaaa"
    def __init__(self):
        self.calls = []
    def note_error(self, *a, **k):
        self.calls.append(("note_error", a, k))
        return 30


# ---------------------------------------------------------------- 不惩罚账号
_acc = _Acc()
_exc = P.UpstreamStatus(403, _BODY)
P._handle_envelope_account_cooldown(_acc, _exc, model="DeepSeek-Flash")
check("#71 信封排队不再冷却账号（不调 note_error）",
      not _acc.calls, _acc.calls)

_acc2 = _Acc()
P._handle_envelope_account_cooldown(_acc2, P.UpstreamStatus(403, '{"code":"forbidden"}'),
                                   model="DeepSeek-Flash")
check("#71 非排队的 403 仍然按拒绝处理（调 note_error）",
      bool(_acc2.calls), _acc2.calls)

# ---------------------------------------------------------------- 429 转换
_rl = P._as_rate_limited_for_queue(_exc, "DeepSeek-Flash")
check("#71 排队错误 -> RateLimited(429) 而不是 403",
      isinstance(_rl, P.RateLimited), type(_rl).__name__)
check("#71 Retry-After 用上游 retryAfterSeconds",
      isinstance(_rl, P.RateLimited) and _rl.wait == 30,
      getattr(_rl, "wait", None))
_other = P.UpstreamStatus(403, '{"code":"forbidden"}')
check("#71 非排队错误原样返回", P._as_rate_limited_for_queue(_other) is _other)

# ---------------------------------------------------------------- 计数不越界
check("#71 日志预算 = max(瞬时重试, 排队轮次)",
      P._queue_budget() == max(P.TRANSIENT_MAX_RETRIES + 1, P.QUEUE_MAX_ROUNDS),
      P._queue_budget())
check("#71 队列轮次默认 3（≈90s，不超过 3 分钟）", P.QUEUE_MAX_ROUNDS == 3,
      P.QUEUE_MAX_ROUNDS)
_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("#71 日志用 min(attempt, budget)/budget 显示（不再出现 4/3）",
      "min(attempt + 1, _queue_budget())" in _src
      and "min(attempts + 1, _queue_budget())" in _src)

# ---------------------------------------------------------------- 跨区
_old_ready = P._realm_ready_total
P._realm_ready_total = lambda realm, model=None: 2 if realm == "intl" else 0
try:
    _alt = P._alt_realm_ready("cn", "DeepSeek-Flash")
    _alt2 = P._alt_realm_ready("intl", "DeepSeek-Flash")
finally:
    P._realm_ready_total = _old_ready
check("#71 cn 排队时能找到 intl 作为跨区目标", _alt == "intl", _alt)
check("#71 另一区无就绪账号时不跨区", _alt2 == "", _alt2)
check("#71 两条流式路径都有跨区 failover",
      _src.count("failing over to") == 2, _src.count("failing over to"))
check("#71 跨区时会尊重显式 X-Realm（不跨）",
      'self.headers.get("X-Realm")' in _src)

finish()
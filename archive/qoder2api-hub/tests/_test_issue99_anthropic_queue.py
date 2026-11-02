"""#99 Anthropic 入口接入排队可见分支（与 chat / responses 对齐）。"""
import io
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py"])

_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
_i = _src.index("def _handle_anthropic")
_blk = _src[_i:_i + 24000]

print("[#99] 源码守卫")
check("anthropic 入口有 UpstreamQueued 分支", "except UpstreamQueued as exc:" in _blk)
check("排队分支在 except UpstreamStatus 之前",
      _blk.index("except UpstreamQueued") < _blk.index("except UpstreamStatus"))
check("visible 流式走流内等待 visible_queue_acquire",
      "visible_queue_acquire(" in _blk)
check("排队事件用 anthropic flavor",
      '_queue_notice("anthropic"' in _blk and '_queue_error_frame(' in _blk)
check("fast / 非流式 503 带 Retry-After",
      "retry_after=exc.retry_after" in _blk)
check("兜底 UpstreamStatus 也透传 retry_after",
      'retry_after=getattr(exc, "retry_after", None)' in _blk)


def _handler(cap):
    """构造一个只实现被调用方法的 Handler 实例。"""
    h = object.__new__(P.Handler)
    h.headers = {}
    h.realm_used = ""
    h.realm_reason = ""
    h._request_realm = lambda: "cn"
    h._model_precheck = lambda m, r: None
    h._key_id_for_usage = lambda: ""
    h._error = lambda code, msg, etype, retry_after=None, **kw: (
        cap.update(kind="error", code=code, msg=msg, etype=etype,
                   retry_after=retry_after) or "ERR")
    h._sse_begin = lambda: cap.update(kind="sse", begun=True)
    h._sse_end = lambda: cap.update(ended=True)
    h._sse_write = lambda frame: cap.setdefault("frames", []).append(
        frame.decode("utf-8", "replace") if isinstance(frame, bytes) else str(frame))
    return h


_payload = {"model": "claude-4-sonnet", "max_tokens": 16,
            "messages": [{"role": "user", "content": "hi"}]}

print("[#99] 非流式：503 + Retry-After（不再零等待静默失败）")
_cap = {}
_h = _handler(_cap)
_real = P.open_upstream


def _boom(*a, **k):
    raise P.UpstreamQueued(30, '{"code":"10605","isQueued":true}', waited=3)


P.open_upstream = _boom

try:
    P.Handler._handle_anthropic(_h, "/v1/messages", dict(_payload))
finally:
    P.open_upstream = _real

check("返回 503", _cap.get("code") == 503, _cap.get("code"))
check("带 Retry-After=30", _cap.get("retry_after") == 30, _cap)
check("错误类型 upstream_queued", _cap.get("etype") == "upstream_queued", _cap.get("etype"))
check("文案说明排队（不是 Key/账号问题）",
      "排队" in (_cap.get("msg") or ""), (_cap.get("msg") or "")[:60])

print("[#99] fast 模式：即使流式也直接 503 + Retry-After")
# queue_mode() 读的是导入时固化的常量 QUEUE_MODE（不是每次读 env），
# 所以这里直接切常量，同时另外断言 env 变量名仍然被读取。
_old_mode = P.QUEUE_MODE
P.QUEUE_MODE = "fast"
check("启动时仍读 QD_QUEUE_WAIT_MODE",
      "QD_QUEUE_WAIT_MODE" in _src)
_cap2 = {}
_h2 = _handler(_cap2)
P.open_upstream = _boom
try:
    P.Handler._handle_anthropic(_h2, "/v1/messages",
                                dict(_payload, stream=True))
finally:
    P.open_upstream = _real
    P.QUEUE_MODE = _old_mode
check("fast：503 + Retry-After",
      _cap2.get("code") == 503 and _cap2.get("retry_after") == 30, _cap2)
check("fast：不建 SSE 流", _cap2.get("kind") != "sse", _cap2.get("kind"))

print("[#99] visible + 流式：流内等待，发排队帧，超预算给 event: error")
_cap3 = {}
_h3 = _handler(_cap3)
_seen = {}


def _fake_acquire(seed, reopen, emit, budget_secs=None, started=None):
    _seen["seed"] = (seed.status, seed.retry_after)
    emit("queue", {"waited_seconds": 7, "budget_seconds": 600, "message": "排队中"})
    emit("timeout", {"waited_seconds": 600, "budget_seconds": 600,
                     "message": "排队超时"})
    return None


_real_acq = P.visible_queue_acquire
P.visible_queue_acquire = _fake_acquire
P.open_upstream = _boom
try:
    P.Handler._handle_anthropic(_h3, "/v1/messages",
                                dict(_payload, stream=True))
finally:
    P.visible_queue_acquire = _real_acq
    P.open_upstream = _real

_frames = "".join(_cap3.get("frames") or [])
check("建了 SSE 流（流内等待）", _cap3.get("kind") == "sse", _cap3.get("kind"))
check("传给 acquire 的是 503 + retry_after", _seen.get("seed") == (503, 30), _seen.get("seed"))
check("发了 anthropic ping 排队帧",
      "event: ping" in _frames and "qoder_queue" in _frames, _frames[:120])
check("超预算发 event: error（overloaded_error）",
      "event: error" in _frames and "overloaded_error" in _frames)
check("超预算后关流、不返回 _error",
      _cap3.get("ended") is True and _cap3.get("kind") != "error", _cap3.get("kind"))

finish()
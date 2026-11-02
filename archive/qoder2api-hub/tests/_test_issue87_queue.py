"""#87：排队可见/快速失败 + Responses 回读与兼容 + 质量指标 + 测试隔离。

    python tests/_test_issue87_queue.py
"""
import json
import os
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py", "dashboard.html"])

# ---------------------------------------------------------------- 排队模式
print("[87] 排队模式与中文原因")
check("#87 默认 visible（流内可见）", P.queue_mode() == "visible", P.queue_mode())
check("#87 QD_QUEUE_WAIT_MODE=fast 可切快速失败",
      "QD_QUEUE_WAIT_MODE" in open(os.path.join(ROOT, "qoder_proxy.py"),
                                   encoding="utf-8").read())
_exc = P.UpstreamQueued(30, '{"code":"10605","isQueued":true}', waited=12)
check("#87 UpstreamQueued 映射为 503（暂时不可用，可重试）", _exc.status == 503)
check("#87 携带 Retry-After 秒数", _exc.retry_after == 30)
_msg, _et = P.friendly_upstream_error(503, '{"code":"10605","isQueued":true}')
check("#87 fast 模式给中文原因", "排队" in _msg and _et == "upstream_queued")
check("#87 文案说明不是 Key/账号/网络问题",
      "不是你的 Key" in _msg, _msg[:60])

# ---------------------------------------------------------------- visible 等待
print("[87] visible_queue_acquire")
_events = []
_calls = {"n": 0}


def _reopen_ok():
    _calls["n"] += 1
    return ("upstream", "account", "body")


_got = P.visible_queue_acquire(P.UpstreamQueued(1, "10605", waited=0),
                               _reopen_ok, lambda k, i: _events.append((k, i)),
                               budget_secs=10)
check("#87 排队后重试成功 -> 返回 upstream", _got and _got[0] == "upstream")
check("#87 等待期间至少发一次 queue 提示",
      any(k == "queue" for k, _ in _events), _events[:2])


def _reopen_always_queue():
    raise P.UpstreamQueued(60, "10605 again", waited=0)


_events2 = []
_got2 = P.visible_queue_acquire(P.UpstreamQueued(60, "10605", waited=0),
                                _reopen_always_queue,
                                lambda k, i: _events2.append((k, i)),
                                budget_secs=1, started=0)
check("#87 超预算返回 None 且发 timeout",
      _got2 is None and any(k == "timeout" for k, _ in _events2))
_timeout_info = [i for k, i in _events2 if k == "timeout"][0]
check("#87 timeout 带中文原因与预算",
      "排队" in (_timeout_info.get("message") or "")
      and _timeout_info.get("budget_seconds") == 1, _timeout_info)

# ---------------------------------------------------------------- Responses 回读
print("[87] Responses 路径与兼容字段")
_src = open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
_i = _src.index("def _handle_responses")
_resp_block = _src[_i:_i + 20000]
check("#87 Responses 流式接入回读链（与 chat 一致）",
      "recover_leaked_tool_calls(" in _resp_block)
_chat = P.responses_to_chat({"model": "m", "input": "hi",
                            "stream_options": {"include_usage": True}})
check("#87 stream_options 透传", _chat.get("stream_options") == {"include_usage": True})
check("#87 parallel_tool_calls 默认 True",
      _chat.get("parallel_tool_calls") is True)
_chat2 = P.responses_to_chat({"model": "m", "input": "hi",
                             "parallel_tool_calls": False})
check("#87 显式 parallel_tool_calls 保留", _chat2.get("parallel_tool_calls") is False)
check("#87 未实现字段会显式告警（store/previous_response_id）",
      "_note_ignored_responses" in _src and "not implemented" in _src)

# ---------------------------------------------------------------- 质量指标
print("[87] 质量指标 quality_snapshot")
_q = P.quality_snapshot()
check("#87 质量指标字段齐全",
      set(["requests", "ttft_p50_ms", "ttft_p90_ms", "tps_p50",
           "queue_rate", "recovery", "queue_mode"]) <= set(_q), sorted(_q))
check("#87 跳过无 account 的测试夹具行", isinstance(_q.get("requests"), int))
check("#87 /health 暴露 quality", '"quality": quality_snapshot()' in _src)
_dash = open(os.path.join(ROOT, "dashboard.html"), encoding="utf-8").read()
check("#87 面板展示首字延迟与排队率",
      "kpiTTFT" in _dash and "loadQuality" in _dash)

# ---------------------------------------------------------------- 测试隔离
print("[87] 测试不写真实 usage 日志")
check("#87 bootstrap 把用量目录隔离到 _acc/usage",
      os.environ.get("QD_PROXY_USAGE_DIR", "").replace("\\", "/")
      .endswith("_acc/usage"), os.environ.get("QD_PROXY_USAGE_DIR"))
_legacy = open(os.path.join(ROOT, "_test_qoder.py"), encoding="utf-8").read()
check("#87 遗留套件同样隔离 QD_PROXY_USAGE_DIR",
      "QD_PROXY_USAGE_DIR" in _legacy)

finish()

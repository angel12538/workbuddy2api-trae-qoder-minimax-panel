"""#100 排队耗尽后 RateLimited.status AttributeError + #101 max_tokens 上限学习/clamp。"""
import io
import json
import os
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py"])

_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()

print("[#100] 异常 -> HTTP 状态（RateLimited 没有 .status）")
check("RateLimited 映射为 429",
      P._exc_http_status(P.RateLimited(None, "q", 30)) == 429)
check("RateLimited 确实没有 status 属性（根因）",
      not hasattr(P.RateLimited(None, "q", 30), "status"))
check("UpstreamQueued 仍取 .status=503",
      P._exc_http_status(P.UpstreamQueued(5, "10605")) == 503)
check("带 .code 的异常走 code",
      P._exc_http_status(type("E", (Exception,), {"code": 400})()) == 400)
check("什么都取不到时回落默认值",
      P._exc_http_status(RuntimeError("x")) == 502
      and P._exc_http_status(RuntimeError("x"), 429) == 429)

_i = _src.index("def _handle_responses")
_resp = _src[_i:_i + 40000]
check("responses 收尾不再裸读 pump_exc.status",
      "pump_exc.status" not in _resp and "_exc_http_status(pump_exc" in _resp)
_j = _src.index("def _handle_chat(") if "def _handle_chat(" in _src else 0
_chat = _src[_j:_j + 60000] if _j else ""
check("chat 收尾同样走 _exc_http_status",
      "_exc_http_status(exc, 429)" in _src)
check("RateLimited 分支仍会发终态事件（不静默断流）",
      "_responses_failed_frame" in _resp and "self._sse_end()" in _resp)

print("[#101] 从上游 400 学输出上限")
_tmp_usage = tempfile.mkdtemp(prefix="maxout-")
_orig_usage = P.USAGE_DIR
P.USAGE_DIR = _tmp_usage
P._MAX_OUT_LIMITS.clear()

_detail = ("data: {\"error\":{\"code\":\"invalid_parameter_error\",\"message\":"
           "\"Range of max_tokens should be [1, 131072]\"}}")
check("从错误文本解析出上限",
      P.learn_max_output_limit(_detail, "DeepSeek-Flash", "cn") == 131072)
check("非该格式的错误不影响",
      P.learn_max_output_limit("some other error", "x", "cn") is None)
check("下一次可查到", P.max_output_limit("DeepSeek-Flash", "cn") == 131072)
check("落盘持久化", os.path.exists(os.path.join(_tmp_usage, "max-output-limits.json")))
P._MAX_OUT_LIMITS.clear()
check("重载后仍在（跨进程可用）",
      P.max_output_limit("DeepSeek-Flash", "cn") == 131072)

print("[#101] clamp 生效")
check("超大值被压到上限", P.clamp_max_tokens(200000, "DeepSeek-Flash", "cn") == (131072, True))
check("等于上限不压", P.clamp_max_tokens(131072, "DeepSeek-Flash", "cn") == (131072, False))
check("未学过的模型不压", P.clamp_max_tokens(200000, "unknown-model", "cn") == (200000, False))
check("非法值原样返回", P.clamp_max_tokens("abc", "DeepSeek-Flash", "cn") == ("abc", False))

_body = P.build_qoder_body({"messages": [{"role": "user", "content": "hi"}],
                            "max_tokens": 200000},
                           None, "dfmodel", "cn")
check("出站请求体已 clamp",
      (_body.get("parameters") or {}).get("max_tokens") == 131072,
      (_body.get("parameters") or {}).get("max_tokens"))
_body2 = P.build_qoder_body({"messages": [{"role": "user", "content": "hi"}],
                             "max_tokens": 256},
                            None, "dfmodel", "cn")
check("小值原样透传",
      (_body2.get("parameters") or {}).get("max_tokens") == 256)

print("[#101] 其它接点")
check("record_error 里会学上限",
      "learn_max_output_limit(message, model)" in _src)
check("/v1/models 会补 max_output_tokens",
      "max_output_limit(mid, realm)" in _src)
check("上限缓存有 7 天 TTL",
      "MAX_OUT_LIMIT_TTL = 7 * 86400" in _src)

P._MAX_OUT_LIMITS.clear()
P.USAGE_DIR = _orig_usage
finish()
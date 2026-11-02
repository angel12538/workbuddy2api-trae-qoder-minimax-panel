"""#72 残缺工具回声：声明了 tools 就 fail-closed，不再静默 stop。"""
import io
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py"])


def _frame(delta, finish=None):
    return ("data: " + json.dumps({"id": "c", "choices": [{"index": 0,
                                   "delta": delta, "finish_reason": finish}]},
                                  ensure_ascii=False) + "\n\n").encode()


def _frames(text, n=8):
    out = [_frame({"content": text[i:i + n]}) for i in range(0, len(text), n)]
    out.append(_frame({}, "stop"))
    return out


# ---------------------------------------------------------------- 判定
check("#72 marker-only 判定：只有标记 -> True",
      P._marker_only_echo(P.LEAK_MARKER) is True)
check("#72 多个标记也算 marker-only",
      P._marker_only_echo(P.LEAK_MARKER + " " + P.LEAK_MARKER) is True)
check("#72 标记+数组 -> False",
      P._marker_only_echo(P.LEAK_MARKER + ' [{"name":"x"}]') is False)
check("#72 空串不算 marker-only", P._marker_only_echo("") is False)

# ---------------------------------------------------------------- 流式
_raised = None
try:
    _out = list(P.recover_leaked_tool_calls(
        iter(_frames(P.LEAK_MARKER)), {"shell"}))
except Exception as exc:
    _raised = exc
check("#72 流式：声明 tools + 只回 marker -> 抛 EmptyToolEcho",
      isinstance(_raised, P.EmptyToolEcho), type(_raised).__name__)
check("#72 EmptyToolEcho 继承 InvalidToolArguments（四条出口自动复用）",
      isinstance(_raised, P.InvalidToolArguments))
check("#72 错误文案说明是残缺回声且应重试",
      "truncated tool-call echo" in (_raised.detail if _raised else ""),
      getattr(_raised, "detail", "")[:60])

_raised2 = None
_chunks = []
try:
    for _f in P.recover_leaked_tool_calls(iter(_frames(P.LEAK_MARKER)), None):
        _chunks.append(_f)
except Exception as exc:
    _raised2 = exc
_text = b"".join(_chunks).decode("utf-8", "replace")
check("#72 未声明 tools 时仍按原样吞掉（不抛错）",
      _raised2 is None, type(_raised2).__name__ if _raised2 else None)
check("#72 吞掉后 marker 不会透传给用户",
      "请求调用工具" not in _text, _text[:80])

# ---------------------------------------------------------------- 非流式 + 出口
# 截断数组（marker + 半截 JSON）维持 issue #9 的「丢弃 + 正常结束」语义，
# 只有**纯 marker 退化**才 fail-closed（#72 报告的就是这一种）。
_TRUNC = P.LEAK_MARKER + ' [{"name": "shell", "arguments": "{\\"cmd\\": \\"ls'
_raised3 = None
try:
    list(P.recover_leaked_tool_calls(iter(_frames(_TRUNC)), {"shell"}))
except Exception as exc:
    _raised3 = exc
check("#72 截断数组仍按 #9 丢弃（不 fail-closed）", _raised3 is None,
      type(_raised3).__name__ if _raised3 else None)

_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("#72 非流式 chat aggregate 同样 fail-closed",
      _src.count("failing the turn closed (issue #72)") >= 3)
check("#72 Responses 出口也覆盖",
      "response.failed" in _src and "EmptyToolEcho" in _src)
check("#72 日志带上 kind / tools_declared（便于区分退化形态）",
      "marker_only" in _src and "tools_declared=%d" in _src)
check("#72 重试/冷却入口仍排除（继承父类即可）",
      "isinstance(exc, InvalidToolArguments)" in _src)

finish()
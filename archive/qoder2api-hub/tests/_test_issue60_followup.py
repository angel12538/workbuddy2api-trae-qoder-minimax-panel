"""#60 后续：dfmodel 白名单 + marker 括号变体 + 散文同帧。

    python tests/_test_issue60_followup.py
"""
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py"])

NAME = "Write"
INNER = json.dumps({"file_path": "F:\\隔离聊天\\log.md",
                    "content": "# 标题\n\n正文"}, ensure_ascii=False)


def body(marker, escaped=True):
    # 真实客户端把 arguments 当 JSON **字符串**发：引号与反斜杠都要再转义一层。
    args = (INNER.replace('\\', '\\\\').replace('"', '\\"')
            if escaped else INNER)
    return marker + ' [{"name": "%s", "arguments": "%s"}]' % (NAME, args)


def stream(text, n, names=None):
    names = {NAME} if names is None else names
    frames = []
    for i in range(0, len(text), n):
        frames.append(("data: " + json.dumps(
            {"id": "c", "choices": [{"index": 0,
                                     "delta": {"content": text[i:i + n]},
                                     "finish_reason": None}]},
            ensure_ascii=False) + "\n\n").encode())
    frames.append(("data: " + json.dumps(
        {"id": "c", "choices": [{"index": 0, "delta": {},
                                 "finish_reason": "stop"}]},
        ensure_ascii=False) + "\n\n").encode())
    calls, txt, fins = [], [], []
    for f in P.recover_leaked_tool_calls(iter(frames), names):
        d = P.strip_data_prefix(f.decode("utf-8", "replace"))
        if not d or d == "[DONE]":
            continue
        o = json.loads(d)
        ch = (o.get("choices") or [{}])[0]
        dd = ch.get("delta") or {}
        calls.extend(dd.get("tool_calls") or [])
        txt.append(dd.get("content") or "")
        if ch.get("finish_reason"):
            fins.append(ch["finish_reason"])
    return calls, "".join(txt), fins


# ---------------------------------------------------------------- 白名单
print("[60f] 结构化工具历史白名单")
_msgs = [
    {"role": "assistant", "content": "",
     "tool_calls": [{"id": "t1", "type": "function",
                     "function": {"name": NAME, "arguments": "{}"}}]},
    {"role": "tool", "tool_call_id": "t1", "content": "ok"},
]
check("#60f 上游 key `dfmodel` 命中结构化直传（此前只写 deepseek 匹配不到）",
      P.structured_tool_history_enabled(model="DeepSeek-V4-Flash",
                                        model_key="dfmodel",
                                        realm="cn", messages=_msgs) is True)
check("#60f deepseek-* 仍命中", P.structured_tool_history_enabled(
    model="deepseek-v4-flash", model_key="deepseek-v4-flash",
    realm="cn", messages=_msgs) is True)
check("#60f 未验证的 provider 仍排除（kimi）",
      P.structured_tool_history_enabled(model="kimi-k2", model_key="kimi-k2",
                                        realm="cn", messages=_msgs) is False)

# ---------------------------------------------------------------- marker 变体
print("[60f] marker 括号变体")
for _mk in ("[assistant 请求调用工具]", "<assistant 请求调用工具>",
            "[<assistant 请求调用工具]", "[<assistant 请求调用工具",
            "[<assistant  请求调用工具]"):
    _c, _ = P.parse_leaked_tool_calls(body(_mk), {NAME})
    check("#60f 解析：%s" % _mk, bool(_c) and _c[0]["function"]["name"] == NAME)

# ---------------------------------------------------------------- 报告者形态
print("[60f] 报告者原文形态（散文 + [< marker + 未转义引号）")
_rep = ("歪比巴布，先回答你。" + body("[<assistant 请求调用工具]", escaped=False))
_c, _ = P.parse_leaked_tool_calls(_rep, {NAME})
check("#60f 直接解析可恢复", bool(_c), _rep[:40])
for _n in (1, 8, 64, 9999):
    _calls, _txt, _fins = stream(_rep, _n)
    check("#60f 流式 n=%d：恢复 1 调用、零泄漏、finish=tool_calls" % _n,
          len(_calls) == 1 and "请求调用工具" not in _txt
          and _fins == ["tool_calls"], (len(_calls), _fins))

# ---------------------------------------------------------------- 反例
print("[60f] 反例仍 fail-open / fail-closed")
_prose = "这段在讨论 [assistant 请求调用工具] 这个格式，不是真的调用。"
check("#60f 纯散文不被误判", P.parse_leaked_tool_calls(_prose, {NAME})[0] is None)
_calls, _txt, _fins = stream(_prose, 8)
check("#60f 纯散文流式逐字守恒", _calls == [] and _prose in _txt, _txt[:40])
_undecl = '[assistant 请求调用工具] [{"name":"Evil","arguments":"{}"}]'
check("#60f 未声明工具名不恢复",
      P.parse_leaked_tool_calls(_undecl, {NAME})[0] is None)

_parts = ["[assistant 请求调用工具", "]\n这不是数组，是中文说明"]
_calls, _txt, _fins = stream("".join(_parts), len(_parts[0]) + 1)
check("#60f 双括号形态逐字节守恒", _txt == "".join(_parts), (_txt, "".join(_parts)))

finish()

"""#55 Anthropic 入口：messages 内 system 不得污染缓存前缀。

    python tests/_test_issue55_system_prefix.py
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_anthropic.py", "qoder_proxy.py"])
import qoder_anthropic as AN       # noqa: E402

TOP = "你是助手。"
DYN1 = "动态补充：当前时间 20:17"
DYN2 = "动态补充：当前时间 20:18"


def payload(dyn=DYN1, top=TOP):
    return {"model": "DeepSeek-Flash", "max_tokens": 1024,
            "system": top,
            "messages": [{"role": "system", "content": dyn},
                         {"role": "user", "content": "hi"}]}


def _mode(value):
    if value is None:
        os.environ.pop("QD_ANTHROPIC_MSG_SYSTEM", None)
    else:
        os.environ["QD_ANTHROPIC_MSG_SYSTEM"] = value


# ---- 默认（inline）：system 只取顶层，动态内容就地降级为 user
try:
    _mode(None)
    check("#55 默认模式 = inline", AN.msg_system_mode() == "inline")
    check("#55 默认：顶层 system 不含 messages 内动态内容",
          AN.system_prompt_of(payload()) == TOP,
          AN.system_prompt_of(payload()))
    _chat = AN.messages_to_chat(payload())
    _roles = [m["role"] for m in _chat["messages"]]
    _texts = [m.get("content") for m in _chat["messages"]]
    check("#55 默认：动态 system 降级成 user 且内容保留",
          DYN1 in _texts and "user" in _roles, _texts)
    check("#55 默认：位置不变（动态内容在最后一条 user 之前）",
          _roles.index("system") == 0
          and _texts.index(DYN1) < _texts.index("hi"), _roles)
    check("#55 默认：缓存前缀稳定（只有动态内容不同 -> 顶层 system 字节相同）",
          AN.system_prompt_of(payload(DYN1)) == AN.system_prompt_of(payload(DYN2)))
finally:
    _mode(None)

# ---- merge：旧行为（会污染前缀）
try:
    _mode("merge")
    check("#55 merge：动态内容被合并进顶层 system",
          DYN1 in AN.system_prompt_of(payload()))
    _chat2 = AN.messages_to_chat(payload())
    check("#55 merge：messages 内 system 不再重复出现",
          DYN1 not in [m.get("content") for m in _chat2["messages"]])
finally:
    _mode(None)

# ---- drop
try:
    _mode("drop")
    _chat3 = AN.messages_to_chat(payload())
    check("#55 drop：动态 system 被丢弃（内容不出现）",
          DYN1 not in [m.get("content") for m in _chat3["messages"]])
finally:
    _mode(None)

# ---- 指纹：sys_src / n_msg_system
try:
    _mode(None)
    _src = P._anthropic_sys_src(payload())
    check("#55 指纹来源标记 = top+inline", _src == "top+inline", _src)
    _fp = P.prompt_fingerprint([{"role": "user", "content": "hi"}],
                               sys_src=_src,
                               n_msg_system=AN.count_msg_system(payload()))
    check("#55 指纹带 sys_src 与 n_msg_system",
          _fp.get("sys_src") == "top+inline" and _fp.get("n_msg_system") == 1,
          _fp)
finally:
    _mode(None)
try:
    _mode("merge")
    check("#55 merge 时来源标记 = top+msg",
          P._anthropic_sys_src(payload()) == "top+msg")
finally:
    _mode(None)
check("#55 无 system 时来源标记 = none",
      P._anthropic_sys_src({"messages": []}) == "none")
check("#55 只有顶层 system 时 = top",
      P._anthropic_sys_src({"system": "s", "messages": []}) == "top")

# ---- #49 后续：DeepSeek 进结构化白名单
_MSGS = [
    {"role": "user", "content": "go"},
    {"role": "assistant", "content": "", "tool_calls": [
        {"id": "call_1", "type": "function",
         "function": {"name": "Bash", "arguments": "{}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "ok"},
]
check("#55/#49 deepseek 在白名单里", "deepseek" in P.STRUCTURED_TOOL_HISTORY_WHITELIST)
check("#55/#49 DeepSeek-CN + id 齐备 -> 结构化直传",
      P.structured_tool_history_enabled(model="DeepSeek-Flash", realm="cn",
                                        messages=_MSGS) is True)
check("#55/#49 INTL 仍不自动启用（未经端到端验证）",
      P.structured_tool_history_enabled(model="DeepSeek-Flash", realm="intl",
                                        messages=_MSGS) is False)
check("#55/#49 id 不齐备时回退文本化",
      P.structured_tool_history_enabled(
          model="DeepSeek-Flash", realm="cn",
          messages=[{"role": "tool", "content": "x"}]) is False)

finish()

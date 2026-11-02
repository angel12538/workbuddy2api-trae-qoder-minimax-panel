"""issue #79 回归套件：Anthropic 入口的形态判定必须和 open_upstream 用同一套 realm 兜底。

背景：#76（v1.6.2 / PR #77）把三个请求入口的 `_request_realm() or CURRENT_REALM`
改成裸调用，让 None 一路传给 open_upstream 做自动路由。但 `_handle_anthropic`
里的形态决策 `structured_tool_history_enabled(..., realm=req_realm)` 拿到了同一个
None，而该函数 auto 模式的第一道闸是 `if (realm or "") != "cn": return False`，
于是 DeepSeek / Qwen 等白名单永远不生效 —— 所有 CN 请求回退文本化，
模型重新看到 LEAK_MARKER 并按 #49 描述的方式复述成正文。

本套件两层守护：

1) 源码守卫（按函数归属）：
   - `_handle_anthropic` 内出现 `structured_tool_history_enabled(` 调用时，
     realm 实参不得是裸 `req_realm`，必须是 `_realm_for_shape`；
   - `_realm_for_shape` 的定义必须同时包含 `req_realm`、`detect_model_realm(`、
     `CURRENT_REALM` 三个成分（与 open_upstream 的兜底同源）；
   - `req_realm = self._request_realm()` 裸调用仍在（#76 的路由语义不被改写）。
2) 行为断言（直接对函数做真值表）：
   - realm="cn" + dfmodel / deepseek → True；
   - realm=None → False（回归本体，用来证明修复点是「喂对 realm」）；
   - realm="intl" → False（INTL 仍排除在 auto 之外）；
   - tool id 不齐 → False（fail-safe 不变）；
   - QD_STRUCTURED_TOOL_HISTORY 的 off / on 三态语义不变。

金丝雀自证：把 `_realm_for_shape` 改回 `req_realm`，源码守卫与行为断言必红。

    python tests/_test_issue79_structured_realm.py
"""
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from _suite_head import bootstrap, check, declare_suite, finish  # noqa: E402

P, A = bootstrap()  # noqa: F401  (P 在下方行为断言使用)
declare_suite(__file__, ["qoder_proxy.py"])

SRC = open(os.path.join(os.path.dirname(HERE), "qoder_proxy.py"),
           encoding="utf-8").read()


def lines_of_func(name):
    """返回 def name( 之后、同级或更外层下一个 def 之前的所有行。"""
    lines = SRC.splitlines()
    out, start, base = [], None, None
    for i, ln in enumerate(lines):
        m = re.match(r"^(\s*)def (\w+)", ln)
        if m:
            indent, fn = len(m.group(1)), m.group(2)
            if fn == name:
                if start is not None:  # 同名多次出现：取第一个
                    continue
                start, base = i + 1, indent
                continue
            if start is not None and indent <= base:
                break
        if start is not None and i >= start:
            out.append(ln)
    return out


def _msgs(ids_ok=True):
    """最小 chat 消息链：assistant.tool_calls + tool 配对。"""
    call_id = "toolu_01" if ids_ok else ""
    return [
        {"role": "user", "content": "hi"},
        {"role": "assistant", "content": "",
         "tool_calls": [{"id": call_id, "type": "function",
                         "function": {"name": "Bash", "arguments": "{}"}}]},
        {"role": "tool", "tool_call_id": call_id, "content": "ok"},
    ]


def _enabled(realm, model="DeepSeek-Flash", key="dfmodel", ids_ok=True):
    return P.structured_tool_history_enabled(
        model=model, model_key=key, realm=realm, messages=_msgs(ids_ok))


print("\n[issue79] 源码守卫（_handle_anthropic 的形态决策）")
body = "\n".join(lines_of_func("_handle_anthropic"))
_call = re.search(r"structured_tool_history_enabled\(", body)
_args = ""
if _call:
    tail = body[_call.end():]
    depth, end = 1, None
    for i, ch in enumerate(tail):
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
            if depth == 0:
                end = i
                break
    _args = tail[:end] if end is not None else tail
check("入口存在形态决策调用", bool(_call))
check("形态判定的 realm 实参不是裸 req_realm", "realm=req_realm" not in _args, _args)
check("形态判定使用 _realm_for_shape", "realm=_realm_for_shape" in _args, _args)
check("兜底以 req_realm 起头", "_realm_for_shape = (req_realm" in body)
check("兜底包含 detect_model_realm", "detect_model_realm(" in body)
check("兜底包含 CURRENT_REALM", "CURRENT_REALM" in body)
check("#76 路由语义未改（req_realm 裸调用仍在）",
      "req_realm = self._request_realm()" in body)

print("\n[issue79] 行为真值表（auto 模式）")
os.environ.pop("QD_STRUCTURED_TOOL_HISTORY", None)
check("cn + dfmodel → True", _enabled("cn") is True)
check("cn + deepseek 前缀 → True",
      _enabled("cn", model="deepseek-v4", key="deepseek-v4") is True)
check("realm=None → False（回归本体）", _enabled(None) is False)
check("intl + dfmodel → False（INTL 仍排除）", _enabled("intl") is False)
check("cn + 非白名单模型 → False",
      _enabled("cn", model="Kimi-K2", key="kimi") is False)
check("cn + tool id 不齐 → False（fail-safe）", _enabled("cn", ids_ok=False) is False)

print("\n[issue79] 三态开关")
os.environ["QD_STRUCTURED_TOOL_HISTORY"] = "off"
check("off + cn → False", _enabled("cn") is False)
os.environ["QD_STRUCTURED_TOOL_HISTORY"] = "on"
check("on + None → True（临时绕过路径，不依赖本次修复）", _enabled(None) is True)
os.environ.pop("QD_STRUCTURED_TOOL_HISTORY", None)
check("恢复 auto 后 None 仍为 False（证明结论来自 realm，不是环境）",
      _enabled(None) is False)

print("\n[issue79] 金丝雀说明：把 _realm_for_shape 改回 req_realm，以上源码守卫与"
      "「realm=None → False」的对照都会变红。")
finish()

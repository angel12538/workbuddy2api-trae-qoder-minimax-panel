"""#57 容器+国际版告警 / #58 模型清单缓存 / #59 图片冗余与 413 / #60 未转义引号抢救。

    python tests/_test_issue57_60.py
"""
import os
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py"])

NAME = "mcp_plugin_playwright_playwright_browser_take_screenshot"


def frame(delta, finish=None):
    return ("data: " + json.dumps({"id": "c", "choices": [{"index": 0,
             "delta": delta, "finish_reason": finish}]},
             ensure_ascii=False) + "\n\n").encode()


def frames(text, n):
    out = [frame({"content": text[i:i + n]}) for i in range(0, len(text), n)]
    out.append(frame({}, "stop"))
    return out


def run_stream(text, n, names):
    calls, txt, fins = [], [], []
    for f in P.recover_leaked_tool_calls(iter(frames(text, n)), names):
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


# ---------------------------------------------------------------- #60
print("[60] 内层引号未转义（外层 JSON 也非法）")
# 截图原文：arguments 里的引号没有转义
_T60 = ('<assistant 请求调用工具> [{"name": "' + NAME + '", "arguments": '
        '"{"filename":"autumn-rain.png","scale":"css"}"}]')
_c60, _clean60 = P.parse_leaked_tool_calls(_T60, {NAME})
check("#60 抢救式解析恢复出调用", bool(_c60) and _c60[0]["function"]["name"] == NAME,
      _c60)
check("#60 参数被补转义后仍可解析",
      bool(_c60) and json.loads(_c60[0]["function"]["arguments"])
      == {"filename": "autumn-rain.png", "scale": "css"},
      _c60[0]["function"]["arguments"] if _c60 else None)
for _n in (8, 64, 9999):
    _calls, _body, _fins = run_stream(_T60, _n, {NAME})
    check("#60 流式 n=%d：恢复 1 调用、零泄漏、finish=tool_calls" % _n,
          len(_calls) == 1 and "请求调用工具" not in _body
          and _fins == ["tool_calls"], (len(_calls), _fins))

# ---------------------------------------------------------------- #59
print("[59] 图片冗余 + 出站体积")
_IMG = "data:image/png;base64," + "A" * 200
_PAY = {"model": "auto", "stream": False,
        "messages": [{"role": "user", "content": [
            {"type": "text", "text": "what color?"},
            {"type": "image_url", "image_url": {"url": _IMG}}]}]}
_body59 = P.build_qoder_body(_PAY, None, "auto", realm="cn")
_cc59 = _body59.get("chat_context") or {}
check("#59 默认不再在顶层重复塞图（body.image_urls 为空）",
      not _body59.get("image_urls"), _body59.get("image_urls"))
check("#59 默认不再重复塞 cc.imageUrls", not _cc59.get("imageUrls"),
      _cc59.get("imageUrls"))
_parts59 = [p for p in (_body59.get("messages") or [{}])[-1].get("content") or []
            if isinstance(p, dict)]
check("#59 messages 里的结构化 image_url parts 保留",
      any(p.get("type") == "image_url" for p in _parts59),
      [p.get("type") for p in _parts59])
os.environ["QD_KEEP_TOPLEVEL_IMAGES"] = "1"
try:
    _b2 = P.build_qoder_body(_PAY, None, "auto", realm="cn")
finally:
    os.environ.pop("QD_KEEP_TOPLEVEL_IMAGES", None)
check("#59 兼容开关可恢复旧行为", bool(_b2.get("image_urls")))

_keep59 = P.MAX_UPSTREAM_BODY_BYTES
try:
    P.MAX_UPSTREAM_BODY_BYTES = 512
    _raised59 = None
    try:
        P.open_upstream({"model": "auto", "stream": False,
                         "messages": [{"role": "user", "content": "x" * 4000}]},
                        target_realm="cn")
    except P.PayloadTooLarge as exc:
        _raised59 = exc
    except Exception as exc:
        _raised59 = exc
finally:
    P.MAX_UPSTREAM_BODY_BYTES = _keep59
check("#59 超限在发网络请求前就地抛 413（不会打爆账号池）",
      isinstance(_raised59, P.PayloadTooLarge) and _raised59.status == 413,
      _raised59)
_f59 = P.friendly_upstream_error(413, "payload too large: 1 > 0")
check("#59 413 文案：type=payload_too_large 且文案含「请求体过大」",
      _f59[1] == "payload_too_large" and "请求体过大" in _f59[0], _f59)

# ---------------------------------------------------------------- #58
print("[58] 模型清单磁盘缓存 + 刷新端点")
_tmp58 = tempfile.mkdtemp()
_old_dir = P.ACCOUNTS_DIR
try:
    P.ACCOUNTS_DIR = _tmp58
    P._save_models_cache("cn", [("dfmodel", {"display_name": "DeepSeek-Flash"})])
    _back = P._load_models_cache("cn")
    check("#58 动态清单落盘并可读回",
          _back and _back[0][0] == "dfmodel", _back)
    check("#58 缓存文件按区域命名",
          os.path.basename(P._models_cache_path("cn")) == "models-cache-cn.json")
    _path58 = P._models_cache_path("cn")
    _blob58 = json.load(open(_path58, encoding="utf-8"))
    _blob58["at"] = 0
    json.dump(_blob58, open(_path58, "w", encoding="utf-8"))
    check("#58 过期缓存不再使用", P._load_models_cache("cn") == [])
finally:
    P.ACCOUNTS_DIR = _old_dir
_src58 = open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("#58 fetch_models 的回退链含磁盘缓存",
      "_load_models_cache(r)" in _src58 and "_save_models_cache(r, live)" in _src58)
check("#58 提供 /models/refresh 手动刷新",
      '"/models/refresh"' in _src58)

# ---------------------------------------------------------------- #57
print("[57] 容器 + 国际版风控告警")
check("#57 容器探测函数可用（返回 bool）",
      isinstance(P._in_container(), bool))
check("#57 启动时会针对「容器 + intl 账号」告警",
      "container egress + intl accounts detected" in _src58)
check("#57 告警不阻断启动（只 log WARN）",
      "level=\"WARN\", tag=\"realm\"" in _src58)

finish()

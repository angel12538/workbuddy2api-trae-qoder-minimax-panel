"""本地 web 工具：SSRF 守卫 / 声明改写 / 开关 / 工具执行。"""
import io
import json
import os
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_webtools.py", "qoder_proxy.py",
                         "qoder_settings.py", "dashboard.html"])
import qoder_webtools as W         # noqa: E402
import qoder_settings as S         # noqa: E402

# ---------------------------------------------------------------- SSRF
print("[web] SSRF 守卫")
for _url in ("http://127.0.0.1:8080/", "http://10.0.0.5/",
             "http://192.168.1.1/", "http://169.254.169.254/latest",
             "http://localhost/", "file:///etc/passwd",
             "http://user:pass@example.com/"):
    ok, reason = W.check_url(_url)
    check("拒绝 %s" % _url, ok is False, reason)
_old_proxy = os.environ.get("HTTPS_PROXY")
os.environ["HTTPS_PROXY"] = "http://127.0.0.1:7897"
try:
    ok, reason = W.check_url("https://example.com/path")
finally:
    if _old_proxy is None:
        os.environ.pop("HTTPS_PROXY", None)
    else:
        os.environ["HTTPS_PROXY"] = _old_proxy
check("走代理时公网域名放行（DNS 交给代理）", ok is True, reason)

# ---------------------------------------------------------------- 声明改写
print("[web] 内置工具声明改写")
_tools = [
    {"type": "web_search"},
    {"type": "web_search_preview"},
    {"type": "web_fetch"},
    {"type": "function", "function": {"name": "shell", "parameters": {}}},
]
_names = P.builtin_web_tool_names(_tools)
check("识别 web_search / web_search_preview / web_fetch",
      _names == {"web_search", "web_fetch"}, _names)
_rw = P.rewrite_web_tools(_tools, _names)
check("内置声明被替换，普通 function 保留",
      len(_rw) == 3 and all(t.get("type") == "function" for t in _rw),
      [t.get("type") for t in _rw])
_names_rw = {t["function"]["name"] for t in _rw}
check("替换后的两个 function 名字正确（且原 shell 仍在）",
      {"web_search", "web_fetch", "shell"} <= _names_rw, _names_rw)
check("替换后带参数 schema（query / url）",
      any("query" in (t["function"].get("parameters", {}).get("properties", {}))
          for t in _rw)
      and any("url" in (t["function"].get("parameters", {}).get("properties", {}))
              for t in _rw))

# ---------------------------------------------------------------- 开关
print("[web] 开关（env 优先 / 面板持久化）")
_dir = tempfile.mkdtemp(prefix="wt-")
_old_env = os.environ.get("QD_WEB_TOOLS")
os.environ.pop("QD_WEB_TOOLS", None)
try:
    check("默认关闭", S.web_tools_enabled(_dir) is False)
    S.set_web_tools_enabled(_dir, True)
    check("面板开启后生效", S.web_tools_enabled(_dir) is True)
    os.environ["QD_WEB_TOOLS"] = "off"
    check("QD_WEB_TOOLS=off 覆盖面板开关", P.web_tools_enabled() is False)
    os.environ["QD_WEB_TOOLS"] = "on"
    check("QD_WEB_TOOLS=on 强制开启", P.web_tools_enabled() is True)
finally:
    os.environ.pop("QD_WEB_TOOLS", None)
    if _old_env is not None:
        os.environ["QD_WEB_TOOLS"] = _old_env

# ---------------------------------------------------------------- 执行
print("[web] 工具执行")
_real_search = W.search
_real_fetch = W.fetch
W.search = lambda q, limit=5: {"ok": True, "engine": "stub",
                               "results": [{"title": "T", "url": "https://e.com",
                                           "snippet": "S"}]}
W.fetch = lambda u: {"ok": True, "url": u, "title": "Page", "text": "BODY"}
try:
    _r1 = W.run_tool("web_search", json.dumps({"query": "hi"}))
    _r2 = W.run_tool("web_fetch", json.dumps({"url": "https://e.com"}))
    _r3 = W.run_tool("nope", "{}")
finally:
    W.search = _real_search
    W.fetch = _real_fetch
check("web_search 结果文本包含标题/链接",
      "T" in _r1 and "https://e.com" in _r1, _r1[:60])
check("web_fetch 结果文本包含正文", "BODY" in _r2, _r2[:60])
check("未知工具返回错误 JSON", "unknown tool" in _r3, _r3[:60])

# ---------------------------------------------------------------- 接线
_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("有 _handle_web_tools 处理器", "def _handle_web_tools(" in _src)
check("请求入口按开关与声明分流",
      "builtin_web_tool_names(payload.get(" in _src
      and "return self._handle_web_tools(" in _src)
check("轮次上限可配（QD_WEB_TOOLS_ROUNDS）",
      "QD_WEB_TOOLS_ROUNDS" in _src)
check("开关端点存在", 'if path == "/settings/web-tools":' in _src)
_dash = io.open(os.path.join(ROOT, "dashboard.html"), encoding="utf-8").read()
check("面板有开关与说明", "setWebTools" in _dash and "saveWebTools" in _dash
      and "本地 web 工具" in _dash)

finish()
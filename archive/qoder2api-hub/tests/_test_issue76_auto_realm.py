"""issue #76 回归套件：未指定出口的请求必须让 None 一路传下去（自动换区入口）。

背景：三个请求入口（chat /v1/chat/completions、responses /v1/responses、
anthropic /v1/messages）此前写成 `self._request_realm() or CURRENT_REALM`，
把「用户没指定出口」折叠成「当前区」，于是区域独占模型（INTL_EXCLUSIVE /
CN_EXCLUSIVE）在 _model_precheck 处被判跨区直接 400，open_upstream 里
`if not target_realm:` 的自动路由分支（detect_model_realm +
fallback_realm_by_accounts）永远进不去——与 _request_realm docstring
（"Returning None lets open_upstream() fall back to model-based detection."）
及文档「?realm= / Key 绑定 / X-Realm 永远优先，自动只在用户没指定时兜底」相反。

本套件两层守护（装置照 _test_model_gates 接线层：真 handler + 拦两层）：

1) 源码守卫（按函数归属，防回归也防过删）：
   - 三个入口函数体内不得再出现 `self._request_realm() or CURRENT_REALM`，
     且必须存在裸调用行；
   - do_GET 的两个模型列表端点**必须保留** `or CURRENT_REALM`
     （列表必须有确定区域，去掉会改变 /v1/models 语义）。
2) 行为断言（open_upstream 桩记录收到的 target_realm）：
   - 自动 Key（未绑区）+ intl 独占模型，无显式出口 → 不被 400 拦下，
     且 target_realm 原样为 None（自动路由的前提）；chat / responses /
     anthropic 三入口各自验证；
   - 显式出口优先级不回退：X-Realm / ?realm= / Key 绑区仍优先生效
     （显式钉 cn 打独占模型 → 仍 400 且不触上游；显式 intl / 绑区
     cn → 语义不变）。

金丝雀自证：把任一行改回 `... or CURRENT_REALM` → 守卫与对应行为断言必红；
套件不依赖桩的返回值语义，改回后 _model_precheck 会先 400，行为断言同样红。

    python tests/_test_issue76_auto_realm.py
"""
import os
import re
import shutil
import sys
import tempfile
import threading
import types

import http.client
import http.server
import json as _json

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from _suite_head import bootstrap, check, declare_suite, finish  # noqa: E402

P, A = bootstrap()  # noqa: F401  (P 在下方接线层使用)
declare_suite(__file__, ["qoder_proxy.py"])

SRC = open(os.path.join(os.path.dirname(HERE), "qoder_proxy.py"),
           encoding="utf-8").read()

# ---------------- 1) 源码守卫：按函数归属 ----------------
BUG = "req_realm = self._request_realm() or CURRENT_REALM"
PLAIN = "req_realm = self._request_realm()"
ENTRIES = ("do_POST", "_handle_responses", "_handle_anthropic")


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


print("\n[issue76] 源码守卫（三个入口 + 模型列表端点）")
for fn in ENTRIES:
    body = lines_of_func(fn)
    check("%s：无『_request_realm() or CURRENT_REALM』" % fn,
          not any(l.strip().startswith(BUG) for l in body),
          [i for i, l in enumerate(body) if l.strip().startswith(BUG)][:3])
    check("%s：裸调用行存在（None 得以传下去）" % fn,
          any(l.strip().startswith(PLAIN) and " or " not in l
              for l in body))
get_body = lines_of_func("do_GET")
n_get = sum(1 for l in get_body if l.strip().startswith(BUG))
check("do_GET：模型列表两处**保留** CURRENT_REALM（防过删，列表必须有确定区）",
      n_get == 2, n_get)

# ---------------- 2) 行为断言：真 handler + 桩 ----------------
MODEL = None
for cand in ("efficient", "performance", "cmodel"):
    if P.exclusive_realm(cand) == "intl":
        MODEL = cand
        break
check("前置：%r 当前仍是 intl 独占（若上游目录变了请换 exclusive 样例）"
      % (MODEL or ""), MODEL is not None and P.CURRENT_REALM == "cn",
      {"model": MODEL, "CURRENT_REALM": P.CURRENT_REALM})
if MODEL is None or P.CURRENT_REALM != "cn":
    finish()

D = tempfile.mkdtemp(prefix="qd-issue76-")
_orig_accounts = P.ACCOUNTS_DIR
_orig_open = P.open_upstream
_orig_urllib = P.urllib
seen = {"open": [], "net": 0}


def _fake_open_upstream(payload, target_realm=None,
                        usage_ctx=None, **kw):
    # 记录入口传下来的 target_realm（None = 未指定，应触发自动路由），
    # 并按 #26 契约回填实际出口，然后掐断——绝不触网。
    seen["open"].append(target_realm)
    if usage_ctx is not None:
        usage_ctx["realm"] = target_realm or P.detect_model_realm(
            str(payload.get("model") or "")) or P.CURRENT_REALM
    raise RuntimeError("stub: 出口路由层已到达（不触网）")


def _fake_urlopen(*a, **k):
    seen["net"] += 1
    raise RuntimeError("stub: network layer must not be reached")


P.ACCOUNTS_DIR = D
import qoder_settings as S  # noqa: E402
S.set_api_keys(D, [
    {"id": "auto76", "name": "自动出口Key", "key": "auto-key-76"},
    {"id": "bound76", "name": "绑区国内Key", "key": "bound-key-76",
     "realm": "cn"},
])
P.open_upstream = _fake_open_upstream
P.urllib = types.SimpleNamespace(
    request=types.SimpleNamespace(Request=_orig_urllib.request.Request,
                                  urlopen=_fake_urlopen),
    error=_orig_urllib.error,
    parse=_orig_urllib.parse)
srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), P.Handler)
threading.Thread(target=srv.serve_forever, daemon=True).start()
_PORT = srv.server_address[1]


def _post(path, body, key="auto-key-76"):
    conn = http.client.HTTPConnection("127.0.0.1", _PORT, timeout=20)
    headers = {"Content-Type": "application/json"}
    if key:
        headers["Authorization"] = "Bearer " + key
    try:
        conn.request("POST", path, body=_json.dumps(body).encode("utf-8"),
                     headers=headers)
        resp = conn.getresponse()
        return int(resp.status), resp.read().decode("utf-8", "replace"), \
            dict(resp.getheaders())
    finally:
        conn.close()


def _reset():
    seen["open"] = []
    seen["net"] = 0


def _route(endpoint, extra_headers=None, key="auto-key-76", query=""):
    """发一次独占模型请求，返回 (status, open 收到的 target_realm 列表)。"""
    _reset()
    if endpoint == "chat":
        path, body = "/v1/chat/completions" + query, \
            {"model": MODEL, "messages": [{"role": "user", "content": "hi"}]}
    elif endpoint == "responses":
        path, body = "/v1/responses" + query, {"model": MODEL, "input": "hi"}
    else:
        path, body = "/v1/messages" + query, \
            {"model": MODEL, "max_tokens": 16,
             "messages": [{"role": "user", "content": "hi"}]}
    conn_hdr = {}
    if extra_headers:
        conn_hdr.update(extra_headers)
    conn = http.client.HTTPConnection("127.0.0.1", _PORT, timeout=20)
    headers = {"Content-Type": "application/json",
               "Authorization": "Be" + "arer " + key}
    headers.update(conn_hdr)
    try:
        conn.request("POST", path, body=_json.dumps(body).encode("utf-8"),
                     headers=headers)
        resp = conn.getresponse()
        resp.read()
        st = int(resp.status)
    finally:
        conn.close()
    return st, list(seen["open"])


print("\n[issue76] 行为断言（未指定出口 → None 传到路由层）")
for ep, label in (("chat", "chat"), ("responses", "responses"),
                  ("anthropic", "anthropic messages")):
    st, realms = _route(ep)
    check("%s：独占模型未被跨区 400（修复前必 400）" % label,
          st != 400, (st, realms))
    check("%s：open_upstream 收到 target_realm=None（自动路由的前提）" % label,
          realms == [None], realms)
    check("%s：网络层计数 0（桩在路由层即拦截）" % label, seen["net"] == 0,
          seen["net"])

print("\n[issue76] 显式出口优先级不回退")
st, realms = _route("chat", extra_headers={"X-Realm": "cn"})
check("X-Realm: cn + intl 独占 → 仍按显式绑定 400，不触上游",
      st == 400 and realms == [], (st, realms))
st, realms = _route("chat", extra_headers={"X-Realm": "intl"})
check("X-Realm: intl → 显式出口原样下传", st != 400 and realms == ["intl"],
      (st, realms))
st, realms = _route("chat", query="?realm=intl")
check("?realm=intl → 显式出口原样下传", st != 400 and realms == ["intl"],
      (st, realms))
st, realms = _route("chat", key="bound-key-76")
check("Key 绑区 cn + intl 独占 → 仍 400（绑定优先于自动，语义不变）",
      st == 400 and realms == [], (st, realms))

P.ACCOUNTS_DIR = _orig_accounts
P.open_upstream = _orig_open
P.urllib = _orig_urllib
srv.shutdown()
shutil.rmtree(D, ignore_errors=True)

finish()

# -*- coding: utf-8 -*-
"""i18n 路由套件：清单发现、免鉴权、以及**路径穿越**（起真实 handler）。

    python tests/_test_i18n_route.py

为什么值得单独一套：/i18n/* 落在 _is_panel_route 的 startswith 链之外，所以它是
**未鉴权**的（这是必须的——登录遮罩本身就要能翻译）。代价是请求者可控的文件名
直接抵到文件系统前面。这里钉的四件事：

  1. 丢一个 json 进目录就出现在清单里，且不重启（「即插即用」的契约本身）；
  2. 穿越载荷（.. / %2e / 绝对路径 / NUL / 非 .json）一律 404，且不回显文件内容；
  3. 未鉴权只放行 /i18n，/accounts 仍然要面板密码；
  4. 坏包被整包跳过而不是把面板带崩。

I18N_DIR 被改指到临时目录：往仓里的 i18n/ 写测试文件会被
scripts/check_clean_checkout.py 判成散落的脏文件。
"""
import http.client
import json
import os
import shutil
import socket
import sys
import tempfile
import threading
import http.server
import urllib.parse

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)
os.environ["ACCOUNTS_DIR"] = tempfile.mkdtemp(prefix="qd-i18n-")
os.environ["QD_PROXY_USAGE_DIR"] = os.path.join(
    os.environ["ACCOUNTS_DIR"], "usage")
import qoder_proxy as P        # noqa: E402

PASS = FAIL = 0


def check(label, cond, extra=None):
    global PASS, FAIL
    if cond:
        PASS += 1
        print("  [PASS] " + label)
    else:
        FAIL += 1
        print("  [FAIL] %s  %r" % (label, extra))


I18N_TMP = os.path.join(os.environ["ACCOUNTS_DIR"], "i18n")
os.makedirs(I18N_TMP)
P.I18N_DIR = I18N_TMP


def write_pack(name, obj=None, raw=None):
    path = os.path.join(I18N_TMP, name)
    with open(path, "wb") as fh:
        fh.write(raw if raw is not None
                 else json.dumps(obj, ensure_ascii=False).encode("utf-8"))
    return path


write_pack("en.json", {
    "@locale": {"code": "en", "name": "English", "order": 10},
    "messages": {"设置": "Settings"},
})

srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), P.Handler)
threading.Thread(target=srv.serve_forever, daemon=True).start()
PORT = srv.server_address[1]


def raw_get(target):
    """http.client 而非 urllib：urllib 会替我们规范化 dot-segment，
    那样就测不到 handler 自己怎么处理 `..` 了。"""
    c = http.client.HTTPConnection("127.0.0.1", PORT, timeout=5)
    c.request("GET", target)
    r = c.getresponse()
    body = r.read()
    c.close()
    return r.status, r.getheader("Content-Type") or "", body


def get(target):
    return raw_get(target)


def socket_get(target):
    """裸 socket：http.client 会拒绝含控制字符的 target，而服务端**确实**能收到
    字面 NUL —— 那是一条和 %00 不同的代码路径（urlparse 看到的是真字节）。"""
    s = socket.create_connection(("127.0.0.1", PORT), timeout=5)
    s.sendall(("GET %s HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"
               % target).encode("latin-1"))
    chunks = []
    while True:
        b = s.recv(4096)
        if not b:
            break
        chunks.append(b)
    s.close()
    blob = b"".join(chunks)
    head, _, body = blob.partition(b"\r\n\r\n")
    status = int(head.split(b" ")[1]) if len(head.split(b" ")) > 1 else 0
    return status, "", body


print("[i18n] 清单发现（即插即用的契约）")
st, ct, body = get("/i18n/manifest")
check("GET /i18n/manifest 免面板密码即 200", st == 200, (st, body[:120]))
man = json.loads(body.decode("utf-8")) if st == 200 else {}
codes = [p.get("code") for p in man.get("packs", [])]
check("清单里有 en", codes == ["en"], codes)
check("清单带出 @locale.name", man["packs"][0].get("name") == "English")
check("清单声明源语言为 zh 默认", man.get("default") == "zh")

write_pack("id.json", {
    "@locale": {"code": "id", "name": "Bahasa Indonesia", "order": 20},
    "messages": {"设置": "Pengaturan"},
})
st, ct, body = get("/i18n/manifest")
codes = [p.get("code") for p in json.loads(body.decode("utf-8"))["packs"]]
check("新增一个 json 文件后清单立刻出现 id（**不重启**）",
      st == 200 and codes == ["en", "id"], codes)
check("清单按 @locale.order 升序", codes == ["en", "id"])

st, ct, body = get("/i18n/id.json")
check("GET /i18n/<code>.json 返回包体本身",
      st == 200 and json.loads(body.decode("utf-8"))["messages"]["设置"] == "Pengaturan")
check("包体 Content-Type 是 application/json; charset=utf-8",
      "application/json" in ct and "utf-8" in ct, ct)

print("[i18n] 文件名是 code 的权威")
write_pack("de.json", {
    "@locale": {"code": "en", "name": "Deutsch"},   # 故意谎报 code
    "messages": {"设置": "Einstellungen"},
})
st, ct, body = get("/i18n/manifest")
packs = {p["code"]: p for p in json.loads(body.decode("utf-8"))["packs"]}
check("谎报 @locale.code 不影响清单键（仍以文件名为准）",
      "de" in packs and "en" in packs and packs["de"]["name"] == "Deutsch",
      sorted(packs))
os.remove(os.path.join(I18N_TMP, "de.json"))

print("[i18n] 路径穿越（本套件的核心风险）")
TRAVERSALS = [
    "/i18n/../qoder_proxy.py",
    "/i18n/..%2Fqoder_proxy.py",
    "/i18n/..%2faccounts%2Fsettings.json",
    "/i18n/%2e%2e%2fqoder_proxy.py",
    "/i18n/....//qoder_proxy.py",
    "/i18n/..%5c..%5cqoder_proxy.py",
    "/i18n/%00.json",
    "/i18n/en.json.json",
    "/i18n/.hidden.json",
    "/i18n/..json",
    "/i18n/C:%5CWindows%5Cwin.ini",
    "/i18n/%2a.json",
    "/i18n/en.py",
    "/i18n/en.txt",
    "/i18n/README.md",
    "/i18n/%E3%82%81.json",
]
LEAK_MARKERS = (b"def ", b"import ", b"ACCOUNTS_DIR", b"panel_password",
                b"-----BEGIN", b"accessToken")
for target in TRAVERSALS:
    try:
        st, ct, body = get(target)
    except Exception as exc:
        check("穿越载荷被安全处理: %r" % target[:44], False, repr(exc))
        continue
    leaked = any(m in body for m in LEAK_MARKERS)
    check("非白名单名一律不 200 且不外泄文件内容: %r" % target[:44],
          st != 200 and not leaked, (st, len(body), leaked))

for target in ("/i18n/en.json\x00.png", "/i18n/../qoder_proxy.py"):
    try:
        st, ct, body = socket_get(target)
    except Exception as exc:
        check("裸 socket 送字面 NUL / dot-segment 不崩: %r" % target[:34],
              False, repr(exc))
        continue
    check("裸 socket 载荷不 200 且不外泄: %r" % target[:34],
          st != 200 and b"def " not in body, (st, body[:120]))

print("[i18n] 免鉴权边界：只放行 /i18n")
st, ct, body = get("/accounts")
check("/accounts 仍然要面板密码（401），/i18n 的公开没有连带扩大攻击面",
      st == 401, (st, body[:120]))
st, ct, body = get("/i18n/manifest")
check("/i18n/manifest 在无面板会话时可读", st == 200, st)

print("[i18n] 坏包：整包跳过，不把面板带崩")
write_pack("broken.json", raw=b"{ this is not json")
write_pack("tierbad.json", {
    "@locale": {"code": "tierbad", "name": "Broken tier"},
    "messages": ["not", "an", "object"],
})
write_pack("valbad.json", {
    "@locale": {"code": "valbad", "name": "Broken value"},
    "messages": {"设置": {"nested": "not a string"}},
})
write_pack("arr.json", raw=b'[1,2,3]')
st, ct, body = get("/i18n/manifest")
codes = [p.get("code") for p in json.loads(body.decode("utf-8"))["packs"]]
check("非法 JSON / 层级类型错 / 顶层非对象 都被跳过，清单仍 200",
      st == 200 and sorted(codes) == ["en", "id"], (st, codes))
st, ct, body = get("/i18n/broken.json")
check("直接取坏包不返回原文（404），错误文本不进响应体",
      st == 404 and b"this is not json" not in body, (st, body[:120]))

print("[i18n] 目录缺失是正常状态，不是 500")
shutil.rmtree(I18N_TMP)
st, ct, body = get("/i18n/manifest")
check("i18n/ 不存在时清单返回空数组且 200",
      st == 200 and json.loads(body.decode("utf-8"))["packs"] == [], (st, body[:120]))
st, ct, body = get("/i18n/en.json")
check("i18n/ 不存在时取包 404 而不是 500", st == 404, st)

srv.shutdown()
shutil.rmtree(os.environ["ACCOUNTS_DIR"], ignore_errors=True)

print("")
print("SUMMARY: TOTAL %d checks, %d passed, %d failed" % (PASS + FAIL, PASS, FAIL))
print("RESULT: %s (exit %d)" % ("RED" if FAIL else "GREEN", 1 if FAIL else 0))
sys.exit(1 if FAIL else 0)

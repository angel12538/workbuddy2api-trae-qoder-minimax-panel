"""issue #81 回归套件：/agents/models 的模型清单不得恒为空。

背景：面板「智能体配置」页的默认模型下拉请求 `/agents/models`，后端是
`gateway_models_for_agents(realm)`。它按 dict 解析 `fetch_models_merged()` 的返回值，
而后者返回 `[(mid, meta, realm)]` 元组 → `isinstance(e, dict)` 全部判负，16 条全被跳过；
带 realm 时还会调用 `fetch_models_merged(realm=...)`（该函数不接受参数）→ TypeError
被 `except Exception` 吞掉。两条路径的结果都是 `{"models": []}`，前端又不提示，
下拉只剩占位项。

本套件三层守护：

1) 行为断言（真实代码，不 mock）：无 realm / realm=cn / realm=intl 三种调用都必须非空，
   且 id 集合与 `/v1/models` 同源（`model_entry(...)["id"]`）一致；
   上下文窗口、最大输出与 `model_entry` 的 context_length / max_output_tokens 一致。
2) 源码守卫：`fetch_models_merged(` 不得带参数调用（它没有参数）。
3) 条目形态：每项 id 为非空字符串，且不重复。

金丝雀自证：把 `gateway_models_for_agents` 还原成 v1.6.5 的实现，行为断言必红。

    python tests/_test_issue81_agents_models.py
"""
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from _suite_head import bootstrap, check, declare_suite, finish  # noqa: E402

P, A = bootstrap()  # noqa: F401
declare_suite(__file__, ["qoder_proxy.py"])

SRC = open(os.path.join(os.path.dirname(HERE), "qoder_proxy.py"),
           encoding="utf-8").read()


def _expected_ids(realm):
    """与 /v1/models 同源：fetch_models* → model_entry → id。"""
    if realm:
        rows = [(mid, meta, realm) for mid, meta in P.fetch_models(realm=realm)]
    else:
        rows = P.fetch_models_merged()
    return [P.model_entry(mid, meta, realm=r)["id"] for mid, meta, r in rows]


def _ids(result):
    return [m["id"] for m in result.get("models", [])]


print("\n[issue81] 行为：三种调用都必须非空，且 id 与 /v1/models 同源")
for label, realm in (("无 realm（两区并集）", None), ("realm=cn", "cn"),
                     ("realm=intl", "intl")):
    expect = _expected_ids(realm)
    got = P.gateway_models_for_agents(realm)
    ids = _ids(got)
    check("%s：同源清单本身非空（前置）" % label, len(expect) > 0, len(expect))
    check("%s：返回非空" % label, len(ids) > 0, got)
    check("%s：id 集合与 /v1/models 一致" % label,
          set(ids) == set(expect), sorted(set(expect) ^ set(ids))[:5])
    check("%s：id 不重复" % label, len(ids) == len(set(ids)), ids)
    check("%s：每项 id 为非空字符串" % label,
          all(isinstance(i, str) and i.strip() for i in ids), ids[:3])

print("\n[issue81] 行为：上下文窗口 / 最大输出与 model_entry 一致")
rows = P.fetch_models_merged()
want = {}
for mid, meta, r in rows:
    e = P.model_entry(mid, meta, realm=r)
    want[e["id"]] = (e.get("context_length"), e.get("max_output_tokens"))
got = {m["id"]: m for m in P.gateway_models_for_agents(None).get("models", [])}
with_ctx = [i for i, (c, _o) in want.items() if c and i in got]
with_out = [i for i, (_c, o) in want.items() if o and i in got]
check("至少有一个模型带上下文窗口（前置，否则本组断言无意义）",
      len(with_ctx) > 0, len(with_ctx))
check("context_window 与 context_length 一致",
      all(got[i].get("context_window") == int(want[i][0]) for i in with_ctx),
      [(i, got[i].get("context_window"), want[i][0]) for i in with_ctx[:3]])
check("max_output 与 max_output_tokens 一致",
      all(got[i].get("max_output") == int(want[i][1]) for i in with_out),
      [(i, got[i].get("max_output"), want[i][1]) for i in with_out[:3]])

print("\n[issue81] 源码守卫：fetch_models_merged 无参数，不得带参数调用")
bad = [m.group(0) for m in re.finditer(r"fetch_models_merged\([^)\s][^)]*\)", SRC)]
check("没有 fetch_models_merged(<实参>) 的调用", not bad, bad)
check("fetch_models_merged 仍为无参定义",
      re.search(r"^def fetch_models_merged\(\):", SRC, re.M) is not None)

print("\n[issue81] 金丝雀说明：还原为 v1.6.5 实现（按 dict 解析 + 传 realm=）时，"
      "以上行为断言与源码守卫都会变红。")
finish()

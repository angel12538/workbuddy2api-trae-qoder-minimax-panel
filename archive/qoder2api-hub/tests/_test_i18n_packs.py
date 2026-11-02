# -*- coding: utf-8 -*-
"""i18n 包体检套件：校验仓里**真实**的 i18n/*.json。

    python tests/_test_i18n_packs.py

路由套件用临时目录测机制，这里守内容。最有价值的一条是第 4 组：
每个 messages 键都必须在 dashboard.html 里真的出现过。键是从源码逐字抄来的，
抄错一个字就永远命不中，而且**静默**（兜底返回原文，看起来只是「这条还没翻」）。
没有这条断言，一个错键只能靠人肉在界面上找。
"""
import io
import json
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

ROOT_DIR = ROOT
I18N_DIR = os.path.join(ROOT_DIR, "i18n")
DASH = os.path.join(ROOT_DIR, "dashboard.html")

declare_suite(__file__, ["dashboard.html"] +
              ["i18n/" + n for n in sorted(
                  f for f in (os.listdir(I18N_DIR) if os.path.isdir(I18N_DIR) else [])
                  if f.endswith(".json"))])

dash = io.open(DASH, encoding="utf-8").read()
NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{1,31}\.json$")
SLOT = re.compile(r"\{(\d+)\}")


def squash(s):
    """与 dashboard.html 里的同名函数同一套折叠规则（源码是真值）。"""
    return re.sub(r"\s+", " ", s).strip()


dash_sq = squash(dash)

packs = sorted(n for n in (os.listdir(I18N_DIR) if os.path.isdir(I18N_DIR) else [])
               if n.endswith(".json"))

print("[packs] 目录里有包可测")
check("i18n/ 至少有一个 locale 包", len(packs) >= 1, packs)

for name in packs:
    code = name[:-len(".json")]
    print("[packs] %s" % name)
    raw = io.open(os.path.join(I18N_DIR, name), encoding="utf-8").read()
    try:
        data = json.loads(raw)
    except Exception as exc:
        check("%s 是合法 JSON" % name, False, repr(exc))
        continue
    check("%s 顶层是对象" % name, isinstance(data, dict))

    loc = data.get("@locale")
    check("%s 有 @locale 头" % name, isinstance(loc, dict))
    loc = loc if isinstance(loc, dict) else {}
    check("%s 的 @locale.code 与文件名一致（不一致会被文件名覆盖，写错只会误导维护者）"
          % name, loc.get("code") == code, (loc.get("code"), code))
    check("%s 有非空 @locale.name（下拉里显示的就是它）"
          % name, isinstance(loc.get("name"), str) and bool(loc.get("name").strip()))
    check("%s 的 dir 是 ltr/rtl" % name, loc.get("dir") in ("ltr", "rtl"), loc.get("dir"))
    check("%s 的 order 是可排序的数字" % name,
          isinstance(loc.get("order"), int) and not isinstance(loc.get("order"), bool),
          loc.get("order"))
    # 前端把包缓存在 localStorage 里，只认这个号；不改 version 的编辑对
    # 已经访问过的人是看不见的（见 docs/i18n.md §8）。
    check("%s 有整数 @locale.version（改了包不抬版本号，老访问者会一直用缓存里的旧包）"
          % name,
          isinstance(loc.get("version"), int) and not isinstance(loc.get("version"), bool),
          loc.get("version"))

    tiers = {}
    for tier in ("messages", "patterns", "fragments"):
        value = data.get(tier, {})
        ok = isinstance(value, dict) and all(
            isinstance(v, str) for v in (value.values() if isinstance(value, dict) else []))
        check("%s 的 %s 是 str->str 对象" % (name, tier), ok)
        tiers[tier] = value if isinstance(value, dict) else {}

    msgs = tiers["messages"]
    check("%s 的 messages 非空（只写 patterns 的包没有可用界面）" % name, len(msgs) > 0)

    print("[packs] %s · 键的真实性与自洽" % name)
    noop = [k for k, v in msgs.items() if k.strip() == v.strip()]
    check("%s 没有「原样映射」的死条目（翻了等于没翻）" % name, not noop, noop[:3])
    # 键必须能被 lookup() 真的命中。core 依次试 原文 / trim / 折叠空白，
    # 所以「在源码里」有两种形态：逐字出现，或折叠空白后出现在折叠过的源码里。
    # 第二种放宽只吃掉换行+缩进这一类抄写差异，抄错字仍然会被抓到。
    absent = [k for k in msgs
              if k not in dash and squash(k) not in dash_sq]
    check("%s 的每个 messages 键都能在 dashboard.html 里命中（抄错的键会静默失效）"
          % name, not absent, absent[:5])
    overlap = set(msgs) & set(tiers["fragments"])
    check("%s 的 messages 与 fragments 无重复键（重复时 fragments 永远不生效，是死配置）"
          % name, not overlap, sorted(overlap)[:5])

    # 观察器终止性靠 t(t(x)) === t(x)。只要译文本身又是一个键，一条文本就会被
    # 连着改两次、结果取决于访问顺序 —— 中文→中文的「润色包」最容易踩到。
    all_keys = set(msgs) | set(tiers["fragments"])
    chained = [k for k, v in msgs.items() if v in all_keys]
    chained += [k for k, v in tiers["fragments"].items() if v in all_keys]
    check("%s 的译文不会恰好又是另一个键（否则会二次改写，观察器不再收敛）"
          % name, not chained, sorted(set(chained))[:3])

    bad_slots = []
    unanchored = []
    for k, v in tiers["patterns"].items():
        if sorted(set(SLOT.findall(k))) != sorted(set(SLOT.findall(v))):
            bad_slots.append(k)
        if not k.strip():
            unanchored.append(k)
        # 槽位之外一个字都不剩的源（'{0}'）编译成 ^(.+)$：任何中文都归它。
        # 核现在会递归翻槽位内容，这种包会让整句原地绕圈（靠深度上限才不炸），
        # 所以它是内容错误，不该靠引擎兜住。
        if not SLOT.sub("", k).strip():
            unanchored.append(k)
    check("%s 的每个 pattern 两侧槽位集合一致（多出的 {n} 会渲染成空）"
          % name, not bad_slots, bad_slots[:5])
    check("%s 没有空/吞整串的 pattern 源（^…$ 会吃掉所有文本）" % name, not unanchored, unanchored)

    # pattern 只有在它的字面片段真存在于源码时才可能命中：槽位把源串切开，
    # 剩下够长的每一段都得能在 dashboard.html 里找到，否则这条永远命不中。
    dead_pat = []
    for k in tiers["patterns"]:
        parts = [p.strip() for p in re.split(r"\{\d+\}", k) if len(p.strip()) >= 4]
        if parts and not any(p in dash for p in parts):
            dead_pat.append(k)
    check("%s 的 pattern 片段能在源码里找到（找不到说明这条永远不会命中）"
          % name, not dead_pat, dead_pat[:3])

print("[packs] 语言码唯一")
codes = [n[:-len(".json")] for n in packs if NAME_RE.match(n)]
check("文件名语言码不重复", len(codes) == len(set(codes)), codes)
check("没有多余的非法文件名混在 i18n/ 里",
      all(NAME_RE.match(n) for n in packs), [n for n in packs if not NAME_RE.match(n)])

finish()

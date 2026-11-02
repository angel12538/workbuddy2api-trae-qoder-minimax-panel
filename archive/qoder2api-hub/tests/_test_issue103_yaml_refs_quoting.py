"""#103 DSH credentials refs 写入未加引号：纯数字 Key 被 YAML 解析成整数。

    python tests/_test_issue103_yaml_refs_quoting.py

背景：面板「一键配置」把网关 Key 写进 DSH 的 `.credentials.yaml` 时是裸拼
（`"  %s: %s" % (key, value)`），没做标量引用。YAML 1.2 core schema 会把
`13638719007` 这类纯数字解析成整数，而 `dsh-credentials-local` 的
`parseRefs()` 硬校验 refs 值必须是 string —— 一个纯数字 Key 就让
`credentials` 插件不激活，`connection` / `authorization` 与全部 tauri、mcp
插件级联 waiting，dsh 完全无法启动。
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_agents.py"])
import qoder_agents as G          # noqa: E402

KEY = "QODER_PROXY_API_KEY"
WITH_REFS = "version: 1\nrefs:\n  OTHER_KEY: sk-abc\n"
EMPTY_REFS = "version: 1\nrefs:\n"
NO_REFS = "version: 1\n"


def line_of(text, key):
    """refs 块里该键所在的行（找不到返回 None）。"""
    for line in text.split("\n"):
        if line.strip().startswith(key + ":"):
            return line
    return None


def quoted(line):
    """值是否是一个双引号标量。"""
    if not line:
        return False
    value = line.partition(":")[2].strip()
    return len(value) >= 2 and value.startswith('"') and value.endswith('"')


def key_lines(text, key):
    return [ln for ln in text.split("\n") if ln.strip().startswith(key + ":")]


# 会被 YAML 1.2 core schema 解析成非字符串的裸值 + 一个对照用的普通字符串。
CASES = [
    ("13638719007", "纯数字（手机号形式）"),
    ("0", "单个零"),
    ("007", "前导零"),
    ("-1", "负数"),
    ("1.5", "浮点"),
    ("1e5", "科学计数法"),
    ("0x1f", "十六进制"),
    ("true", "布尔真"),
    ("false", "布尔假"),
    ("null", "null"),
    ("~", "波浪号 null"),
    ("sk-normal-key", "普通字符串（对照）"),
]

# --- 分支一：refs 块已存在、键已存在 → 覆盖原行
for value, label in CASES:
    out = G.yaml_refs_upsert(WITH_REFS + "  %s: 旧值\n" % KEY, KEY, value)
    check("#103 已有同名键时 %s 覆盖后带引号" % label,
          quoted(line_of(out, KEY)) and len(key_lines(out, KEY)) == 1,
          line_of(out, KEY))

# --- 分支二：refs 块已存在、键不存在 → 插入新行
for value, label in CASES:
    out = G.yaml_refs_upsert(WITH_REFS, KEY, value)
    check("#103 已有 refs 块时 %s 插入后带引号" % label,
          quoted(line_of(out, KEY)), line_of(out, KEY))

# --- 分支三：空 refs 块 / 完全没有 refs 块 → 追加
for value, label in CASES:
    out = G.yaml_refs_upsert(EMPTY_REFS, KEY, value)
    check("#103 空 refs 块时 %s 带引号" % label,
          quoted(line_of(out, KEY)), line_of(out, KEY))
    out = G.yaml_refs_upsert(NO_REFS, KEY, value)
    check("#103 无 refs 块时 %s 带引号" % label,
          quoted(line_of(out, KEY)), line_of(out, KEY))

# --- 不破坏相邻键
out = G.yaml_refs_upsert(WITH_REFS, KEY, "13638719007")
check("#103 相邻键保持原样", line_of(out, "OTHER_KEY") == "  OTHER_KEY: sk-abc",
      line_of(out, "OTHER_KEY"))
check("#103 相邻键仍是裸字符串（不该被引号化）",
      not quoted(line_of(out, "OTHER_KEY")), line_of(out, "OTHER_KEY"))

# --- 转义：值里带引号 / 冒号 / 井号 / 换行 / 反斜杠，都不能把 YAML 写坏
for raw in ('a"b', "a: b", "a #b", "a\nb", "a\\b", 'a"b\\c'):
    out = G.yaml_refs_upsert(WITH_REFS, KEY, raw)
    check("#103 值含特殊字符 %r 时仍是单个带引号标量" % raw,
          quoted(line_of(out, KEY)) and len(key_lines(out, KEY)) == 1,
          line_of(out, KEY))

# --- 真实反序列化（可选）：装了 PyYAML 才做，否则跳过
try:
    import yaml as _yaml
except ImportError:
    skip("#103 PyYAML 未安装，跳过真实反序列化校验")
else:
    for value, label in CASES:
        doc = _yaml.safe_load(G.yaml_refs_upsert(WITH_REFS, KEY, value)) or {}
        got = (doc.get("refs") or {}).get(KEY)
        check("#103 %s 反序列化回来仍是 str 且值不变" % label,
              isinstance(got, str) and got == value, repr(got))

    for raw in ('a"b', "a: b", "a #b", "a\nb", "a\\b"):
        doc = _yaml.safe_load(G.yaml_refs_upsert(WITH_REFS, KEY, raw)) or {}
        got = (doc.get("refs") or {}).get(KEY)
        check("#103 特殊字符 %r 反序列化后值不变" % raw, got == raw, repr(got))

    check("#103 反序列化后相邻键仍在",
          ((_yaml.safe_load(G.yaml_refs_upsert(WITH_REFS, KEY, "1")) or {})
           .get("refs") or {}).get("OTHER_KEY") == "sk-abc")

finish()

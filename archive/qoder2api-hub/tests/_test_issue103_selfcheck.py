"""#103 补充：DSH credentials 写后自检（防止再写出裸标量）。"""
import io
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_agents.py"])
import qoder_agents as G          # noqa: E402

_src = io.open(os.path.join(ROOT, "qoder_agents.py"), encoding="utf-8").read()

print("[#103+] 自检函数")
check("有 refs_value_is_scalar_string",
      "def refs_value_is_scalar_string(text, key):" in _src)

_ok = G.yaml_refs_upsert("version: 1\nrefs:\n  OTHER: sk-1\n", "K", "13800138000")
check("写入后是带引号的标量", G.refs_value_is_scalar_string(_ok, "K") is True,
      [ln for ln in _ok.split("\n") if ln.strip().startswith("K:")])
check("裸值会被判定为不合格",
      G.refs_value_is_scalar_string("version: 1\nrefs:\n  K: 13800138000\n", "K")
      is False)
check("缺键也算不合格",
      G.refs_value_is_scalar_string("version: 1\nrefs:\n", "K") is False)
check("单引号也不算（我们统一写双引号）",
      G.refs_value_is_scalar_string("refs:\n  K: '13800138000'\n", "K") is False)

print("[#103+] 写入路径接了自检")
check("_plan_dsh 写完后自检并在失败时抛错",
      "refs_value_is_scalar_string(cred_text, CREDENTIAL_REF)" in _src
      and "raise ValueError(" in _src)
check("错误信息提示了根因（YAML 会解析成非字符串）",
      "YAML 解析成非字符串" in _src)

print("[#103+] 各种会踩坑的 Key 都能安全写入")
for _v, _why in (("13800138000", "纯数字"), ("0", "单个零"), ("007", "前导零"),
                 ("-1", "负数"), ("1e5", "科学计数"), ("true", "布尔字面量"),
                 ("null", "空值字面量"), ("sk-abc", "普通串"),
                 ("a \"b\" c", "含双引号"), ("a\\b", "含反斜杠")):
    _txt = G.yaml_refs_upsert("version: 1\nrefs:\n", "K", _v)
    check("可安全写入：%s（%s）" % (_why, _v),
          G.refs_value_is_scalar_string(_txt, "K") is True)

finish()
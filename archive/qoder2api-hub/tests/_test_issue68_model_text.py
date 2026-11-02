"""#68 官方模型文案：内嵌快照刷新 + 容器/Linux 可覆盖。

    python tests/_test_issue68_model_text.py
"""
import os
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_catalog.py"])
import qoder_catalog as C           # noqa: E402


def _reset():
    C._official_text_cache = {}
    C._OFFICIAL_TEXT_LOGGED["embedded"] = False


# ---- 内嵌快照已刷新：不再写死已下线的版本号
_reset()
_cands = C._official_text_candidates
C._official_text_candidates = lambda: []
try:
    _d = C.official_description("dfmodel")
    _qmodel_desc = C.official_description("qmodel")
    _all = " ".join(C.official_description(k) for k in
                    ("auto", "dmodel", "gmodel", "kmodel", "mmodel", "qmodel",
                     "dfmodel", "gfmodel", "qfmodel", "qmodel_38max",
                     "kmodel_latest", "qmodel_latest"))
finally:
    C._official_text_candidates = _cands
    _reset()
check("#68 内嵌快照不再含已下线版本号（0731/0813）",
      "0731" not in _all and "0813" not in _all)
check("#68 dfmodel 文案改为现行表述（推理速度快且成本低廉）",
      "推理速度快" in _d and "成本" in _d, _d)
check("#68 qmodel 文案与现行文档一致（推理能力、效率与多模态体验）",
      "多模态体验" in _qmodel_desc, _qmodel_desc)

# ---- kmodel label 不再钉在旧代号
_reset()
C._official_text_candidates = lambda: []
try:
    _lab = C.load_official_text()["labels"].get("kmodel")
finally:
    C._official_text_candidates = _cands
    _reset()
check("#68 kmodel label 与上游清单一致（Kimi-K2.8-Preview）",
      _lab == "Kimi-K2.8-Preview", _lab)

# ---- QD_OFFICIAL_TEXT 覆盖（文件）
_tmp = tempfile.mkdtemp()
_p = os.path.join(_tmp, "qoder-cn.json")
with open(_p, "w", encoding="utf-8") as fh:
    json.dump({"zh": {"model": {"dfmodel": {"detail": "OVERRIDE-1",
                                            "label": "OVERRIDE-LABEL"}}}}, fh)
os.environ["QD_OFFICIAL_TEXT"] = _p
_reset()
try:
    _ov = C.official_description("dfmodel")
finally:
    os.environ.pop("QD_OFFICIAL_TEXT", None)
    _reset()
check("#68 QD_OFFICIAL_TEXT 指向文件时优先使用",
      _ov == "OVERRIDE-1", _ov)

# ---- QD_OFFICIAL_TEXT 覆盖（目录）
os.environ["QD_OFFICIAL_TEXT"] = _tmp
_reset()
try:
    _ov2 = C.official_description("dfmodel")
finally:
    os.environ.pop("QD_OFFICIAL_TEXT", None)
    _reset()
check("#68 QD_OFFICIAL_TEXT 指向目录时扫描其中的 *.json",
      _ov2 == "OVERRIDE-1", _ov2)

# ---- 候选路径覆盖容器挂载点 + Linux/macOS
_cand_list = C._official_text_candidates()
_src = open(os.path.join(ROOT, "qoder_catalog.py"), encoding="utf-8").read()
check("#68 候选路径包含容器挂载目录",
      "/app/assets/dynamic-text" in _src and "/data/dynamic-text" in _src)
check("#68 候选路径包含 Linux/macOS 客户端位置",
      ".config/qoder" in _src or '"qoder", "dynamic-text"' in _src)
check("#68 候选去重且为列表", isinstance(_cand_list, list)
      and len(_cand_list) == len(set(_cand_list)))

# ---- 回退可见
_reset()
C._official_text_candidates = lambda: []
_warns = []
_real_warn = C._warn
C._warn = lambda msg: _warns.append(str(msg))
try:
    C.load_official_text()
    C._official_text_cache = {}
    C.load_official_text()
finally:
    C._warn = _real_warn
    C._official_text_candidates = _cands
    _reset()
check("#68 回退内嵌快照时打一条 WARN（只一次）",
      len(_warns) == 1 and "EMBEDDED" in _warns[0], _warns)

finish()

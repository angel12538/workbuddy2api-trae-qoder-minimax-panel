"""#69 账号 / API Key 备注。

    python tests/_test_issue69_notes.py
"""
import io
import json
import os
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_accounts.py", "qoder_settings.py",
                         "qoder_proxy.py", "dashboard.html"])
import qoder_settings as S         # noqa: E402

# ---------------------------------------------------------------- 账号备注
_tmp = tempfile.mkdtemp(prefix="note69-")
_acc = A.Account({"uid": "n1", "realm": "cn", "accessToken": "t",
                  "note": "公司团队号"})
check("#69 账号能读入备注", _acc.note == "公司团队号", _acc.note)
check("#69 to_dict 输出 note", _acc.to_dict().get("note") == "公司团队号")
_acc.save(_tmp)
_back = A.Account(json.load(io.open(os.path.join(_tmp, "n1.json"),
                                   encoding="utf-8")))
check("#69 备注落盘并可读回", _back.note == "公司团队号", _back.note)
_acc.note = "改过的备注"
_acc.save(_tmp)
_back2 = A.Account(json.load(io.open(os.path.join(_tmp, "n1.json"),
                                    encoding="utf-8")))
check("#69 修改备注会覆盖旧值", _back2.note == "改过的备注", _back2.note)
check("#69 老文件（没有 note 字段）不报错且为空串",
      A.Account({"uid": "n2", "realm": "cn", "accessToken": "t"}).note == "")

# ---------------------------------------------------------------- Key 备注
_entry = S._clean_key_entry({"key": "sk-x", "name": "测试", "note": "团队A"})
check("#69 Key 条目保留 note", _entry.get("note") == "团队A", _entry.get("note"))
_long = S._clean_key_entry({"key": "sk-y", "note": "x" * 300})
check("#69 Key note 截断到 120 字符", len(_long.get("note", "")) == 120,
      len(_long.get("note", "")))
_no = S._clean_key_entry({"key": "sk-z"})
check("#69 没有 note 时为空串", _no.get("note") == "")

# ---------------------------------------------------------------- 端点与面板
_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("#69 POST /accounts/note 存在", 'if path == "/accounts/note":' in _src)
_seg = _src.split('if path == "/accounts/note":')[1][:2500]
check("#69 备注端点要求面板会话", "_panel_ok()" in _seg)
check("#69 备注端点保存到磁盘", ".save(ACCOUNTS_DIR)" in _seg)
check("#69 备注长度受限（120）", "[:120]" in _seg)

_dash = io.open(os.path.join(ROOT, "dashboard.html"), encoding="utf-8").read()
check("#69 账号行显示备注", "a.note" in _dash and "editAccountNote" in _dash)
check("#69 账号「备注」按钮存在", "备注</button>" in _dash)
check("#69 Key 编辑表单有备注输入", "editKeyNote_" in _dash)
check("#69 Key 保存带 note", "note: (row.note || '').trim()" in _dash)
check("#69 Key 卡片显示备注", "row.note ?" in _dash)

finish()
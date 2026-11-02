"""#90 企业/团队账号：判定口径、登录字段、面板徽标。"""
import io
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_accounts.py", "qoder_proxy.py", "dashboard.html",
                         "docs/accounts.md"])

print("[#90] 判定口径")
_plain = A.Account({"uid": "u-p", "realm": "cn", "accessToken": "t"})
check("个人号不算团队", _plain.is_team() is False)
check("个人号 org_label 为空", _plain.org_label() == "")

_org = A.Account({"uid": "u-o", "realm": "cn", "accessToken": "t",
                  "organizationId": "org-1234567890",
                  "organizationName": "示例科技"})
check("带 organization_id/name -> 团队", _org.is_team() is True)
check("org_label 优先用名字", _org.org_label() == "示例科技", _org.org_label())

_orgid = A.Account({"uid": "u-o2", "realm": "cn", "accessToken": "t",
                    "organizationId": "org-abcdef1234"})
check("只有组织 ID 也算团队", _orgid.is_team() is True)
check("无名字时 org_label 用 ID 前缀", _orgid.org_label() == "org org-abcd",
      _orgid.org_label())

_ut = A.Account({"uid": "u-t", "realm": "cn", "accessToken": "t",
                 "userType": "enterprise_member"})
check("userType=enterprise 也算", _ut.is_team() is True)

_pool = A.Account({"uid": "u-s", "realm": "cn", "accessToken": "t"})
_pool.credits = {"shared": {"size": 2500, "remain": 952, "available": True}}
check("额度里有共享池也算", _pool.is_team() is True)
_pool2 = A.Account({"uid": "u-s2", "realm": "cn", "accessToken": "t"})
_pool2.credits = {"shared": {"size": -1, "unlimited": True}}
check("无限池（cap=-1）也算", _pool2.is_team() is True)

print("[#90] proxy 侧同源")
check("_is_team_account 委托 Account.is_team",
      "probe = getattr(account, \"is_team\", None)" in
      io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read())
check("proxy 也认 organization_id（兜底路径）",
      "organization_id" in io.open(os.path.join(ROOT, "qoder_proxy.py"),
                                    encoding="utf-8").read())

print("[#90] 面板与接口")
_pub = _org.public()
check("public() 暴露 isTeam", _pub.get("isTeam") is True)
check("public() 暴露 orgLabel", _pub.get("orgLabel") == "示例科技")
_dash = io.open(os.path.join(ROOT, "dashboard.html"), encoding="utf-8").read()
check("面板有企业徽标渲染", "function teamBadge(" in _dash)
check("账号行插入徽标", "teamBadge(a)" in _dash)
check("徽标样式存在", ".team-badge" in _dash)

print("[#90] 登录路径兼容 camelCase")
_src = io.open(os.path.join(ROOT, "qoder_accounts.py"), encoding="utf-8").read()
check("organization_id 兼容 organizationId",
      "ui.get(\"organization_id\") or ui.get(\"organizationId\")" in _src)
check("organization_name 兼容 organizationName",
      "ui.get(\"organization_name\") or ui.get(\"organizationName\")" in _src)

print("[#90] 文档")
_doc = io.open(os.path.join(ROOT, "docs", "accounts.md"), encoding="utf-8").read()
check("文档有接入企业账号小节", "怎么把企业账号接进来" in _doc)
check("文档解释「抓不到」的原因与 PAT 兜底",
      "导入 PAT" in _doc and "强制 SSO" in _doc)
check("文档写清三层判定口径",
      "Account.is_team()" in _doc and "最可靠" in _doc)

finish()
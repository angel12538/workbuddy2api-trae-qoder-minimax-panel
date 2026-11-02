"""账号独立页：导航、功能迁移、搜索过滤。"""
import io
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["dashboard.html"])

_dash = io.open(os.path.join(ROOT, "dashboard.html"), encoding="utf-8").read()


def _page(name):
    """截出某个 .main-page 的 HTML 片段。"""
    start = _dash.index('<div id="page%s" class="main-page">' % name)
    nxt = _dash.find('<div id="page', start + 10)
    return _dash[start: nxt if nxt > 0 else len(_dash)]


print("[accounts] 导航与页签")
check("导航栏有「账号」入口", 'id="btnNavAccounts"' in _dash)
check("入口带人物图标",
      'id="btnNavAccounts"' in _dash
      and _dash.split('id="btnNavAccounts"')[1][:400].count("<svg") == 1)
check("MAIN_TABS 含 accounts",
      "'accounts'" in _dash.split("const MAIN_TABS =")[1][:200])
check("预点亮 TABS 也含 accounts（否则刷新会回落网关页）",
      "'accounts'" in _dash.split("var TABS = [")[1][:200])
check("有独立页面容器 #pageAccounts", 'id="pageAccounts"' in _dash)
check("切到账号页会加载账号/签到/概览",
      "if(tab === 'accounts'){ loadAccounts(); loadGrowthTasks(); refresh(); }" in _dash)

print("[accounts] 功能迁移")
_acct = _page("Accounts")
_gw = _page("Gateway")
check("账号列表在新页", 'id="accounts"' in _acct and 'id="accounts"' not in _gw)
check("工具栏（签到/刷新积分/导入/导出/批量）在新页",
      all(k in _acct for k in ("doCheckin", "fetchCredits", "openPatImport",
                               "exportAccounts", "toggleBulkMenu")))
check("签到与福利中心整体迁移",
      'id="sectionGrowthTasks"' in _acct and 'id="sectionGrowthTasks"' not in _gw)
check("最近请求 + 分页迁移",
      'id="recent"' in _acct and 'id="recentPagination"' in _acct
      and 'id="recent"' not in _gw)
check("区域卡（国际/国内）随账号页",
      'id="tabIntl"' in _acct and 'id="tabCn"' in _acct
      and 'id="tabIntl"' not in _gw)
check("账号相关 KPI（账号池/积分余额）搬到账号页",
      'id="acctCards"' in _acct and 'id="acctCards"' not in _gw)
check("网关页仍保留出口卡与调度条",
      'id="cardGatewayCtrl"' in _gw and 'id="schedulerBar"' in _gw)
check("网关页仍保留模型库表", 'id="modelsTable"' in _gw)

print("[accounts] 搜索与细节")
check("账号页有搜索框", 'id="acctSearch"' in _acct)
check("搜索触发 setAccountSearch",
      'oninput="setAccountSearch(this.value)"' in _acct
      and "function setAccountSearch(" in _dash)
check("过滤口径含昵称/UID/组织/备注",
      "a.organizationName" in _dash and "a.note" in _dash)
check("搜索无结果有独立空状态",
      "acct-empty" in _dash and "没有匹配" in _dash)
check("行入场动画 + reduced-motion 降级",
      "acctRowIn" in _dash and "prefers-reduced-motion" in _dash)
check("工具栏吸顶（滚动时仍可操作）",
      "#pageAccounts .toolbar{position:sticky" in _dash)

print("[accounts] 页面结构")
check("账号页至少 3 个 section（左侧导航才有锚点）",
      _acct.count("<section") >= 3, _acct.count("<section"))
check("每个 section 都有 h2（左侧导航靠它建锚点）",
      _acct.count("<section") <= _acct.count("<h2"),
      (_acct.count("<section"), _acct.count("<h2")))

finish()